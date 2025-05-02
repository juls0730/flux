package API

type Info struct {
	// an int between 0 and 9 that represents the compression level, 0 being no compression, 9 being maximum compression
	CompressionLevel int `json:"compression_level"`
	// the version of the daemon (see pkg/version.go)
	Version string `json:"version"`
}
