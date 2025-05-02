package cli_util

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/juls0730/flux/pkg"
	"github.com/juls0730/flux/pkg/API"
)

type Project struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

func GetProject(command string, args []string, config pkg.CLIConfig) (*Project, error) {
	var projectName string

	// we are in a project directory and the project is deployed
	if _, err := os.Stat(".fluxid"); err == nil {
		id, err := os.ReadFile(".fluxid")
		if err != nil {
			return nil, fmt.Errorf("failed to read .fluxid: %v", err)
		}

		app, err := GetRequest[API.App](config.DaemonURL + "/app/by-id/" + string(id))
		if err != nil {
			return nil, fmt.Errorf("failed to get app: %v", err)
		}

		return &Project{
			Id:   app.Id.String(),
			Name: app.Name,
		}, nil
	}

	// we are calling flux from a project directory, but the project isnt deployed yet
	if len(args) == 0 {
		if _, err := os.Stat("flux.json"); err != nil {
			return nil, fmt.Errorf("the current directory is not a flux project, please run flux %[1]s in the project directory", command)
		}

		fluxConfigFile, err := os.Open("flux.json")
		if err != nil {
			return nil, fmt.Errorf("failed to open flux.json: %v", err)
		}
		defer fluxConfigFile.Close()

		var config pkg.ProjectConfig
		if err := json.NewDecoder(fluxConfigFile).Decode(&config); err != nil {
			return nil, fmt.Errorf("failed to decode flux.json: %v", err)
		}

		projectName = config.Name
	} else {
		projectName = args[0]
	}

	// we are calling flux with a project name (ie `flux start my-project`)
	app, err := GetRequest[API.App](config.DaemonURL + "/app/by-name/" + projectName)
	if err != nil {
		return nil, fmt.Errorf("failed to get app: %v", err)
	}

	return &Project{
		Id:   app.Id.String(),
		Name: app.Name,
	}, nil
}
