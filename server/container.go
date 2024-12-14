package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/joho/godotenv"
	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

var (
	volumeInsertStmt    *sql.Stmt
	volumeUpdateStmt    *sql.Stmt
	containerInsertStmt *sql.Stmt
)

type Volume struct {
	ID          int64  `json:"id"`
	VolumeID    string `json:"volume_id"`
	Mountpoint  string `json:"mountpoint"`
	ContainerID string `json:"container_id"`
}

type Container struct {
	ID           int64       `json:"id"`
	Head         bool        `json:"head"` // if the container is the head of the deployment
	Deployment   *Deployment `json:"-"`
	Volumes      []Volume    `json:"volumes"`
	ContainerID  [64]byte    `json:"container_id"`
	DeploymentID int64       `json:"deployment_id"`
}

func CreateDockerVolume(ctx context.Context) (vol *Volume, err error) {
	dockerVolume, err := Flux.dockerClient.VolumeCreate(ctx, volume.CreateOptions{
		Driver:     "local",
		DriverOpts: map[string]string{},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create volume: %v", err)
	}

	logger.Debugw("Volume created", zap.String("volume_id", dockerVolume.Name), zap.String("mountpoint", dockerVolume.Mountpoint))

	vol = &Volume{
		VolumeID: dockerVolume.Name,
	}

	return vol, nil
}

func CreateDockerContainer(ctx context.Context, imageName, projectPath string, projectConfig pkg.ProjectConfig, vol *Volume) (*Container, error) {
	containerName := fmt.Sprintf("%s-%s", projectConfig.Name, time.Now().Format("20060102-150405"))

	if projectConfig.EnvFile != "" {
		envBytes, err := os.Open(filepath.Join(projectPath, projectConfig.EnvFile))
		if err != nil {
			return nil, fmt.Errorf("failed to open env file: %v", err)
		}
		defer envBytes.Close()

		envVars, err := godotenv.Parse(envBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse env file: %v", err)
		}

		for key, value := range envVars {
			projectConfig.Environment = append(projectConfig.Environment, fmt.Sprintf("%s=%s", key, value))
		}
	}

	logger.Debugw("Creating container", zap.String("container_id", containerName))
	resp, err := Flux.dockerClient.ContainerCreate(ctx, &container.Config{
		Image: imageName,
		Env:   projectConfig.Environment,
		Volumes: map[string]struct{}{
			vol.VolumeID: {},
		},
	},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			NetworkMode:   "bridge",
			Mounts: []mount.Mount{
				{
					Type:     mount.TypeVolume,
					Source:   vol.VolumeID,
					Target:   vol.Mountpoint,
					ReadOnly: false,
				},
			},
		},
		nil,
		nil,
		containerName,
	)
	if err != nil {
		return nil, err
	}

	c := &Container{
		ContainerID: [64]byte([]byte(resp.ID)),
		Volumes:     []Volume{*vol},
	}

	return c, nil
}

func CreateContainer(ctx context.Context, imageName, projectPath string, projectConfig pkg.ProjectConfig, head bool, deployment *Deployment) (c *Container, err error) {
	logger.Debugw("Creating container with image", zap.String("image", imageName))

	if projectConfig.EnvFile != "" {
		envBytes, err := os.Open(filepath.Join(projectPath, projectConfig.EnvFile))
		if err != nil {
			return nil, fmt.Errorf("failed to open env file: %v", err)
		}
		defer envBytes.Close()

		envVars, err := godotenv.Parse(envBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse env file: %v", err)
		}

		for key, value := range envVars {
			projectConfig.Environment = append(projectConfig.Environment, fmt.Sprintf("%s=%s", key, value))
		}
	}

	var vol *Volume
	vol, err = CreateDockerVolume(ctx)
	if err != nil {
		return nil, err
	}

	vol.Mountpoint = "/workspace"

	if volumeInsertStmt == nil {
		volumeInsertStmt, err = Flux.db.Prepare("INSERT INTO volumes (volume_id, mountpoint, container_id) VALUES (?, ?, ?) RETURNING id, volume_id, mountpoint, container_id")
		if err != nil {
			logger.Errorw("Failed to prepare statement", zap.Error(err))
			return nil, err
		}
	}

	c, err = CreateDockerContainer(ctx, imageName, projectPath, projectConfig, vol)
	if err != nil {
		return nil, err
	}

	if containerInsertStmt == nil {
		containerInsertStmt, err = Flux.db.Prepare("INSERT INTO containers (container_id, head, deployment_id) VALUES ($1, $2, $3) RETURNING id, container_id, head, deployment_id")
		if err != nil {
			return nil, err
		}
	}

	var containerIDString string
	err = containerInsertStmt.QueryRow(c.ContainerID[:], head, deployment.ID).Scan(&c.ID, &containerIDString, &c.Head, &c.DeploymentID)
	if err != nil {
		return nil, err
	}
	copy(c.ContainerID[:], containerIDString)

	err = volumeInsertStmt.QueryRow(vol.VolumeID, vol.Mountpoint, c.ContainerID[:]).Scan(&vol.ID, &vol.VolumeID, &vol.Mountpoint, &vol.ContainerID)
	if err != nil {
		return nil, err
	}

	c.Deployment = deployment
	if head {
		deployment.Head = c
	}
	deployment.Containers = append(deployment.Containers, c)

	return c, nil
}

