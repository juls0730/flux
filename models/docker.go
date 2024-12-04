package models

type Containers struct {
	ID           string `json:"id"`
	ContainerID  string `json:"container_id"`
	DeploymentID int64  `json:"deployment_id"`
	CreatedAt    string `json:"created_at"`
}

type Deployments struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
}
