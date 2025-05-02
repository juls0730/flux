package commands

import (
	"fmt"

	util "github.com/juls0730/flux/internal/util/cli"
)

func StopCommand(ctx CommandCtx, args []string) error {
	projectName, err := util.GetProject("stop", args, ctx.Config)
	if err != nil {
		return err
	}

	util.PutRequest(ctx.Config.DaemonURL+"/app/"+projectName.Id+"/stop", nil)

	fmt.Printf("Successfully stopped %s\n", projectName)
	return nil
}
