package API

import "github.com/google/uuid"

type App struct {
	Id               uuid.UUID `json:"id,omitempty"`
	Name             string    `json:"name,omitempty"`
	DeploymentID     int64     `json:"deployment_id,omitempty"`
	DeploymentStatus string    `json:"deployment_status,omitempty"`
}
