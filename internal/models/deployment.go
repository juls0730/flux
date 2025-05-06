package models

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	"github.com/juls0730/flux/internal/docker"
	proxyManagerService "github.com/juls0730/flux/internal/services/proxy"
	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

type Deployment struct {
	ID         int64        `json:"id"`
	containers []*Container `json:"-"`
	URL        string       `json:"url"`
	Port       uint16       `json:"port"`

	headCache *Container
}

func NewDeployment() *Deployment {
	return &Deployment{
		containers: make([]*Container, 0),
	}
}

func (d *Deployment) Remove(ctx context.Context, dockerClient *docker.DockerClient, db *sql.DB, logger *zap.SugaredLogger) error {
	logger.Debugw("Removing deployment", zap.Int64("id", d.ID))
	for _, container := range d.containers {
		err := container.Remove(ctx, dockerClient, db, logger)
		if err != nil {
			return err
		}
	}

	db.ExecContext(ctx, "DELETE FROM deployments WHERE id = ?", d.ID)

	return nil
}

func (d *Deployment) Head() *Container {
	if d.headCache != nil {
		return d.headCache
	}

	for _, container := range d.containers {
		if container.Head {
			d.headCache = container
			return container
		}
	}

	return nil
}

func (d *Deployment) Containers() []*Container {
	if d.containers == nil {
		return nil
	}

	// copy the slice so that we don't modify the original
	containers := make([]*Container, len(d.containers))
	copy(containers, d.containers)

	return containers
}

func (d *Deployment) AppendContainer(container *Container) {
	d.headCache = nil
	d.containers = append(d.containers, container)
}

func (d *Deployment) Start(ctx context.Context, dockerClient *docker.DockerClient) error {
	for _, container := range d.containers {
		err := dockerClient.StartContainer(ctx, container.ContainerID)
		if err != nil {
			return fmt.Errorf("failed to start container (%s): %v", container.ContainerID, err)
		}
	}

	return nil
}

func (d *Deployment) GetInternalUrl(dockerClient *docker.DockerClient) (*url.URL, error) {
	containerJSON, err := dockerClient.ContainerInspect(context.Background(), d.Head().ContainerID)
	if err != nil {
		return nil, err
	}

	if containerJSON.NetworkSettings.IPAddress == "" {
		return nil, fmt.Errorf("no IP address found for container %s", d.Head().ContainerID)
	}

	containerUrl, err := url.Parse(fmt.Sprintf("http://%s:%d", containerJSON.NetworkSettings.IPAddress, d.Port))
	if err != nil {
		return nil, err
	}

	return containerUrl, nil
}

func (d *Deployment) Stop(ctx context.Context, dockerClient *docker.DockerClient) error {
	for _, container := range d.containers {
		err := dockerClient.StopContainer(ctx, container.ContainerID)
		if err != nil {
			return fmt.Errorf("failed to stop container (%s): %v", container.ContainerID, err)
		}
	}
	return nil
}

// gets the status of the head container, and attempt to get the supplemental containers in an aligned state
func (deployment *Deployment) Status(ctx context.Context, dockerClient *docker.DockerClient, logger *zap.SugaredLogger) (string, error) {
	// first, get the status of the head container
	headStatus, err := dockerClient.GetContainerStatus(deployment.Head().ContainerID)
	if err != nil {
		return "", err
	}

	// then, check the status of all supplemental containers
	for _, container := range deployment.containers {
		if container.Head {
			continue
		}

		containerStatus, err := dockerClient.GetContainerStatus(container.ContainerID)
		if err != nil {
			return "", err
		}

		// if the head is stopped, but the supplemental container is running, stop the supplemental container
		if headStatus.Status != "running" && containerStatus.Status == "running" {
			err := dockerClient.StopContainer(ctx, container.ContainerID)
			if err != nil {
				return "", err
			}
		}

		// if the head is running, but the supplemental container is stopped, return "failed"
		if headStatus.Status == "running" && containerStatus.Status != "running" {
			logger.Debugw("Supplemental container is not running but head is, returning to failed state", zap.String("container_id", string(container.ContainerID)))
			for _, supplementalContainer := range deployment.containers {
				err := dockerClient.StopContainer(ctx, supplementalContainer.ContainerID)
				if err != nil {
					return "", err
				}
			}

			return "failed", nil
		}
	}

	switch headStatus.Status {
	case "running":
		return "running", nil
	case "exited", "dead":
		if headStatus.ExitCode != 0 {
			// non-zero exit code in unix terminology means the program did not complete successfully
			return "failed", nil
		}

		return "stopped", nil
	default:
		return "stopped", nil
	}
}

