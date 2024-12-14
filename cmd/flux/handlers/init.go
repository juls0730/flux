package handlers

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/briandowns/spinner"
	"github.com/juls0730/flux/cmd/flux/models"
	"github.com/juls0730/flux/pkg"
)

func InitCommand(seekingHelp bool, config models.Config, info pkg.Info, loadingSpinner *spinner.Spinner, spinnerWriter *models.CustomSpinnerWriter, args []string) error {
	if seekingHelp {
		fmt.Println(`Usage:
		  flux init [project-name]
		  
		Options:
		  project-name: The name of the project to initialize
		  
		Flux will initialize a new project in the current directory or the specified project.`)
		return nil
	}

	var projectConfig pkg.ProjectConfig

	var response string
	if len(args) > 1 {
		response = args[0]
	} else {
		fmt.Println("What is the name of your project?")
		fmt.Scanln(&response)
	}

	projectConfig.Name = response

	fmt.Println("What URL should your project listen to?")
	fmt.Scanln(&response)
	if strings.HasPrefix(response, "http") {
		response = strings.TrimPrefix(response, "http://")
		response = strings.TrimPrefix(response, "https://")
	}

	response = strings.Split(response, "/")[0]

	projectConfig.Url = response

	fmt.Println("What port does your project listen to?")
	fmt.Scanln(&response)
	port, err := strconv.ParseUint(response, 10, 16)
	portErr := fmt.Errorf("that doesnt look like a valid port, try a number between 1024 and 65535")
	if port > 65535 {
		return portErr
	}

	projectConfig.Port = uint16(port)
	if err != nil || projectConfig.Port < 1024 {
		return portErr
	}

	configBytes, err := json.MarshalIndent(projectConfig, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to parse project config: %v", err)
	}

	os.WriteFile("flux.json", configBytes, 0644)

	fmt.Printf("Successfully initialized project %s\n", projectConfig.Name)

	return nil
}
