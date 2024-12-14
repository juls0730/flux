package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/agnivade/levenshtein"
	"github.com/briandowns/spinner"
	"github.com/juls0730/flux/cmd/flux/handlers"
	"github.com/juls0730/flux/cmd/flux/models"
	"github.com/juls0730/flux/pkg"
)

//go:embed config.json
var config []byte

var configPath = filepath.Join(os.Getenv("HOME"), "/.config/flux")

var helpStr = `Usage:
  flux <command>

Available Commands:
  init        Initialize a new project
  deploy      Deploy a new version of the app
  stop        Stop a container
  start       Start a container
  delete      Delete a container
  list        List all containers

Flags:
  -h, --help   help for flux

Use "flux <command> --help" for more information about a command.`

var maxDistance = 3

type CommandHandler struct {
	commands map[string]func(bool, models.Config, pkg.Info, *spinner.Spinner, *models.CustomSpinnerWriter, []string) error
}

func (h *CommandHandler) RegisterCmd(name string, handler func(bool, models.Config, pkg.Info, *spinner.Spinner, *models.CustomSpinnerWriter, []string) error) {
	h.commands[name] = handler
}

func runCommand(command string, args []string, config models.Config, info pkg.Info, cmdHandler CommandHandler, try int) error {
	if try == 2 {
		return fmt.Errorf("unknown command: %s", command)
	}

	seekingHelp := false
	if len(args) > 0 && (args[len(args)-1] == "--help" || args[len(args)-1] == "-h") {
		seekingHelp = true
		args = args[:len(args)-1]
	}

	spinnerWriter := models.NewCustomSpinnerWriter()

	loadingSpinner := spinner.New(spinner.CharSets[14], 100*time.Millisecond, spinner.WithWriter(spinnerWriter))
	defer func() {
		if loadingSpinner.Active() {
			loadingSpinner.Stop()
		}
	}()

	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, os.Interrupt)
	go func() {
		<-signalChannel
		if loadingSpinner.Active() {
			loadingSpinner.Stop()
		}

		os.Exit(0)
	}()

	handler, ok := cmdHandler.commands[command]
	if ok {
		return handler(seekingHelp, config, info, loadingSpinner, spinnerWriter, args)
	}

	// diff the command against the list of commands and if we find a command that is more than 80% similar, ask if that's what the user meant
	var closestMatch struct {
		name  string
		score int
	}
	for cmdName := range cmdHandler.commands {
		distance := levenshtein.ComputeDistance(cmdName, command)

		if distance <= maxDistance {
			if closestMatch.name == "" || distance < closestMatch.score {
				closestMatch.name = cmdName
				closestMatch.score = distance
			}
		}
	}

	if closestMatch.name == "" {
		return fmt.Errorf("unknown command: %s", command)
	}

	var response string
	fmt.Printf("No command found with the name '%s'. Did you mean '%s'?\n", command, closestMatch.name)
	fmt.Scanln(&response)

	if strings.ToLower(response) == "y" || strings.ToLower(response) == "yes" {
		command = closestMatch.name
	} else {
		return nil
	}

	return runCommand(command, args, config, info, cmdHandler, try+1)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println(helpStr)
		os.Exit(1)
	}

	if os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Println(helpStr)
		os.Exit(0)
	}

	if _, err := os.Stat(filepath.Join(configPath, "config.json")); err != nil {
		if err := os.MkdirAll(configPath, 0755); err != nil {
			fmt.Printf("Failed to create config directory: %v\n", err)
			os.Exit(1)
		}

		if err = os.WriteFile(filepath.Join(configPath, "config.json"), config, 0644); err != nil {
			fmt.Printf("Failed to write config file: %v\n", err)
			os.Exit(1)
		}
	}

	var config models.Config
	configBytes, err := os.ReadFile(filepath.Join(configPath, "config.json"))
	if err != nil {
		fmt.Printf("Failed to read config file: %v\n", err)
		os.Exit(1)
	}

	if err := json.Unmarshal(configBytes, &config); err != nil {
		fmt.Printf("Failed to parse config file: %v\n", err)
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]

	resp, err := http.Get(config.DeamonURL + "/heartbeat")
	if err != nil {
		fmt.Println("Failed to connect to daemon")
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Println("Failed to connect to daemon")
		os.Exit(1)
	}

	var info pkg.Info
	err = json.NewDecoder(resp.Body).Decode(&info)
	if err != nil {
		fmt.Printf("Failed to decode info: %v\n", err)
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		fmt.Println("Failed to connect to daemon")
		os.Exit(1)
	}

	cmdHandler := CommandHandler{
		commands: make(map[string]func(bool, models.Config, pkg.Info, *spinner.Spinner, *models.CustomSpinnerWriter, []string) error),
	}

	cmdHandler.RegisterCmd("deploy", handlers.DeployCommand)
	cmdHandler.RegisterCmd("stop", handlers.StopCommand)
	cmdHandler.RegisterCmd("start", handlers.StartCommand)
	cmdHandler.RegisterCmd("delete", handlers.DeleteCommand)
	cmdHandler.RegisterCmd("init", handlers.InitCommand)

	err = runCommand(command, args, config, info, cmdHandler, 0)
	if err != nil {
		fmt.Printf("%v\n", err)
		os.Exit(1)
	}
}
