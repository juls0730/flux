package handlers

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/juls0730/flux/pkg"
)

func GetProjectName(command string, args []string) (string, error) {
	var projectName string

	if len(args) == 0 {
		if _, err := os.Stat("flux.json"); err != nil {
			return "", fmt.Errorf("usage: flux %[1]s <app name>, or run flux %[1]s in the project directory", command)
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

	return projectName, nil
}
