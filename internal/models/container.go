package models

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	docker "github.com/juls0730/flux/internal/docker"
	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

type Container struct {
	ID int64 `json:"id"`

	Name        string          `json:"name"` // name of the container in the docker daemon
	ContainerID docker.DockerID `json:"container_id"`

	Head         bool        `json:"head"`          // if the container is the head of the deployment
	FriendlyName string      `json:"friendly_name"` // name used by other containers to reach this container
	Volumes      []*Volume   `json:"volumes"`
	Deployment   *Deployment `json:"-"`

	DeploymentID int64 `json:"deployment_id"`
}

// Create a container given a container configuration and a deployment. This will do a few things:
//
// 1. Create the container in the docker daemon
//
// 2. Create the volumes for the container
//
// 3. Insert the container and volumes into the database
//
// This will not mess with containers already in the Deployment object, it is expected that this function will only be
// called when the app in initially created
func CreateContainer(ctx context.Context, imageName string, friendlyName string, head bool, environment []string, containerVols []pkg.Volume, deployment *Deployment, logger *zap.SugaredLogger, dockerClient *docker.DockerClient, db *sql.DB) (c *Container, err error) {
	if friendlyName == "" {
		return nil, fmt.Errorf("container friendly name is empty")
	}

	if imageName == "" {
		return nil, fmt.Errorf("container image name is empty")
	}

	logger.Debugw("Creating container with image", zap.String("image", imageName))

	var volumes []*Volume
	// in the head container, we have a default volume where the project is mounted, this is important so that if the project uses sqlite for example,
	// all the data will not be lost the second the containers turns off.
	if head {
		vol, err := CreateVolume(ctx, "/workspace", dockerClient, logger)
		if err != nil {
			logger.Errorw("Failed to create head's workspace volume", zap.Error(err))
			return nil, err
		}

		vol.Mountpoint = "/workspace"

		volumes = append(volumes, vol)
	}

	for _, containerVolume := range containerVols {
		if containerVolume.Mountpoint == "" {
			return nil, fmt.Errorf("mountpoint is empty")
		}

		if containerVolume.Mountpoint == "/workspace" || containerVolume.Mountpoint == "/" {
			return nil, fmt.Errorf("invalid mountpoint")
		}

		vol, err := CreateVolume(ctx, containerVolume.Mountpoint, dockerClient, logger)
		if err != nil {
			logger.Errorw("Failed to create volume", zap.Error(err))
			return nil, err
		}

		volumes = append(volumes, vol)
	}

	// if the container is the head, build a list of hostnames that the container can reach by name for this deployment
	// TODO: this host list should be consistent across all containers in the deployment, not just the head
	var hosts []string
	if head {
		logger.Debug("Building host list")

		for _, container := range deployment.Containers() {
			containerName, err := container.GetIp(dockerClient, logger)
			if err != nil {
				logger.Errorw("Failed to get container ip", zap.Error(err))
				return nil, err
			}

			hosts = append(hosts, fmt.Sprintf("%s:%s", container.FriendlyName, containerName))
		}
	}

	// if the container is not the head, pull the image from docker hub
	if !head {
		logger.Debug("Pulling image", zap.String("image", imageName))

		image, err := dockerClient.ImagePull(ctx, imageName, image.PullOptions{})
		if err != nil {
			logger.Errorw("Failed to pull image", zap.Error(err))
			return nil, err
		}

		// blcok untile the image is pulled
		io.Copy(io.Discard, image)
	}

	dockerVols := make([]*docker.DockerVolume, 0)
	for _, volume := range volumes {
		dockerVols = append(dockerVols, &volume.DockerVolume)
	}

	logger.Debugw("Creating container", zap.String("image", imageName))
	dockerContainer, err := dockerClient.CreateDockerContainer(ctx, imageName, dockerVols, environment, hosts, nil)
	if err != nil {
		logger.Errorw("Failed to create container", zap.Error(err))
		return nil, err
	}

	c = &Container{
		ContainerID:  dockerContainer.ID,
		Name:         dockerContainer.Name,
		FriendlyName: friendlyName,
		Volumes:      volumes,
	}

	err = db.QueryRow("INSERT INTO containers (container_id, head, friendly_name, deployment_id) VALUES (?, ?, ?, ?) RETURNING id, container_id, head, deployment_id", string(c.ContainerID), head, friendlyName, deployment.ID).Scan(&c.ID, &c.ContainerID, &c.Head, &c.DeploymentID)
	if err != nil {
		logger.Errorw("Failed to insert container", zap.Error(err))
		return nil, err
	}

	tx, err := db.Begin()
	if err != nil {
		logger.Errorw("Failed to begin transaction", zap.Error(err))
		return nil, err
	}

	volumeInsertStmt, err := tx.Prepare("INSERT INTO volumes (volume_id, mountpoint, container_id) VALUES (?, ?, ?) RETURNING id, volume_id, mountpoint, container_id")
	if err != nil {
		logger.Errorw("Failed to prepare statement", zap.Error(err))
		tx.Rollback()
		return nil, err
	}

	for _, vol := range c.Volumes {
		logger.Debug("Inserting volume", zap.String("volume_id", vol.VolumeID), zap.String("mountpoint", vol.Mountpoint), zap.String("container_id", string(c.ContainerID)))
		err = volumeInsertStmt.QueryRow(vol.VolumeID, vol.Mountpoint, c.ID).Scan(&vol.ID, &vol.VolumeID, &vol.Mountpoint, &vol.ContainerID)
		if err != nil {
			logger.Errorw("Failed to insert volume", zap.Error(err))
			tx.Rollback()
			return nil, err
		}
	}

	err = tx.Commit()
	if err != nil {
		logger.Errorw("Failed to commit transaction", zap.Error(err))
		tx.Rollback()
		return nil, err
	}

	c.Deployment = deployment
	deployment.AppendContainer(c)

	return c, nil
}

