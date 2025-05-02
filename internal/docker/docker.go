package docker

import (
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

type DockerID string

// structure that holds the docker daemon information
type DockerClient struct {
	client *client.Client
	logger *zap.SugaredLogger
}

func NewDocker(rawDockerClient *client.Client, logger *zap.SugaredLogger) *DockerClient {
	if rawDockerClient == nil {
		var err error
		rawDockerClient, err = client.NewClientWithOpts(client.FromEnv)
		if err != nil {
			logger.Fatalw("Failed to create docker client", zap.Error(err))
		}
	}

	return &DockerClient{
		client: rawDockerClient,
		logger: logger,
	}
}
