package commands

import (
	"fmt"

	util "github.com/juls0730/flux/internal/util/cli"
	"github.com/juls0730/flux/pkg/API"
)

func ListCommand(ctx CommandCtx, args []string) error {
	apps, err := util.GetRequest[[]API.App](ctx.Config.DaemonURL + "/apps")
	if err != nil {
		return fmt.Errorf("failed to get apps: %v", err)
	}

	if len(*apps) == 0 {
		fmt.Println("No apps found")
		return nil
	}

	for _, app := range *apps {
		fmt.Printf("%s (%s)\n", app.Name, app.DeploymentStatus)
	}

	return nil
}
