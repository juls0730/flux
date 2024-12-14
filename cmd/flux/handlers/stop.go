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

func StopCommand(seekingHelp bool, config models.Config, info pkg.Info, loadingSpinner *spinner.Spinner, spinnerWriter *models.CustomSpinnerWriter, args []string) error {
	if seekingHelp {
		fmt.Println(`Usage:
		  flux stop
		  
		Flux will stop the deployment of the app in the current directory.`)
		return nil
	}

	projectName, err := GetProjectName("stop", args)
	if err != nil {
		return err
	}

	req, err := http.Post(config.DeamonURL+"/stop/"+projectName, "application/json", nil)
	if err != nil {
		return fmt.Errorf("failed to stop app: %v", err)
	}
	defer req.Body.Close()

	if req.StatusCode != http.StatusOK {
		responseBody, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %v", err)
		}

		responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

		return fmt.Errorf("stop failed: %s", responseBody)
	}

	fmt.Printf("Successfully stopped %s\n", projectName)
	return nil
}
