package models

type ProjectConfig struct {
	Name        string   `json:"name"`
	Urls        []string `json:"urls"`
	Port        int      `json:"port"`
	EnvFile     string   `json:"env_file"`
	Environment []string `json:"environment"`
}

type App struct {
	ID            int64         `json:"id"`
	Name          string        `json:"name"`
	Image         string        `json:"image"`
	ProjectPath   string        `json:"project_path"`
	ProjectConfig ProjectConfig `json:"project_config"`
	DeploymentID  int64         `json:"deployment_id"`
	CreatedAt     string        `json:"created_at"`
}
