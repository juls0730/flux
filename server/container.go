package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/joho/godotenv"
	"github.com/juls0730/fluxd/pkg"
)

var dockerClient *client.Client

type Volume struct {
	ID          int64  `json:"id"`
	VolumeID    string `json:"volume_id"`
	ContainerID int64  `json:"container_id"`
}

type Container struct {
	ID           int64 `json:"id"`
	Head         bool  `json:"head"` // if the container is the head of the deployment
	Deployment   *Deployment
	Volumes      []Volume `json:"volumes"`
	ContainerID  [64]byte `json:"container_id"`
	DeploymentID int64    `json:"deployment_id"`
}

func init() {
	log.Printf("Initializing Docker client...\n")

	var err error
	dockerClient, err = client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatalf("Failed to create Docker client: %v", err)
	}
}

func CreateVolume(ctx context.Context, name string) (vol *Volume, err error) {
	dockerVolume, err := dockerClient.VolumeCreate(ctx, volume.CreateOptions{
		Driver:     "local",
		DriverOpts: map[string]string{},
		Name:       name,
	})
	if err != nil {
		return nil, fmt.Errorf("Failed to create volume: %v", err)
	}

	log.Printf("Volume %s created at %s\n", dockerVolume.Name, dockerVolume.Mountpoint)

	vol = &Volume{
		VolumeID: dockerVolume.Name,
	}

	return vol, nil
}

func CreateDockerContainer(ctx context.Context, imageName, projectPath string, projectConfig pkg.ProjectConfig) (c *Container, err error) {
	log.Printf("Deploying container with image %s\n", imageName)

	containerName := fmt.Sprintf("%s-%s", projectConfig.Name, time.Now().Format("20060102-150405"))

	if projectConfig.EnvFile != "" {
		envBytes, err := os.Open(filepath.Join(projectPath, projectConfig.EnvFile))
		if err != nil {
			return nil, fmt.Errorf("Failed to open env file: %v", err)
		}
		defer envBytes.Close()

		envVars, err := godotenv.Parse(envBytes)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse env file: %v", err)
		}

		for key, value := range envVars {
			projectConfig.Environment = append(projectConfig.Environment, fmt.Sprintf("%s=%s", key, value))
		}
	}

	vol, err := CreateVolume(ctx, fmt.Sprintf("flux_%s-volume", projectConfig.Name))

	log.Printf("Creating container %s...\n", containerName)
	resp, err := dockerClient.ContainerCreate(ctx, &container.Config{
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
		return nil, fmt.Errorf("Failed to create container: %v", err)
	}

	c = &Container{
		ContainerID: [64]byte([]byte(resp.ID)),
		Volumes:     []Volume{*vol},
	}

	log.Printf("Created new container: %s\n", containerName)
	return c, nil
}

func (c *Container) Start(ctx context.Context) error {
	return dockerClient.ContainerStart(ctx, string(c.ContainerID[:]), container.StartOptions{})
}

func (c *Container) Stop(ctx context.Context) error {
	return dockerClient.ContainerStop(ctx, string(c.ContainerID[:]), container.StopOptions{})
}

func (c *Container) Remove(ctx context.Context) error {
	err := RemoveDockerContainer(ctx, string(c.ContainerID[:]))

	if err != nil {
		return fmt.Errorf("Failed to remove container (%s): %v", c.ContainerID[:12], err)
	}

	tx, err := Flux.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
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
			return fmt.Errorf("Failed to remove volume (%s): %v", volume.VolumeID, err)
		}

		_, err = tx.Exec("DELETE FROM volumes WHERE volume_id = ?", volume.VolumeID)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		return err
	}

	return nil
}

func (c *Container) Wait(ctx context.Context, port uint16) error {
	return WaitForDockerContainer(ctx, string(c.ContainerID[:]), port)
}

// RemoveContainer stops and removes a container, but be warned that this will not remove the container from the database
func RemoveDockerContainer(ctx context.Context, containerID string) error {
	if err := dockerClient.ContainerStop(ctx, containerID, container.StopOptions{}); err != nil {
		return fmt.Errorf("Failed to stop container (%s): %v", containerID[:12], err)
	}

	if err := dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("Failed to remove container (%s): %v", containerID[:12], err)
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
			containerJSON, err := dockerClient.ContainerInspect(ctx, containerID)
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
	err := dockerClient.ContainerStop(ctx, containerID, container.StopOptions{
		Timeout: &timeout,
	})
	if err != nil {
		return fmt.Errorf("Failed to stop container: %v", err)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{})
		default:
			containerJSON, err := dockerClient.ContainerInspect(ctx, containerID)
			if err != nil {
				return err
			}

			if !containerJSON.State.Running {
				return dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{})
			}

			time.Sleep(time.Second)
		}
	}
}

func RemoveVolume(ctx context.Context, volumeID string) error {
	log.Printf("Removed volume %s\n", volumeID)

	if err := dockerClient.VolumeRemove(ctx, volumeID, true); err != nil {
		return fmt.Errorf("Failed to remove volume (%s): %v", volumeID, err)
	}

	return nil
}

func findExistingDockerContainers(ctx context.Context, containerPrefix string) (map[string]bool, error) {
	containers, err := dockerClient.ContainerList(ctx, container.ListOptions{
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
