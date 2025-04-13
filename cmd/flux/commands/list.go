package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/juls0730/flux/cmd/flux/models"
	"github.com/juls0730/flux/pkg"
)

func ListCommand(ctx models.CommandCtx, args []string) error {
	resp, err := http.Get(ctx.Config.DeamonURL + "/apps")
	if err != nil {
		return fmt.Errorf("failed to get apps: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %v", err)
		}

		responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

		return fmt.Errorf("list failed: %s", responseBody)
	}

	var apps []pkg.App
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return fmt.Errorf("failed to decode apps: %v", err)
	}

	if len(apps) == 0 {
		fmt.Println("No apps found")
		return nil
	}

	for _, app := range apps {
		fmt.Printf("%s (%s)\n", app.Name, app.DeploymentStatus)
	}

	return nil
}
