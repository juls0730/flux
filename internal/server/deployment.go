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
func (flux *FluxServer) CreateDeployment(port uint16, appUrl string) (*Deployment, error) {
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
	// we only upgrade the head container, in the future we might want to allow upgrading supplemental containers, but this should work just fine for now.
	newHeadContainer, err := deployment.Head.Upgrade(ctx, imageName, projectPath, projectConfig.Environment)
	if err != nil {
		logger.Errorw("Failed to upgrade container", zap.Error(err))
		return err
	}

	oldHeadContainer := deployment.Head
	Flux.db.Exec("DELETE FROM containers WHERE id = ?", oldHeadContainer.ID)

	var containers []*Container
	for _, container := range deployment.Containers {
		if !container.Head {
			containers = append(containers, container)
		}
	}

	deployment.Head = newHeadContainer
	deployment.Containers = append(containers, newHeadContainer)

	logger.Debugw("Starting container", zap.ByteString("container_id", newHeadContainer.ContainerID[:12]))
	err = newHeadContainer.Start(ctx, true)
	if err != nil {
		logger.Errorw("Failed to start container", zap.Error(err))
		return err
	}

	if err := newHeadContainer.Wait(ctx, projectConfig.Port); err != nil {
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

	// gracefully shutdown the old proxy, or if it doesnt exist, just remove the containers
	if oldProxy != nil {
		go oldProxy.GracefulShutdown([]*Container{oldHeadContainer})
	} else {
		err := RemoveDockerContainer(context.Background(), string(oldHeadContainer.ContainerID[:]))
		if err != nil {
			logger.Errorw("Failed to remove container", zap.Error(err))
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
