package pkg

type App struct {
	ID               int64  `json:"id,omitempty"`
	Name             string `json:"name,omitempty"`
	DeploymentID     int64  `json:"deployment_id,omitempty"`
	DeploymentStatus string `json:"deployment_status,omitempty"`
}

type Compression struct {
	Enabled bool `json:"enabled"`
	Level   int  `json:"level,omitempty"`
}

type Info struct {
	Compression Compression `json:"compression"`
}

type DeploymentEvent struct {
	Message interface{} `json:"message"`
}
