package handlers

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/briandowns/spinner"
	"github.com/juls0730/flux/cmd/flux/models"
	"github.com/juls0730/flux/pkg"
)

func DeleteCommand(seekingHelp bool, config models.Config, info pkg.Info, loadingSpinner *spinner.Spinner, spinnerWriter *models.CustomSpinnerWriter, args []string) error {
	if seekingHelp {
		fmt.Println(`Usage:
		  flux delete [project-name | all]

		Options:
		  project-name: The name of the project to delete
		  all: Delete all projects
		  
		Flux will delete the deployment of the app in the current directory or the specified project.`)
		return nil
	}

	if len(args) == 1 {
		if args[0] == "all" {
			var response string
			fmt.Print("Are you sure you want to delete all projects? this will delete all volumes and containers associated and cannot be undone. \n[y/N] ")
			fmt.Scanln(&response)

			if strings.ToLower(response) != "y" {
				fmt.Println("Aborting...")
				return nil
			}

			response = ""

			fmt.Printf("Are you really sure you want to delete all projects? \n[y/N] ")
			fmt.Scanln(&response)

			if strings.ToLower(response) != "y" {
				fmt.Println("Aborting...")
				return nil
			}

			req, err := http.NewRequest("DELETE", config.DeamonURL+"/deployments", nil)
			if err != nil {
				return fmt.Errorf("failed to delete deployments: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("failed to delete deployments: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				responseBody, err := io.ReadAll(resp.Body)
				if err != nil {
					return fmt.Errorf("error reading response body: %v", err)
				}

				responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

				return fmt.Errorf("delete failed: %s", responseBody)
			}

			fmt.Printf("Successfully deleted all projects\n")
			return nil
		}
	}

	projectName, err := GetProjectName("delete", args)
	if err != nil {
		return err
	}

	// ask for confirmation
	fmt.Printf("Are you sure you want to delete %s? this will delete all volumes and containers associated with the deployment, and cannot be undone. \n[y/N] ", projectName)
	var response string
	fmt.Scanln(&response)

	if strings.ToLower(response) != "y" {
		fmt.Println("Aborting...")
		return nil
	}

	req, err := http.NewRequest("DELETE", config.DeamonURL+"/deployments/"+projectName, nil)
	if err != nil {
		return fmt.Errorf("failed to delete app: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to delete app: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %v", err)
		}

		responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

		return fmt.Errorf("delete failed: %s", responseBody)
	}

	fmt.Printf("Successfully deleted %s\n", projectName)

	return nil
}
