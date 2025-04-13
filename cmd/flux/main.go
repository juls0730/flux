package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/agnivade/levenshtein"
	"github.com/juls0730/flux/cmd/flux/commands"
	"github.com/juls0730/flux/cmd/flux/models"
	"github.com/juls0730/flux/pkg"
)

//go:embed config.json
var config []byte

var configPath = filepath.Join(os.Getenv("HOME"), "/.config/flux")

var version = pkg.Version

var helpStr = `Usage:
  flux <command>

Available Commands:
%s

Available Flags:
  --help, -h: Show this help message

Use "flux <command> --help" for more information about a command.
`

var maxDistance = 3

type CommandFunc func(models.CommandCtx, []string) error

type Command struct {
	Help        string
	HandlerFunc CommandFunc
}

type CommandHandler struct {
	commands map[string]Command
	aliases  map[string]string
}

func NewCommandHandler() CommandHandler {
	return CommandHandler{
		commands: make(map[string]Command),
		aliases:  make(map[string]string),
	}
}

func (h *CommandHandler) RegisterCmd(name string, handler CommandFunc, help string) {
	coomand := Command{
		Help:        help,
		HandlerFunc: handler,
	}

	h.commands[name] = coomand
}

func (h *CommandHandler) RegisterAlias(alias string, command string) {
	h.aliases[alias] = command
}

// returns the command and whether or not it exists
func (h *CommandHandler) GetCommand(command string) (Command, bool) {
	if command, ok := h.aliases[command]; ok {
		return h.commands[command], true
	}

	commandStruct, ok := h.commands[command]
	return commandStruct, ok
}

var helpPadding = 13

func (h *CommandHandler) GetHelp() {
	commandsStr := ""
	for command := range h.commands {
		curLine := ""

		curLine += command
		for alias, aliasCommand := range h.aliases {
			if aliasCommand == command {
				curLine += fmt.Sprintf(", %s", alias)
			}
		}

		curLine += strings.Repeat(" ", helpPadding-(len(curLine)-2))
		commandsStr += fmt.Sprintf("  %s  %s\n", curLine, h.commands[command].Help)
	}

	fmt.Printf(helpStr, strings.TrimRight(commandsStr, "\n"))
}

func (h *CommandHandler) GetHelpCmd(models.CommandCtx, []string) error {
	h.GetHelp()
	return nil
}

func runCommand(command string, args []string, config models.Config, info pkg.Info, cmdHandler CommandHandler) error {
	commandCtx := models.CommandCtx{
		Config: config,
		Info:   info,
	}

	commandStruct, ok := cmdHandler.commands[command]
	if ok {
		return commandStruct.HandlerFunc(commandCtx, args)
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
	// new line ommitted because it will be produced when the user presses enter to submit their response
	fmt.Printf("No command found with the name '%s'. Did you mean '%s'? (y/N)", command, closestMatch.name)
	fmt.Scanln(&response)

	if strings.ToLower(response) == "y" || strings.ToLower(response) == "yes" {
		command = closestMatch.name
	} else {
		return nil
	}

	// re-run command after accepting the suggestion
	return runCommand(command, args, config, info, cmdHandler)
}

func main() {
	cmdHandler := NewCommandHandler()

	cmdHandler.RegisterCmd("init", commands.InitCommand, "Initialize a new project")
	cmdHandler.RegisterCmd("deploy", commands.DeployCommand, "Deploy a new version of the app")
	cmdHandler.RegisterCmd("start", commands.StartCommand, "Start the app")
	cmdHandler.RegisterCmd("stop", commands.StopCommand, "Stop the app")
	cmdHandler.RegisterCmd("list", commands.ListCommand, "List all the apps")
	cmdHandler.RegisterCmd("delete", commands.DeleteCommand, "Delete the app")

	fs := flag.NewFlagSet("flux", flag.ExitOnError)
	fs.Usage = func() {
		cmdHandler.GetHelp()
	}

	err := fs.Parse(os.Args[1:])
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if len(os.Args) < 2 {
		cmdHandler.GetHelp()
		os.Exit(1)
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

	if info.Version != version {
		fmt.Printf("Version mismatch, daemon is running version %s, but you are running version %s\n", info.Version, version)
		os.Exit(1)
	}

	err = runCommand(os.Args[1], fs.Args()[1:], config, info, cmdHandler)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}
