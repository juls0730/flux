package pkg

type App struct {
	ID               int64  `json:"id,omitempty"`
	Name             string `json:"name,omitempty"`
	DeploymentID     int64  `json:"deployment_id,omitempty"`
	DeploymentStatus string `json:"deployment_status,omitempty"`
}

// TODO: this should be flattened to an int, where 0 = disabled and any other number is the level
type Compression struct {
	Enabled bool `json:"enabled"`
	Level   int  `json:"level,omitempty"`
}

type Info struct {
	Compression Compression `json:"compression"`
	Version     string      `json:"version"`
}

type DeploymentEvent struct {
	Message interface{} `json:"message"`
}
