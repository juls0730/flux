package models

type ProjectConfig struct {
	Name        string   `json:"name"`
	Urls        []string `json:"urls"`
	Port        int      `json:"port"`
	EnvFile     string   `json:"env_file"`
	Environment []string `json:"environment"`
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
