package commands

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/juls0730/flux/cmd/flux/models"
)

var usage = `Usage:
  flux delete [project-name | all]

Options:
  project-name: The name of the project to delete
  all: Delete all projects

Flags:
%s

Flux will delete the deployment of the app in the current directory or the specified project.
`

func deleteAll(ctx models.CommandCtx, noConfirm *bool) error {
	if !*noConfirm {
		var response string
		fmt.Print("Are you sure you want to delete all projects? this will delete all volumes and containers associated and cannot be undone. [y/N] ")
		fmt.Scanln(&response)

		if strings.ToLower(response) != "y" {
			fmt.Println("Aborting...")
			return nil
		}

		response = ""

		// since we are deleting **all** projects, I feel better asking for confirmation twice
		fmt.Printf("Are you really sure you want to delete all projects? [y/N] ")
		fmt.Scanln(&response)

		if strings.ToLower(response) != "y" {
			fmt.Println("Aborting...")
			return nil
		}
	}

	req, err := http.NewRequest("DELETE", ctx.Config.DeamonURL+"/deployments", nil)
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

func DeleteCommand(ctx models.CommandCtx, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	fs.Usage = func() {
		var buf bytes.Buffer
		// Redirect flagset to print to buffer instead of stdout
		fs.SetOutput(&buf)
		fs.PrintDefaults()

		fmt.Printf(usage, strings.TrimRight(buf.String(), "\n"))
	}

	noConfirm := fs.Bool("no-confirm", false, "Skip confirmation prompt")

	err := fs.Parse(args)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	args = fs.Args()

	if len(args) == 1 && args[0] == "all" {
		return deleteAll(ctx, noConfirm)
	}

	projectName, err := GetProjectId("delete", args, ctx.Config)
	if err != nil {
		return fmt.Errorf("\tfailed to get project name: %v.\n\tSee flux delete --help for more information", err)
	}

	// ask for confirmation
	if !*noConfirm {
		fmt.Printf("Are you sure you want to delete %s? this will delete all volumes and containers associated with the deployment, and cannot be undone. \n[y/N] ", projectName)
		var response string
		fmt.Scanln(&response)

		if strings.ToLower(response) != "y" {
			fmt.Println("Aborting...")
			return nil
		}
	}

	req, err := http.NewRequest("DELETE", ctx.Config.DeamonURL+"/deployments/"+projectName, nil)
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

	if len(args) == 0 {
		// remove the .fluxid file if it exists
		os.Remove(".fluxid")
	}

	fmt.Printf("Successfully deleted %s\n", projectName)

	return nil
}
