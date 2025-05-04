package models

import (
	"context"
	"database/sql"

	docker "github.com/juls0730/flux/internal/docker"
	"go.uber.org/zap"
)

type Volume struct {
	ID int64 `json:"id"`
	docker.DockerVolume
	ContainerID int64 `json:"container_id"`
}

// Creates a volume for a container, does not insert it into the database
func CreateVolume(ctx context.Context, mountpoint string, dockerClient *docker.DockerClient, logger *zap.SugaredLogger) (*Volume, error) {
	logger.Debugw("Creating volume", zap.String("mountpoint", mountpoint))

	dockerVol, err := dockerClient.CreateDockerVolume(ctx)
	if err != nil {
		logger.Errorw("Failed to create volume", zap.Error(err))
		return nil, err
	}

	dockerVol.Mountpoint = mountpoint

	vol := &Volume{
		DockerVolume: *dockerVol,
	}

	return vol, nil
}

func (v *Volume) Remove(ctx context.Context, dockerClient *docker.DockerClient, db *sql.DB, logger *zap.SugaredLogger) error {
	logger.Debugw("Removing volume", zap.String("volume_id", v.VolumeID))

	_, err := db.ExecContext(ctx, "DELETE FROM volumes WHERE volume_id = ?", v.VolumeID)
	if err != nil {
		logger.Errorw("Failed to delete volume", zap.Error(err))
		return err
	}

	return dockerClient.DeleteDockerVolume(ctx, v.VolumeID)
}
