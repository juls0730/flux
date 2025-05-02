package API

import (
	"mime/multipart"

	"github.com/google/uuid"
	"github.com/juls0730/flux/pkg"
)

type DeployRequest struct {
	Id     uuid.UUID         `form:"id"`
	Config pkg.ProjectConfig `form:"config"`
	Code   multipart.File    `form:"code"`
}

type DeploymentEvent struct {
	Message any    `json:"message"`
	Stage   string `json:"stage"`
}
