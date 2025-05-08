package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/namesgenerator"
	"go.uber.org/zap"
)

type DockerContainer struct {
	ID      DockerID
	Name    string
	Volumes []*DockerVolume
}

// Creates a container in the docker daemon and returns the descriptor for the container
func (d *DockerClient) CreateDockerContainer(ctx context.Context, imageName string, vols []*DockerVolume, environment []string, hosts []string, name *string) (*DockerContainer, error) {
	for _, host := range hosts {
		if host == ":" {
			return nil, fmt.Errorf("invalid host %s", host)
		}
	}

	if name == nil {
		containerName := fmt.Sprintf("flux-%s", namesgenerator.GetRandomName(0))
		name = &containerName
	}
	d.logger.Debugw("Creating container", zap.String("container_name", *name))
	mounts := make([]mount.Mount, len(vols))
	volumes := make(map[string]struct{}, len(vols))
	for i, volume := range vols {
		volumes[volume.VolumeID] = struct{}{}

		mounts[i] = mount.Mount{
			Type:     mount.TypeVolume,
			Source:   volume.VolumeID,
			Target:   volume.Mountpoint,
			ReadOnly: false,
		}
	}

	resp, err := d.client.ContainerCreate(ctx, &container.Config{
		Image:   imageName,
		Env:     environment,
		Volumes: volumes,
		Labels: map[string]string{
			"managed-by": "flux",
		},
	},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			NetworkMode:   "bridge",
			Mounts:        mounts,
			ExtraHosts:    hosts,
		},
		nil,
		nil,
		*name,
	)
	if err != nil {
		return nil, err
	}

	c := &DockerContainer{
		ID:      DockerID(resp.ID),
		Name:    *name,
		Volumes: vols,
	}

	return c, nil
}

func (d *DockerClient) ContainerRemove(ctx context.Context, containerID DockerID, options container.RemoveOptions) error {
	d.logger.Debugw("Removing container", zap.String("container_id", string(containerID)))
	return d.client.ContainerRemove(ctx, string(containerID), options)
}

func (d *DockerClient) StartContainer(ctx context.Context, containerID DockerID) error {
	return d.client.ContainerStart(ctx, string(containerID), container.StartOptions{})
}

const CONTAINER_START_TIMEOUT = 30 * time.Second

// blocks until the container returns a 200 status code for a max of CONTAINER_START_TIMEOUT (30 seconds)
func (d *DockerClient) ContainerWait(ctx context.Context, containerID DockerID, port uint16) error {
	ctx, cancel := context.WithTimeout(ctx, CONTAINER_START_TIMEOUT)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("container failed to become ready in time")

		default:
			containerJSON, err := d.ContainerInspect(ctx, containerID)
			if err != nil {
				return err
			}

			if containerJSON.State.Running {
				resp, err := http.Get(fmt.Sprintf("http://%s:%d/", containerJSON.NetworkSettings.IPAddress, port))
				if err == nil && resp.StatusCode == http.StatusOK {
					return nil
				}
			}

			time.Sleep(time.Second)
		}
	}
}

func (d *DockerClient) DeleteDockerContainer(ctx context.Context, containerID DockerID) error {
	d.logger.Debugw("Removing container", zap.String("container_id", string(containerID)))
	return d.client.ContainerRemove(ctx, string(containerID), container.RemoveOptions{})
}

func (d *DockerClient) GetContainerIp(containerID DockerID) (string, error) {
	containerJSON, err := d.client.ContainerInspect(context.Background(), string(containerID))
	if err != nil {
		return "", err
	}

	ip := containerJSON.NetworkSettings.IPAddress

	return ip, nil
}

type ContainerStatus struct {
	// Can be  "created", "running", "paused", "restarting", "removing", "exited", or "dead"
	Status   string
	ExitCode int
}

func (d *DockerClient) GetContainerStatus(containerID DockerID) (*ContainerStatus, error) {
	containerJSON, err := d.client.ContainerInspect(context.Background(), string(containerID))
	if err != nil {
		return nil, err
	}

	containerStatus := &ContainerStatus{
		Status:   containerJSON.State.Status,
		ExitCode: containerJSON.State.ExitCode,
	}

	return containerStatus, nil
}

func (d *DockerClient) StopContainer(ctx context.Context, containerID DockerID) error {
	d.logger.Debugw("Stopping container", zap.String("container_id", string(containerID)))
	return d.client.ContainerStop(ctx, string(containerID), container.StopOptions{})
}

func (d *DockerClient) ImagePull(ctx context.Context, imageName string, options image.PullOptions) (io.ReadCloser, error) {
	d.logger.Debugw("Pulling image", zap.String("image", imageName))
	return d.client.ImagePull(ctx, imageName, options)
}

func (d *DockerClient) ContainerInspect(ctx context.Context, containerID DockerID) (dockerTypes.ContainerJSON, error) {
	d.logger.Debugw("Inspecting container", zap.String("container_id", string(containerID)))
	return d.client.ContainerInspect(ctx, string(containerID))
}

func (d *DockerClient) ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error {
	d.logger.Debugw("Starting container", zap.String("container_id", containerID))
	return d.client.ContainerStart(ctx, containerID, options)
}