// Updates Container in place
func (c *Container) Upgrade(ctx context.Context, imageName string, environment []string, dockerClient *docker.DockerClient, db *sql.DB, logger *zap.SugaredLogger) error {
	// Create new container with new image
	logger.Debugw("Upgrading container", zap.String("container_id", string(c.ContainerID[:12])))
	if c.Volumes == nil {
		return fmt.Errorf("no volumes found for container %s", c.ContainerID[:12])
	}

	containerJSON, err := dockerClient.ContainerInspect(context.Background(), c.ContainerID)
	if err != nil {
		return err
	}

	hosts := containerJSON.HostConfig.ExtraHosts

	var dockerVolumes []*docker.DockerVolume
	for _, volume := range c.Volumes {
		dockerVolumes = append(dockerVolumes, &docker.DockerVolume{
			VolumeID:   volume.VolumeID,
			Mountpoint: volume.Mountpoint,
		})
	}

	newDockerContainer, err := dockerClient.CreateDockerContainer(ctx, imageName, dockerVolumes, environment, hosts, nil)
	if err != nil {
		return err
	}

	err = db.QueryRow("INSERT INTO containers (container_id, head, friendly_name, deployment_id) VALUES (?, ?, ?, ?) RETURNING id, container_id, head, deployment_id", newDockerContainer.ID, c.Head, c.FriendlyName, c.Deployment.ID).Scan(&c.ID, &c.ContainerID, &c.Head, &c.DeploymentID)
	if err != nil {
		logger.Errorw("Failed to insert container", zap.Error(err))
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		logger.Errorw("Failed to begin transaction", zap.Error(err))
		return err
	}

	volumeUpdateStmt, err := tx.Prepare("UPDATE volumes SET container_id = ? WHERE id = ? RETURNING id, volume_id, mountpoint, container_id")
	if err != nil {
		tx.Rollback()
		logger.Errorw("Failed to prepare statement", zap.Error(err))
		return err
	}

	for _, vol := range c.Volumes {
		err = volumeUpdateStmt.QueryRow(c.ID, vol.ID).Scan(&vol.ID, &vol.VolumeID, &vol.Mountpoint, &vol.ContainerID)
		if err != nil {
			tx.Rollback()
			logger.Error("Failed to update volume", zap.Error(err))
			return err
		}
	}

	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		logger.Errorw("Failed to commit transaction", zap.Error(err))
		return err
	}

	logger.Debug("Upgraded container")

	return nil
}

