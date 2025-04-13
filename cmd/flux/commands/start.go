package commands

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/juls0730/flux/cmd/flux/models"
)

func StartCommand(ctx models.CommandCtx, args []string) error {
	projectName, err := GetProjectId("start", args, ctx.Config)
	if err != nil {
		return err
	}

	req, err := http.Post(ctx.Config.DeamonURL+"/start/"+projectName, "application/json", nil)
	if err != nil {
		return fmt.Errorf("failed to start app: %v", err)
	}
	defer req.Body.Close()

	if req.StatusCode != http.StatusOK {
		responseBody, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %v", err)
		}

		responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

		return fmt.Errorf("start failed: %s", responseBody)
	}

	fmt.Printf("Successfully started %s\n", projectName)

	return nil
}
