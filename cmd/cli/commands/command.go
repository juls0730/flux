package commands

import (
	"github.com/juls0730/flux/pkg"
	"github.com/juls0730/flux/pkg/API"
)

type CommandCtx struct {
	Config      pkg.CLIConfig
	Info        API.Info
	Interactive bool
}

type CommandFunc func(CommandCtx, []string) error