// Takes an existing deployment, and gracefully upgrades the app to a new image
func (deployment *Deployment) Upgrade(ctx context.Context, projectConfig *pkg.ProjectConfig, imageName string, dockerClient *docker.DockerClient, proxyManager *proxyManagerService.ProxyManager, db *sql.DB, logger *zap.SugaredLogger) error {
	// copy the old head container since Upgrade updates the container in place
	oldHeadContainer := *deployment.Head()

	// we only upgrade the head container, in the future we might want to allow upgrading supplemental containers, but this should work just fine for now.
	err := deployment.Head().Upgrade(ctx, imageName, projectConfig.Environment, dockerClient, db, logger)
	if err != nil {
		logger.Errorw("Failed to upgrade container", zap.Error(err))
		return err
	}

	db.Exec("DELETE FROM containers WHERE id = ?", oldHeadContainer.ID)

	newHeadContainer := deployment.Head()
	logger.Debugw("Starting container", zap.String("container_id", string(newHeadContainer.ContainerID)))
	err = newHeadContainer.Start(ctx, true, db, dockerClient, logger)
	if err != nil {
		logger.Errorw("Failed to start container", zap.Error(err))
		return err
	}

	if err := newHeadContainer.Wait(ctx, projectConfig.Port, dockerClient); err != nil {
		logger.Errorw("Failed to wait for container", zap.Error(err))
		return err
	}

	if _, err := db.Exec("UPDATE deployments SET url = ?, port = ? WHERE id = ?", projectConfig.Url, projectConfig.Port, deployment.ID); err != nil {
		logger.Errorw("Failed to update deployment", zap.Error(err))
		return err
	}

	// Create a new proxy that points to the new head, and replace the old one, but ensure that the old one is gracefully drained of connections
	oldProxy, ok := proxyManager.Load(deployment.URL)

	newDeploymentInternalUrl, err := deployment.GetInternalUrl(dockerClient)
	if err != nil {
		logger.Errorw("Failed to get internal url", zap.Error(err))
		return err
	}
	newProxy, err := proxyManagerService.NewDeploymentProxy(*newDeploymentInternalUrl)
	if err != nil {
		logger.Errorw("Failed to create deployment proxy", zap.Error(err))
		return err
	}

	proxyManager.RemoveDeployment(deployment.URL)
	proxyManager.AddProxy(projectConfig.Url, newProxy)
	deployment.URL = projectConfig.Url

	// gracefully shutdown the old proxy, or if it doesnt exist, just remove the containers
	if ok {
		go oldProxy.GracefulShutdown(func() {
			err := dockerClient.StopContainer(context.Background(), oldHeadContainer.ContainerID)
			if err != nil {
				logger.Errorw("Failed to stop container", zap.Error(err))
				return
			}

			err = dockerClient.DeleteDockerContainer(context.Background(), oldHeadContainer.ContainerID)
			if err != nil {
				logger.Errorw("Failed to remove container", zap.Error(err))
			}
		})
	} else {
		err := dockerClient.DeleteDockerContainer(context.Background(), oldHeadContainer.ContainerID)
		if err != nil {
			logger.Errorw("Failed to remove container", zap.Error(err))
		}
	}

	return nil
}
