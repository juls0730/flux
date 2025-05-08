package commands

import (
	"github.com/juls0730/flux/pkg"
	"github.com/juls0730/flux/pkg/API"
	"go.uber.org/zap"
)

type CommandCtx struct {
	Config      pkg.CLIConfig
	Logger      *zap.SugaredLogger
	Info        *API.Info
	Interactive bool
}

type CommandFunc func(CommandCtx, []string) error
