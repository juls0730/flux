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
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/joho/godotenv"
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

func (cm *ContainerManager) DeployContainer(ctx context.Context, imageName, containerPrefix, projectPath string, projectConfig ProjectConfig) (string, error) {
	containerName := fmt.Sprintf("%s-%s", containerPrefix, time.Now().Format("20060102-150405"))

	existingContainers, err := cm.findExistingContainers(ctx, containerPrefix)
	if err != nil {
		return "", fmt.Errorf("Failed to find existing containers: %v", err)
	}

	// TODO: swap containers if they are running and have the same image so that we can have a constant uptime
	for _, existingContainer := range existingContainers {
		log.Printf("Stopping existing container: %s", existingContainer)

		if err := cm.dockerClient.ContainerStop(ctx, existingContainer, container.StopOptions{}); err != nil {
			return "", fmt.Errorf("Failed to stop existing container: %v", err)
		}

		if err := cm.dockerClient.ContainerRemove(ctx, existingContainer, container.RemoveOptions{}); err != nil {
			return "", fmt.Errorf("Failed to remove existing container: %v", err)
		}
	}

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

	resp, err := cm.dockerClient.ContainerCreate(ctx, &container.Config{
		Image: imageName,
		Env:   projectConfig.Environment,
		ExposedPorts: nat.PortSet{
			nat.Port(fmt.Sprintf("%d/tcp", projectConfig.Port)): {},
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
		},
		nil,
		nil,
		containerName,
	)
	if err != nil {
		return "", fmt.Errorf("Failed to create container: %v", err)
	}

	if err := cm.dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("Failed to start container: %v", err)
	}

	log.Printf("Deployed new container: %s", containerName)
	return resp.ID, nil
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
