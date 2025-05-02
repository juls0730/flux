package commands

import (
	"fmt"

	util "github.com/juls0730/flux/internal/util/cli"
)

func StartCommand(ctx CommandCtx, args []string) error {
	projectName, err := util.GetProject("start", args, ctx.Config)
	if err != nil {
		return err
	}

	// Put request to start the project, since the start endpoint is idempotent.
	// If the project is already running, this will return a 304 Not Modified
	util.PutRequest(ctx.Config.DaemonURL+"/app/"+projectName.Id+"/start", nil)

	fmt.Printf("Successfully started %s\n", projectName)

	return nil
}
