package pkg

type Volume struct {
	Mountpoint string `json:"mountpoint,omitempty"`
}

type Container struct {
	Name        string   `json:"name,omitempty"`
	ImageName   string   `json:"image,omitempty"`
	Volumes     []Volume `json:"volumes,omitempty"`
	Environment []string `json:"environment,omitempty"`
}

type ProjectConfig struct {
	// name of the app
	Name string `json:"name,omitempty"`
	// public url of the app
	// TODO: support multiple urls
	Url string `json:"url,omitempty"`
	// Port the web app listens on from the head container
	Port    uint16 `json:"port,omitempty"`
	EnvFile string `json:"env_file,omitempty"`
	// additional environment variables
	Environment []string `json:"environment,omitempty"`
	// volumes for the head container
	Volumes []Volume `json:"volumes,omitempty"`
	// config for supplemental containersm
	Containers []Container `json:"containers,omitempty"`
}