func (c *Container) Upgrade(ctx context.Context, imageName, projectPath string, projectConfig pkg.ProjectConfig) (*Container, error) {
	// Create new container with new image
	logger.Debugw("Upgrading container", zap.ByteString("container_id", c.ContainerID[:12]))
	if c.Volumes == nil {
		return nil, fmt.Errorf("no volumes found for container %s", c.ContainerID[:12])
	}

	vol := &c.Volumes[0]

	newContainer, err := CreateDockerContainer(ctx, imageName, projectPath, projectConfig, vol)
	if err != nil {
		return nil, err
	}
	newContainer.Deployment = c.Deployment

	if containerInsertStmt == nil {
		containerInsertStmt, err = Flux.db.Prepare("INSERT INTO containers (container_id, head, deployment_id) VALUES ($1, $2, $3) RETURNING id, container_id, head, deployment_id")
		if err != nil {
			return nil, err
		}
	}

	var containerIDString string
	err = containerInsertStmt.QueryRow(newContainer.ContainerID[:], c.Head, c.Deployment.ID).Scan(&newContainer.ID, &containerIDString, &newContainer.Head, &newContainer.DeploymentID)
	if err != nil {
		logger.Errorw("Failed to insert container", zap.Error(err))
		return nil, err
	}
	copy(newContainer.ContainerID[:], containerIDString)

	if volumeUpdateStmt == nil {
		volumeUpdateStmt, err = Flux.db.Prepare("UPDATE volumes SET container_id = ? WHERE id = ? RETURNING id, volume_id, mountpoint, container_id")
		if err != nil {
			return nil, err
		}
	}

	vol = &newContainer.Volumes[0]
	volumeUpdateStmt.QueryRow(newContainer.ContainerID[:], vol.ID).Scan(&vol.ID, &vol.VolumeID, &vol.Mountpoint, &vol.ContainerID)

	logger.Debug("Upgraded container")

	return newContainer, nil
}

func (c *Container) Start(ctx context.Context) error {
	return Flux.dockerClient.ContainerStart(ctx, string(c.ContainerID[:]), container.StartOptions{})
}

func (c *Container) Stop(ctx context.Context) error {
	return Flux.dockerClient.ContainerStop(ctx, string(c.ContainerID[:]), container.StopOptions{})
}

func (c *Container) Remove(ctx context.Context) error {
	err := RemoveDockerContainer(ctx, string(c.ContainerID[:]))

	if err != nil {
		return fmt.Errorf("failed to remove container (%s): %v", c.ContainerID[:12], err)
	}

	tx, err := Flux.db.Begin()
	if err != nil {
		logger.Errorw("Failed to begin transaction", zap.Error(err))
		return err
	}

	_, err = tx.Exec("DELETE FROM containers WHERE container_id = ?", c.ContainerID[:])
	if err != nil {
		tx.Rollback()
		return err
	}

	for _, volume := range c.Volumes {
		if err := RemoveVolume(ctx, volume.VolumeID); err != nil {
			tx.Rollback()
			return fmt.Errorf("failed to remove volume (%s): %v", volume.VolumeID, err)
		}

		_, err = tx.Exec("DELETE FROM volumes WHERE volume_id = ?", volume.VolumeID)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		logger.Errorw("Failed to commit transaction", zap.Error(err))
		return err
	}

	return nil
}

func (c *Container) Wait(ctx context.Context, port uint16) error {
	return WaitForDockerContainer(ctx, string(c.ContainerID[:]), port)
}

func (c *Container) Status(ctx context.Context) (string, error) {
	containerJSON, err := Flux.dockerClient.ContainerInspect(ctx, string(c.ContainerID[:]))
	if err != nil {
		return "", err
	}

	return containerJSON.State.Status, nil
}

// RemoveContainer stops and removes a container, but be warned that this will not remove the container from the database
func RemoveDockerContainer(ctx context.Context, containerID string) error {
	if err := Flux.dockerClient.ContainerStop(ctx, containerID, container.StopOptions{}); err != nil {
		return fmt.Errorf("failed to stop container (%s): %v", containerID[:12], err)
	}

	if err := Flux.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("failed to remove container (%s): %v", containerID[:12], err)
	}

	return nil
}

// scuffed af "health check" for docker containers
func WaitForDockerContainer(ctx context.Context, containerID string, containerPort uint16) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("container failed to become ready in time")

		default:
			containerJSON, err := Flux.dockerClient.ContainerInspect(ctx, containerID)
			if err != nil {
				return err
			}

			if containerJSON.State.Running {
				resp, err := http.Get(fmt.Sprintf("http://%s:%d/", containerJSON.NetworkSettings.IPAddress, containerPort))
				if err == nil && resp.StatusCode == http.StatusOK {
					return nil
				}
			}

			time.Sleep(time.Second)
		}
	}
}

func GracefullyRemoveDockerContainer(ctx context.Context, containerID string) error {
	timeout := 30
	err := Flux.dockerClient.ContainerStop(ctx, containerID, container.StopOptions{
		Timeout: &timeout,
	})
	if err != nil {
		return fmt.Errorf("failed to stop container: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return Flux.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{})
		default:
			containerJSON, err := Flux.dockerClient.ContainerInspect(ctx, containerID)
			if err != nil {
				return err
			}

			if !containerJSON.State.Running {
				return Flux.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{})
			}

			time.Sleep(time.Second)
		}
	}
}

func RemoveVolume(ctx context.Context, volumeID string) error {
	logger.Debugw("Removed volume", zap.String("volume_id", volumeID))

	if err := Flux.dockerClient.VolumeRemove(ctx, volumeID, true); err != nil {
		return fmt.Errorf("failed to remove volume (%s): %v", volumeID, err)
	}

	return nil
}

func findExistingDockerContainers(ctx context.Context, containerPrefix string) (map[string]bool, error) {
	containers, err := Flux.dockerClient.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	var existingContainers map[string]bool = make(map[string]bool)
	for _, container := range containers {
		if strings.HasPrefix(container.Names[0], fmt.Sprintf("/%s-", containerPrefix)) {
			existingContainers[container.ID] = true
		}
	}

	return existingContainers, nil
}
