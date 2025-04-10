package server

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

var (
	deploymentInsertStmt *sql.Stmt
)

type Deployment struct {
	ID         int64            `json:"id"`
	Head       *Container       `json:"head,omitempty"`
	Containers []*Container     `json:"containers,omitempty"`
	Proxy      *DeploymentProxy `json:"-"`
	URL        string           `json:"url"`
	Port       uint16           `json:"port"`
}

// Creates a deployment row in the database, containting the URL the app should be hosted on (it's public hostname)
// and the port that the web server is listening on
func CreateDeployment(port uint16, appUrl string, db *sql.DB) (*Deployment, error) {
	var deployment Deployment

	err := deploymentInsertStmt.QueryRow(appUrl, port).Scan(&deployment.ID, &deployment.URL, &deployment.Port)
	if err != nil {
		logger.Errorw("Failed to insert deployment", zap.Error(err))
		return nil, err
	}

	return &deployment, nil
}

// Takes an existing deployment, and gracefully upgrades the app to a new image
func (deployment *Deployment) Upgrade(ctx context.Context, projectConfig *pkg.ProjectConfig, imageName string, projectPath string) error {
	existingContainers, err := findExistingDockerContainers(ctx, projectConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to find existing containers: %v", err)
	}

	// we only upgrade the head container, in the future we might want to allow upgrading supplemental containers, but this should work just fine for now.
	container, err := deployment.Head.Upgrade(ctx, imageName, projectPath, projectConfig)
	if err != nil {
		logger.Errorw("Failed to upgrade container", zap.Error(err))
		return err
	}

	// copy(container.ContainerID[:], containerIDString)
	deployment.Head = container
	deployment.Containers = append(deployment.Containers, container)

	logger.Debugw("Starting container", zap.ByteString("container_id", container.ContainerID[:12]))
	err = container.Start(ctx, true)
	if err != nil {
		logger.Errorw("Failed to start container", zap.Error(err))
		return err
	}

	if err := container.Wait(ctx, projectConfig.Port); err != nil {
		logger.Errorw("Failed to wait for container", zap.Error(err))
		return err
	}

	if _, err := Flux.db.Exec("UPDATE deployments SET url = ?, port = ? WHERE id = ?", projectConfig.Url, projectConfig.Port, deployment.ID); err != nil {
		logger.Errorw("Failed to update deployment", zap.Error(err))
		return err
	}

	// Create a new proxy that points to the new head, and replace the old one, but ensure that the old one is gracefully drained of connections
	oldProxy := deployment.Proxy
	deployment.Proxy, err = deployment.NewDeploymentProxy()
	if err != nil {
		logger.Errorw("Failed to create deployment proxy", zap.Error(err))
		return err
	}

	tx, err := Flux.db.Begin()
	if err != nil {
		logger.Errorw("Failed to begin transaction", zap.Error(err))
		return err
	}

	var containers []*Container
	var oldContainers []*Container
	// delete the old head container from the database, and update the deployment's container list
	for _, container := range deployment.Containers {
		if existingContainers[string(container.ContainerID[:])] {
			logger.Debugw("Deleting container from db", zap.ByteString("container_id", container.ContainerID[:12]))

			_, err = tx.Exec("DELETE FROM containers WHERE id = ?", container.ID)
			oldContainers = append(oldContainers, container)

			if err != nil {
				logger.Errorw("Failed to delete container", zap.Error(err))
				tx.Rollback()
				return err
			}

			continue
		}

		containers = append(containers, container)
	}

	if err := tx.Commit(); err != nil {
		logger.Errorw("Failed to commit transaction", zap.Error(err))
		return err
	}

	// gracefully shutdown the old proxy, or if it doesnt exist, just remove the containers
	if oldProxy != nil {
		go oldProxy.GracefulShutdown(oldContainers)
	} else {
		for _, container := range oldContainers {
			err := RemoveDockerContainer(context.Background(), string(container.ContainerID[:]))
			if err != nil {
				logger.Errorw("Failed to remove container", zap.Error(err))
			}
		}
	}

	deployment.Containers = containers
	return nil
}

// Remove a deployment and all of it's containers
func (d *Deployment) Remove(ctx context.Context) error {
	for _, container := range d.Containers {
		err := container.Remove(ctx)
		if err != nil {
			logger.Errorf("Failed to remove container (%s): %v\n", container.ContainerID[:12], err)
			return err
		}
	}

	Flux.proxy.RemoveDeployment(d)

	_, err := Flux.db.Exec("DELETE FROM deployments WHERE id = ?", d.ID)
	if err != nil {
		logger.Errorw("Failed to delete deployment", zap.Error(err))
		return err
	}

	return nil
}

func (d *Deployment) Start(ctx context.Context) error {
	for _, container := range d.Containers {
		err := container.Start(ctx, false)
		if err != nil {
			logger.Errorf("Failed to start container (%s): %v\n", container.ContainerID[:12], err)
			return err
		}
	}

	if d.Proxy == nil {
		d.Proxy, _ = d.NewDeploymentProxy()
		Flux.proxy.AddDeployment(d)
	}

	return nil
}

func (d *Deployment) Stop(ctx context.Context) error {
	for _, container := range d.Containers {
		err := container.Stop(ctx)
		if err != nil {
			logger.Errorf("Failed to start container (%s): %v\n", container.ContainerID[:12], err)
			return err
		}
	}

	Flux.proxy.RemoveDeployment(d)
	d.Proxy = nil

	return nil
}

// return the status of a deployment, either "running", "failed", "stopped", or "pending", errors if not all
// containers are in the same state
func (d *Deployment) Status(ctx context.Context) (string, error) {
	var status *ContainerStatus
	if d == nil {
		return "", fmt.Errorf("deployment is nil")
	}

	if d.Containers == nil {
		return "", fmt.Errorf("containers are nil")
	}

	for _, container := range d.Containers {
		containerStatus, err := container.Status(ctx)
		if err != nil {
			logger.Errorw("Failed to get container status", zap.Error(err))
			return "", err
		}

		// if not all containers are in the same state
		if status != nil && status.Status != containerStatus.Status {
			return "", fmt.Errorf("malformed deployment")
		}

		status = containerStatus
	}

	switch status.Status {
	case "running":
		return "running", nil
	case "exited":
		if status.ExitCode != 0 {
			// non-zero exit code in unix terminology means the program did no complete successfully
			return "failed", nil
		}

		return "stopped", nil
	default:
		return "pending", nil
	}
}
