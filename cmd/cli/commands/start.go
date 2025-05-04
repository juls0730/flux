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
	err = util.PutRequest(ctx.Config.DaemonURL+"/app/"+projectName.Id+"/start", nil)
	if err != nil {
		return fmt.Errorf("failed to start %s: %v", projectName.Name, err)
	}

	fmt.Printf("Successfully started %s\n", projectName.Name)

	return nil
}
