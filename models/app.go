package models

type ProjectConfig struct {
	Name        string   `json:"name,omitempty"`
	Url         string   `json:"url,omitempty"`
	Port        int      `json:"port,omitempty"`
	EnvFile     string   `json:"env_file,omitempty"`
	Environment []string `json:"environment,omitempty"`
}

type App struct {
	ID               int64         `json:"id,omitempty"`
	Name             string        `json:"name,omitempty"`
	Image            string        `json:"image,omitempty"`
	ProjectPath      string        `json:"project_path,omitempty"`
	ProjectConfig    ProjectConfig `json:"project_config,omitempty"`
	DeploymentID     int64         `json:"deployment_id,omitempty"`
	CreatedAt        string        `json:"created_at,omitempty"`
	DeploymentStatus string        `json:"deployment_status,omitempty"`
}
