package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/joho/godotenv"
	"github.com/juls0730/fluxd/models"
)

type ContainerManager struct {
	dockerClient *client.Client
}

func NewContainerManager() *ContainerManager {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}

	return &ContainerManager{
		dockerClient: dockerClient,
	}
}

func (cm *ContainerManager) CreateContainer(ctx context.Context, imageName, projectPath string, projectConfig models.ProjectConfig) (string, error) {
	log.Printf("Deploying container with image %s\n", imageName)

	containerName := fmt.Sprintf("%s-%s", projectConfig.Name, time.Now().Format("20060102-150405"))

	if projectConfig.EnvFile != "" {
		envBytes, err := os.Open(filepath.Join(projectPath, projectConfig.EnvFile))
		if err != nil {
			return "", fmt.Errorf("Failed to open env file: %v", err)
		}
		defer envBytes.Close()

		envVars, err := godotenv.Parse(envBytes)
		if err != nil {
			return "", fmt.Errorf("Failed to parse env file: %v", err)
		}

		for key, value := range envVars {
			projectConfig.Environment = append(projectConfig.Environment, fmt.Sprintf("%s=%s", key, value))
		}
	}

	vol, err := cm.dockerClient.VolumeCreate(ctx, volume.CreateOptions{
		Driver:     "local",
		DriverOpts: map[string]string{},
		Name:       fmt.Sprintf("%s-volume", projectConfig.Name),
	})
	if err != nil {
		return "", fmt.Errorf("Failed to create volume: %v", err)
	}

	log.Printf("Volume %s created at %s\n", vol.Name, vol.Mountpoint)

	log.Printf("Creating container %s...\n", containerName)
	resp, err := cm.dockerClient.ContainerCreate(ctx, &container.Config{
		Image: imageName,
		Env:   projectConfig.Environment,
		ExposedPorts: nat.PortSet{
			nat.Port(fmt.Sprintf("%d/tcp", projectConfig.Port)): {},
		},
		Volumes: map[string]struct{}{
			vol.Name: {},
		},
	},
		&container.HostConfig{
			PortBindings: nat.PortMap{
				nat.Port(fmt.Sprintf("%d/tcp", projectConfig.Port)): []nat.PortBinding{
					{
						HostIP:   "0.0.0.0",
						HostPort: strconv.Itoa(projectConfig.Port),
					},
				},
			},
			Mounts: []mount.Mount{
				{
					Type:     mount.TypeVolume,
					Source:   vol.Name,
					Target:   "/workspace",
					ReadOnly: false,
				},
			},
		},
		nil,
		nil,
		containerName,
	)
	if err != nil {
		return "", fmt.Errorf("Failed to create container: %v", err)
	}

	log.Printf("Created new container: %s\n", containerName)
	return resp.ID, nil
}

func (cm *ContainerManager) StartContainer(ctx context.Context, containerID string) error {
	return cm.dockerClient.ContainerStart(ctx, containerID, container.StartOptions{})
}

func (cm *ContainerManager) StopContainer(ctx context.Context, containerID string) error {
	return cm.dockerClient.ContainerStop(ctx, containerID, container.StopOptions{})
}

// RemoveContainer stops and removes a container, but be warned that this will not remove the container from the database
func (cm *ContainerManager) RemoveContainer(ctx context.Context, containerID string) error {
	if err := cm.dockerClient.ContainerStop(ctx, containerID, container.StopOptions{}); err != nil {
		return fmt.Errorf("Failed to stop existing container: %v", err)
	}

	if err := cm.dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("Failed to remove existing container: %v", err)
	}

	return nil
}

func (cm *ContainerManager) RemoveVolume(ctx context.Context, volumeID string) error {
	if err := cm.dockerClient.VolumeRemove(ctx, volumeID, true); err != nil {
		return fmt.Errorf("Failed to remove existing volume: %v", err)
	}

	return nil
}

func (cm *ContainerManager) findExistingContainers(ctx context.Context, containerPrefix string) ([]string, error) {
	containers, err := cm.dockerClient.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	var existingContainers []string
	for _, container := range containers {
		if strings.HasPrefix(container.Names[0], fmt.Sprintf("/%s", containerPrefix)) {
			existingContainers = append(existingContainers, container.ID)
		}
	}

	return existingContainers, nil
}
