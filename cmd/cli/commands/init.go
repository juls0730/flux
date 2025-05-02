package commands

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/juls0730/flux/pkg"
)

var initUsage = `Usage:
  flux init [project-name]

Options:
  project-name: The name of the project to initialize

Flux will initialize a new project in the current directory or the specified project.`

func InitCommand(ctx CommandCtx, args []string) error {
	if !ctx.Interactive {
		return fmt.Errorf("init command can only be run in interactive mode")
	}

	fs := flag.NewFlagSet("init", flag.ExitOnError)
	fs.Usage = func() {
		var buf bytes.Buffer
		// Redirect flagset to print to buffer instead of stdout
		fs.SetOutput(&buf)
		fs.PrintDefaults()

		fmt.Println(initUsage)
	}

	err := fs.Parse(args)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	args = fs.Args()

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
