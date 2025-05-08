package commands

import (
	"fmt"

	util "github.com/juls0730/flux/internal/util/cli"
)

func StopCommand(ctx CommandCtx, args []string) error {
	projectName, err := util.GetProject("stop", args, ctx.Config, ctx.Logger)
	if err != nil {
		return err
	}

	err = util.PutRequest(ctx.Config.DaemonURL+"/app/"+projectName.Id+"/stop", nil, ctx.Logger)
	if err != nil {
		return fmt.Errorf("failed to stop %s: %v", projectName.Name, err)
	}

	fmt.Printf("Successfully stopped %s\n", projectName.Name)
	return nil
}
