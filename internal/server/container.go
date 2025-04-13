package server

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

var (
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
	Head         bool        `json:"head"`          // if the container is the head of the deployment
	FriendlyName string      `json:"friendly_name"` // name used by other containers to reach this container
	Name         string      `json:"name"`          // name of the container in the docker daemon
	Deployment   *Deployment `json:"-"`
	Volumes      []*Volume   `json:"volumes"`
	ContainerID  [64]byte    `json:"container_id"`
	DeploymentID int64       `json:"deployment_id"`
}

// Creates a volume in the docker daemon and returns the descriptor for the volume
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

// Creates a container in the docker daemon and returns the descriptor for the container
func CreateDockerContainer(ctx context.Context, imageName string, vols []*Volume, environment []string, hosts []string) (*Container, error) {
	for _, host := range hosts {
		if host == ":" {
			return nil, fmt.Errorf("invalid host %s", host)
		}
	}

	containerName := fmt.Sprintf("flux-%s", namesgenerator.GetRandomName(0))
	logger.Debugw("Creating container", zap.String("container_id", containerName))
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

	resp, err := Flux.dockerClient.ContainerCreate(ctx, &container.Config{
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
		containerName,
	)
	if err != nil {
		return nil, err
	}

	c := &Container{
		ContainerID: [64]byte([]byte(resp.ID)),
		Volumes:     vols,
		Name:        containerName,
	}

	return c, nil
}

// Create a container given a container configuration and a deployment. This will do a few things:
// 1. Create the container in the docker daemon
// 2. Create the volumes for the container
// 3. Insert the container and volumes into the database
func (flux *FluxServer) CreateContainer(ctx context.Context, container *pkg.Container, head bool, deployment *Deployment, friendlyName string) (c *Container, err error) {
	if friendlyName == "" {
		return nil, fmt.Errorf("container friendly name is empty")
	}

	if container.ImageName == "" {
		return nil, fmt.Errorf("container image name is empty")
	}

	logger.Debugw("Creating container with image", zap.String("image", container.ImageName))

	var volumes []*Volume
	// in the head container, we have a default volume where the project is mounted, this is important so that if the project uses sqlite for example,
	// all the data will not be lost the second the containers turns off.
	if head {
		vol, err := CreateDockerVolume(ctx)
		if err != nil {
			return nil, err
		}

		vol.Mountpoint = "/workspace"

		volumes = append(volumes, vol)
	}

	for _, containerVolume := range container.Volumes {
		vol, err := CreateDockerVolume(ctx)
		if err != nil {
			return nil, err
		}

		if containerVolume.Mountpoint == "" {
			return nil, fmt.Errorf("mountpoint is empty")
		}

		if containerVolume.Mountpoint == "/workspace" || containerVolume.Mountpoint == "/" {
			return nil, fmt.Errorf("invalid mountpoint")
		}

		vol.Mountpoint = containerVolume.Mountpoint
		volumes = append(volumes, vol)
	}

	// if the container is the head, build a list of hostnames that the container can reach by name for this deployment
	// TODO: this host list should be consistent across all containers in the deployment, not just the head
	var hosts []string
	if head {
		for _, container := range deployment.Containers {
			containerName, err := container.GetIp()
			if err != nil {
				return nil, err
			}

			hosts = append(hosts, fmt.Sprintf("%s:%s", container.FriendlyName, containerName))
		}
	}

	// if the container is not the head, pull the image from docker hub
	if !head {
		image, err := Flux.dockerClient.ImagePull(ctx, container.ImageName, image.PullOptions{})
		if err != nil {
			logger.Errorw("Failed to pull image", zap.Error(err))
			return nil, err
		}

		// blcok untile the image is pulled
		io.Copy(io.Discard, image)
	}

	c, err = CreateDockerContainer(ctx, container.ImageName, volumes, container.Environment, hosts)
	if err != nil {
		return nil, err
	}

	c.FriendlyName = friendlyName

	var containerIDString string
	err = containerInsertStmt.QueryRow(c.ContainerID[:], head, deployment.ID).Scan(&c.ID, &containerIDString, &c.Head, &c.DeploymentID)
	if err != nil {
		return nil, err
	}
	copy(c.ContainerID[:], containerIDString)

	tx, err := flux.db.Begin()
	if err != nil {
		return nil, err
	}

	volumeInsertStmt, err := tx.Prepare("INSERT INTO volumes (volume_id, mountpoint, container_id) VALUES (?, ?, ?) RETURNING id, volume_id, mountpoint, container_id")
	if err != nil {
		logger.Errorw("Failed to prepare statement", zap.Error(err))
		tx.Rollback()
		return nil, err
	}

	for _, vol := range c.Volumes {
		err = volumeInsertStmt.QueryRow(vol.VolumeID, vol.Mountpoint, c.ContainerID[:]).Scan(&vol.ID, &vol.VolumeID, &vol.Mountpoint, &vol.ContainerID)
		if err != nil {
			tx.Rollback()
			return nil, err
		}
	}

	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	c.Deployment = deployment
	if head {
		deployment.Head = c
	}
	deployment.Containers = append(deployment.Containers, c)

	return c, nil
}

func (c *Container) Upgrade(ctx context.Context, imageName, projectPath string, emvironment []string) (*Container, error) {
	// Create new container with new image
	logger.Debugw("Upgrading container", zap.ByteString("container_id", c.ContainerID[:12]))
	if c.Volumes == nil {
		return nil, fmt.Errorf("no volumes found for container %s", c.ContainerID[:12])
	}

	containerJSON, err := Flux.dockerClient.ContainerInspect(context.Background(), string(c.ContainerID[:]))
	if err != nil {
		return nil, err
	}

	hosts := containerJSON.HostConfig.ExtraHosts

	newContainer, err := CreateDockerContainer(ctx, imageName, c.Volumes, emvironment, hosts)
	if err != nil {
		return nil, err
	}
	newContainer.Deployment = c.Deployment

	var containerIDString string
	err = containerInsertStmt.QueryRow(newContainer.ContainerID[:], c.Head, c.Deployment.ID).Scan(&newContainer.ID, &containerIDString, &newContainer.Head, &newContainer.DeploymentID)
	if err != nil {
		logger.Errorw("Failed to insert container", zap.Error(err))
		return nil, err
	}
	copy(newContainer.ContainerID[:], containerIDString)

	tx, err := Flux.db.Begin()
	if err != nil {
		logger.Errorw("Failed to begin transaction", zap.Error(err))
		return nil, err
	}

	volumeUpdateStmt, err := tx.Prepare("UPDATE volumes SET container_id = ? WHERE id = ? RETURNING id, volume_id, mountpoint, container_id")
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	for _, vol := range newContainer.Volumes {
		err = volumeUpdateStmt.QueryRow(newContainer.ContainerID[:], vol.ID).Scan(&vol.ID, &vol.VolumeID, &vol.Mountpoint, &vol.ContainerID)
		if err != nil {
			tx.Rollback()
			logger.Error("Failed to update volume", zap.Error(err))
			return nil, err
		}
	}

	err = tx.Commit()
	if err != nil {
		tx.Rollback()
		return nil, err
	}

	logger.Debug("Upgraded container")

	return newContainer, nil
}

// initial indicates if the container was just created, because if not, we need to fix the extra hosts field since it's not guaranteed that the supplemental containers have the same ip
// as they had when the deployment was previously on
func (c *Container) Start(ctx context.Context, initial bool) error {
	if !initial && c.Head {
		containerJSON, err := Flux.dockerClient.ContainerInspect(ctx, string(c.ContainerID[:]))
		if err != nil {
			return err
		}

		// remove yourself
		Flux.dockerClient.ContainerRemove(ctx, string(c.ContainerID[:]), container.RemoveOptions{})

		var volumes map[string]struct{} = make(map[string]struct{})
		var hosts []string
		var mounts []mount.Mount

		for _, volume := range c.Volumes {
			volumes[volume.VolumeID] = struct{}{}

			mounts = append(mounts, mount.Mount{
				Type:     mount.TypeVolume,
				Source:   volume.VolumeID,
				Target:   volume.Mountpoint,
				ReadOnly: false,
			})
		}

		for _, supplementalContainer := range c.Deployment.Containers {
			if supplementalContainer.Head {
				continue
			}

			ip, err := supplementalContainer.GetIp()
			if err != nil {
				return err
			}

			hosts = append(hosts, fmt.Sprintf("%s:%s", supplementalContainer.FriendlyName, ip))
		}

		// recreate yourself
		// TODO: pull this out so it stays in sync with CreateDockerContainer
		resp, err := Flux.dockerClient.ContainerCreate(ctx, &container.Config{
			Image:   containerJSON.Image,
			Env:     containerJSON.Config.Env,
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
			c.Name,
		)
		if err != nil {
			return err
		}

		c.ContainerID = [64]byte([]byte(resp.ID))
		Flux.db.Exec("UPDATE containers SET container_id = ? WHERE id = ?", c.ContainerID[:], c.ID)
	}

	return Flux.dockerClient.ContainerStart(ctx, string(c.ContainerID[:]), container.StartOptions{})
}

func (c *Container) Stop(ctx context.Context) error {
	return Flux.dockerClient.ContainerStop(ctx, string(c.ContainerID[:]), container.StopOptions{})
}

// Stop and remove a container and all of its volumes
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

type ContainerStatus struct {
	Status   string
	ExitCode int
}

func (c *Container) Status(ctx context.Context) (*ContainerStatus, error) {
	containerJSON, err := Flux.dockerClient.ContainerInspect(ctx, string(c.ContainerID[:]))
	if err != nil {
		return nil, err
	}

	containerStatus := &ContainerStatus{
		Status:   containerJSON.State.Status,
		ExitCode: containerJSON.State.ExitCode,
	}

	return containerStatus, nil
}

func (c *Container) GetIp() (string, error) {
	containerJSON, err := Flux.dockerClient.ContainerInspect(context.Background(), string(c.ContainerID[:]))
	if err != nil {
		return "", err
	}

	ip := containerJSON.NetworkSettings.IPAddress

	return ip, nil
}

// Stops and deletes a container from the docker daemon
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
