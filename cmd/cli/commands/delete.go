package commands

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"

	util "github.com/juls0730/flux/internal/util/cli"
)

var deleteUsage = `Usage:
  flux delete [project-name | all]

Options:
  project-name: The name of the project to delete
  all: Delete all projects

Flags:
%s

Flux will delete the deployment of the app in the current directory or the specified project.`

func deleteAll(ctx CommandCtx, noConfirm *bool) error {
	if !*noConfirm {
		if !ctx.Interactive {
			return fmt.Errorf("delete command cannot be run non-interactively without --no-confirm")
		}

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

	util.DeleteRequest(ctx.Config.DaemonURL+"/deployments", ctx.Logger)

	fmt.Printf("Successfully deleted all projects\n")
	return nil
}

func DeleteCommand(ctx CommandCtx, args []string) error {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	fs.Usage = func() {
		var buf bytes.Buffer
		// Redirect flagset to print to buffer instead of stdout
		fs.SetOutput(&buf)
		fs.PrintDefaults()

		fmt.Println(deleteUsage, strings.TrimRight(buf.String(), "\n"))
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

	project, err := util.GetProject("delete", args, ctx.Config, ctx.Logger)
	if err != nil {
		return fmt.Errorf("\tfailed to get project name: %v.\n\tSee flux delete -help for more information", err)
	}

	// ask for confirmation if not --no-confirm
	if !*noConfirm {
		if !ctx.Interactive {
			return fmt.Errorf("delete command cannot be run non-interactively without --no-confirm")
		}

		fmt.Printf("Are you sure you want to delete %s? this will delete all volumes and containers associated with the deployment, and cannot be undone. \n[y/N] ", project.Name)
		var response string
		fmt.Scanln(&response)

		if strings.ToLower(response) != "y" {
			fmt.Println("Aborting...")
			return nil
		}
	}

	err = util.DeleteRequest(ctx.Config.DaemonURL+"/app/"+project.Id, ctx.Logger)
	if err != nil {
		return fmt.Errorf("failed to delete project: %v", err)
	}

	if len(args) == 0 {
		// remove the .fluxid file if it exists
		os.Remove(".fluxid")
	}

	fmt.Printf("Successfully deleted %s\n", project.Name)

	return nil
}
