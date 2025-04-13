package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/juls0730/flux/cmd/flux/models"
	"github.com/juls0730/flux/pkg"
)

func GetProjectId(command string, args []string, config models.Config) (string, error) {
	var projectName string

	if _, err := os.Stat(".fluxid"); err == nil {
		id, err := os.ReadFile(".fluxid")
		if err != nil {
			return "", fmt.Errorf("failed to read .fluxid: %v", err)
		}

		return string(id), nil
	}

	if len(args) == 0 {
		if _, err := os.Stat("flux.json"); err != nil {
			return "", fmt.Errorf("the current directory is not a flux project, please run flux %[1]s in the project directory", command)
		}

		fluxConfigFile, err := os.Open("flux.json")
		if err != nil {
			return "", fmt.Errorf("failed to open flux.json: %v", err)
		}
		defer fluxConfigFile.Close()

		var config pkg.ProjectConfig
		if err := json.NewDecoder(fluxConfigFile).Decode(&config); err != nil {
			return "", fmt.Errorf("failed to decode flux.json: %v", err)
		}

		projectName = config.Name
	} else {
		projectName = args[0]
	}

	// make an http get request to the daemon to get the project name
	resp, err := http.Get(config.DeamonURL + "/apps/by-name/" + projectName)
	if err != nil {
		return "", fmt.Errorf("failed to get project name: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("error reading response body: %v", err)
		}

		responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

		return "", fmt.Errorf("get project name failed: %s", responseBody)
	}

	var app pkg.App
	if err := json.NewDecoder(resp.Body).Decode(&app); err != nil {
		return "", fmt.Errorf("failed to decode app: %v", err)
	}

	return app.Id.String(), nil
}
