package pkg

type ProjectConfig struct {
	Name        string   `json:"name,omitempty"`
	Url         string   `json:"url,omitempty"`
	Port        uint16   `json:"port,omitempty"`
	EnvFile     string   `json:"env_file,omitempty"`
	Environment []string `json:"environment,omitempty"`
}
