package models

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	docker "github.com/juls0730/flux/internal/docker"
	"go.uber.org/zap"
)

type App struct {
	Id           uuid.UUID   `json:"id,omitempty"`
	Name         string      `json:"name,omitempty"`
	State        string      `json:"state,omitempty"`
	Deployment   *Deployment `json:"-"`
	DeploymentID int64       `json:"deployment_id,omitempty"`
}

func (app *App) Remove(ctx context.Context, dockerClient *docker.DockerClient, db *sql.DB, logger *zap.SugaredLogger) error {
	app.Deployment.Remove(ctx, dockerClient, db, logger)
	_, err := db.ExecContext(ctx, "DELETE FROM apps WHERE id = ?", app.Id[:])
	if err != nil {
		logger.Errorw("Failed to delete app", zap.Error(err))
		return err
	}

	return nil
}
