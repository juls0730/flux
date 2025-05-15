package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agnivade/levenshtein"
	"github.com/juls0730/flux/cmd/cli/commands"
	util "github.com/juls0730/flux/internal/util/cli"
	"github.com/juls0730/flux/pkg"
	"github.com/juls0730/flux/pkg/API"
	"github.com/mattn/go-isatty"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func isInteractive() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
}

//go:embed config.json
var config []byte

var configPath = filepath.Join(os.Getenv("HOME"), "/.config/flux")

var version = pkg.Version

var helpStr = `Usage:
  flux <command>

Available Commands:
%s

Available Flags:
  -help, -h: Show this help message

Use "flux <command> -help" for more information about a command.
`

var maxDistance = 3

type Command struct {
	Help            string
	DaemonConnected bool
	HandlerFunc     commands.CommandFunc
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

func (h *CommandHandler) RegisterCmd(name string, handler commands.CommandFunc, daemonConnected bool, help string) {
	coomand := Command{
		Help:            help,
		DaemonConnected: daemonConnected,
		HandlerFunc:     handler,
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

func (h *CommandHandler) GetHelpCmd(commands.CommandCtx, []string) error {
	h.GetHelp()
	return nil
}

func runCommand(command string, args []string, config pkg.CLIConfig, cmdHandler CommandHandler, logger *zap.SugaredLogger) error {
	commandStruct, ok := cmdHandler.commands[command]
	if !ok {
		panic("runCommand was passed an invalid command name")
	}

	var info *API.Info = nil

	if commandStruct.DaemonConnected {
		var err error
		info, err = util.GetRequest[API.Info](config.DaemonURL+"/heartbeat", logger)
		if err != nil {
			fmt.Printf("Failed to connect to daemon\n")
			os.Exit(1)
		}

		if info.Version != version {
			fmt.Printf("Version mismatch, daemon is running version %s, but you are running version %s\n", info.Version, version)
			os.Exit(1)
		}
	}

	commandCtx := commands.CommandCtx{
		Config:      config,
		Info:        info,
		Logger:      logger,
		Interactive: isInteractive(),
	}

	return commandStruct.HandlerFunc(commandCtx, args)
}

func main() {
	if !isInteractive() {
		fmt.Printf("Flux is being run non-interactively\n")
	}

	zapConfig := zap.NewDevelopmentConfig()
	verbosity := 0

	debug, err := strconv.ParseBool(os.Getenv("DEBUG"))
	if err != nil {
		debug = false
	}

	if debug {
		zapConfig = zap.NewDevelopmentConfig()
		verbosity = -1
	}

	zapConfig.Level = zap.NewAtomicLevelAt(zapcore.Level(verbosity))

	lameLogger, err := zapConfig.Build()
	if err != nil {
		fmt.Printf("Failed to create logger: %v\n", err)
		os.Exit(1)
	}

	logger := lameLogger.Sugar()

	cmdHandler := NewCommandHandler()

	cmdHandler.RegisterCmd("init", commands.InitCommand, false, "Initialize a new project")
	cmdHandler.RegisterCmd("deploy", commands.DeployCommand, true, "Deploy a new version of the app")
	cmdHandler.RegisterCmd("start", commands.StartCommand, true, "Start the app")
	cmdHandler.RegisterCmd("stop", commands.StopCommand, true, "Stop the app")
	cmdHandler.RegisterCmd("list", commands.ListCommand, true, "List all the apps")
	cmdHandler.RegisterCmd("delete", commands.DeleteCommand, true, "Delete the app")

	fs := flag.NewFlagSet("flux", flag.ExitOnError)
	fs.Usage = func() {
		cmdHandler.GetHelp()
	}

	err = fs.Parse(os.Args[1:])
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

	var config pkg.CLIConfig
	configBytes, err := os.ReadFile(filepath.Join(configPath, "config.json"))
	if err != nil {
		fmt.Printf("Failed to read config file: %v\n", err)
		os.Exit(1)
	}

	if err := json.Unmarshal(configBytes, &config); err != nil {
		fmt.Printf("Failed to parse config file: %v\n", err)
		os.Exit(1)
	}

	if config.DaemonURL == "" {
		fmt.Printf("Daemon URL is empty\n")
		os.Exit(1)
	}

	command := os.Args[1]

	if _, ok := cmdHandler.commands[command]; !ok {
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
			fmt.Printf("unknown command: %s", command)
			os.Exit(1)
		}

		var response string
		// new line ommitted because it will be produced when the user presses enter to submit their response
		fmt.Printf("No command found with the name '%s'. Did you mean '%s'? (y/N)", command, closestMatch.name)
		fmt.Scanln(&response)

		if strings.ToLower(response) == "y" || strings.ToLower(response) == "yes" {
			command = closestMatch.name
		} else {
			os.Exit(0)
		}
	}

	err = runCommand(command, fs.Args()[1:], config, cmdHandler, logger)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}