func (c *Container) Remove(ctx context.Context, dockerClient *docker.DockerClient, db *sql.DB, logger *zap.SugaredLogger) error {
	logger.Debugw("Removing container", zap.String("container_id", string(c.ContainerID)))

	err := dockerClient.StopContainer(ctx, c.ContainerID)
	if err != nil {
		logger.Errorw("Failed to stop container", zap.Error(err))
		return err
	}

	for _, volume := range c.Volumes {
		logger.Debugw("Removing volume", zap.String("volume_id", volume.VolumeID))
		err := volume.Remove(ctx, dockerClient, db, logger)
		if err != nil {
			return err
		}
	}

	_, err = db.ExecContext(ctx, "DELETE FROM containers WHERE container_id = ?", c.ContainerID)
	if err != nil {
		logger.Errorw("Failed to delete container", zap.Error(err))
		return err
	}

	return dockerClient.ContainerRemove(ctx, c.ContainerID, container.RemoveOptions{})
}

func (c *Container) Start(ctx context.Context, initial bool, db *sql.DB, dockerClient *docker.DockerClient, logger *zap.SugaredLogger) error {
	logger.Debugf("Starting container %+v", c)
	logger.Info("Starting container", zap.String("container_id", string(c.ContainerID)[:12]))

	if !initial && c.Head {
		logger.Debug("Starting and repairing head container")
		containerJSON, err := dockerClient.ContainerInspect(ctx, c.ContainerID)
		if err != nil {
			return err
		}

		// remove yourself
		dockerClient.ContainerRemove(ctx, c.ContainerID, container.RemoveOptions{})

		var volumes []*docker.DockerVolume
		var hosts []string

		for _, volume := range c.Volumes {
			volumes = append(volumes, &docker.DockerVolume{
				VolumeID:   volume.VolumeID,
				Mountpoint: volume.Mountpoint,
			})
		}

		for _, supplementalContainer := range c.Deployment.Containers() {
			if supplementalContainer.Head {
				continue
			}

			ip, err := supplementalContainer.GetIp(dockerClient, logger)
			if err != nil {
				return err
			}

			hosts = append(hosts, fmt.Sprintf("%s:%s", supplementalContainer.FriendlyName, ip))
		}

		// recreate yourself
		resp, err := dockerClient.CreateDockerContainer(ctx,
			containerJSON.Image,
			volumes,
			containerJSON.Config.Env,
			hosts,
			&c.Name,
		)
		if err != nil {
			return err
		}

		c.ContainerID = resp.ID
		db.Exec("UPDATE containers SET container_id = ? WHERE id = ?", string(c.ContainerID), c.ID)
	}

	return dockerClient.ContainerStart(ctx, string(c.ContainerID), container.StartOptions{})
}

func (c *Container) Wait(ctx context.Context, port uint16, dockerClient *docker.DockerClient) error {
	return dockerClient.ContainerWait(ctx, c.ContainerID, port)
}

func (c *Container) GetIp(dockerClient *docker.DockerClient, logger *zap.SugaredLogger) (string, error) {
	containerJSON, err := dockerClient.ContainerInspect(context.Background(), c.ContainerID)
	if err != nil {
		logger.Errorw("Failed to inspect container", zap.Error(err), zap.String("container_id", string(c.ContainerID[:12])))
		return "", err
	}

	ip := containerJSON.NetworkSettings.IPAddress

	return ip, nil
}
