package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/volume"
	"go.uber.org/zap"
)

type DockerVolume struct {
	VolumeID   string
	Mountpoint string
}

func (d *DockerClient) CreateDockerVolume(ctx context.Context) (vol *DockerVolume, err error) {
	dockerVolume, err := d.client.VolumeCreate(ctx, volume.CreateOptions{
		Driver:     "local",
		DriverOpts: map[string]string{},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create volume: %v", err)
	}

	d.logger.Debugw("Volume created", zap.String("volume_id", dockerVolume.Name), zap.String("mountpoint", dockerVolume.Mountpoint))

	vol = &DockerVolume{
		VolumeID: dockerVolume.Name,
	}

	return vol, nil
}

func (d *DockerClient) DeleteDockerVolume(ctx context.Context, volumeID string) error {
	d.logger.Debugw("Removing volume", zap.String("volume_id", volumeID))
	return d.client.VolumeRemove(ctx, volumeID, true)
}
