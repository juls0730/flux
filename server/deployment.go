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

// Creates a deployment and containers in the database
func CreateDeployment(port uint16, appUrl string, db *sql.DB) (*Deployment, error) {
	var deployment Deployment
	var err error

	if deploymentInsertStmt == nil {
		deploymentInsertStmt, err = db.Prepare("INSERT INTO deployments (url, port) VALUES ($1, $2) RETURNING id, url, port")
		if err != nil {
			logger.Errorw("Failed to prepare statement", zap.Error(err))
			return nil, err
		}
	}

	err = deploymentInsertStmt.QueryRow(appUrl, port).Scan(&deployment.ID, &deployment.URL, &deployment.Port)
	if err != nil {
		logger.Errorw("Failed to insert deployment", zap.Error(err))
		return nil, err
	}

	return &deployment, nil
}

func (deployment *Deployment) Upgrade(ctx context.Context, projectConfig pkg.ProjectConfig, imageName string, projectPath string) error {
	existingContainers, err := findExistingDockerContainers(ctx, projectConfig.Name)
	if err != nil {
		return fmt.Errorf("failed to find existing containers: %v", err)
	}

	container, err := deployment.Head.Upgrade(ctx, imageName, projectPath, projectConfig)
	if err != nil {
		logger.Errorw("Failed to upgrade container", zap.Error(err))
		return err
	}

	// copy(container.ContainerID[:], containerIDString)
	deployment.Head = container
	deployment.Containers = append(deployment.Containers, container)

	logger.Debugw("Starting container", zap.ByteString("container_id", container.ContainerID[:12]))
	err = container.Start(ctx)
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

	// Create a new proxy that points to the new head, and replace the old one, but ensure that the old one is gracefully shutdown
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
		err := container.Start(ctx)
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

func (d *Deployment) Status(ctx context.Context) (string, error) {
	var status string
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
		if status != "" && status != containerStatus {
			return "", fmt.Errorf("malformed deployment")
		}

		status = containerStatus
	}

	switch status {
	case "running":
		return "running", nil
	case "exited":
		return "stopped", nil
	default:
		return "pending", nil
	}
}
