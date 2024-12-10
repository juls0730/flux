package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/briandowns/spinner"
	"github.com/juls0730/flux/pkg"
)

//go:embed config.json
var config []byte

var configPath = filepath.Join(os.Getenv("HOME"), "/.config/flux")

type Config struct {
	DeamonURL string `json:"deamon_url"`
}

func matchesIgnorePattern(path string, info os.FileInfo, patterns []string) bool {
	normalizedPath := filepath.ToSlash(path)
	normalizedPath = strings.TrimPrefix(normalizedPath, "./")

	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}

		regexPattern := convertGitignorePatternToRegex(pattern)

		matched, err := regexp.MatchString(regexPattern, normalizedPath)
		if err == nil && matched {
			if strings.HasSuffix(pattern, "/") && info.IsDir() {
				return true
			}
			if !info.IsDir() {
				dir := filepath.Dir(normalizedPath)
				for dir != "." && dir != "/" {
					dirPattern := convertGitignorePatternToRegex(pattern)
					if matched, _ := regexp.MatchString(dirPattern, filepath.ToSlash(dir)); matched {
						return true
					}
					dir = filepath.Dir(dir)
				}
			}
			return true
		}
	}
	return false
}

func convertGitignorePatternToRegex(pattern string) string {
	pattern = strings.TrimSuffix(pattern, "/")
	pattern = regexp.QuoteMeta(pattern)
	pattern = strings.ReplaceAll(pattern, "\\*\\*", ".*")
	pattern = strings.ReplaceAll(pattern, "\\*", "[^/]*")
	pattern = strings.ReplaceAll(pattern, "\\?", ".")
	pattern = "(^|.*/)" + pattern + "(/.*)?$"

	return pattern
}

func compressDirectory(compression pkg.Compression) ([]byte, error) {
	var buf bytes.Buffer
	var err error

	var ignoredFiles []string
	fluxIgnore, err := os.Open(".fluxignore")
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	}

	if fluxIgnore != nil {
		defer fluxIgnore.Close()

		scanner := bufio.NewScanner(fluxIgnore)
		for scanner.Scan() {
			ignoredFiles = append(ignoredFiles, scanner.Text())
		}
	}

	var gzWriter *gzip.Writer
	if compression.Enabled {
		gzWriter, err = gzip.NewWriterLevel(&buf, compression.Level)
		if err != nil {
			return nil, err
		}
	}

	var tarWriter *tar.Writer
	if gzWriter != nil {
		tarWriter = tar.NewWriter(gzWriter)
	} else {
		tarWriter = tar.NewWriter(&buf)
	}

	err = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if path == "flux.json" || info.IsDir() || matchesIgnorePattern(path, info, ignoredFiles) {
			return nil
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = path

		if err = tarWriter.WriteHeader(header); err != nil {
			return err
		}

		if !info.IsDir() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			if _, err = io.Copy(tarWriter, file); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if err = tarWriter.Close(); err != nil {
		return nil, err
	}

	if gzWriter != nil {
		if err = gzWriter.Close(); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func arrayContains(arr []string, str string) bool {
	for _, a := range arr {
		if a == str {
			return true
		}
	}
	return false
}

func getProjectName(command string, args []string) (string, error) {
	var projectName string

	if len(args) == 0 {
		if _, err := os.Stat("flux.json"); err != nil {
			return "", fmt.Errorf("Usage: flux %[1]s <app name>, or run flux %[1]s in the project directory", command)
		}

		fluxConfigFile, err := os.Open("flux.json")
		if err != nil {
			return "", fmt.Errorf("Failed to open flux.json: %v", err)
		}
		defer fluxConfigFile.Close()

		var config pkg.ProjectConfig
		if err := json.NewDecoder(fluxConfigFile).Decode(&config); err != nil {
			return "", fmt.Errorf("Failed to decode flux.json: %v", err)
		}

		projectName = config.Name
	} else {
		projectName = args[0]
	}

	return projectName, nil
}

type CustomSpinnerWriter struct {
	currentSpinnerMsg string
	lock              sync.Mutex
}

func (w *CustomSpinnerWriter) Write(p []byte) (n int, err error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	n, err = os.Stdout.Write(p)
	if err != nil {
		return n, err
	}

	w.currentSpinnerMsg = string(p)

	return len(p), nil
}

type CustomStdout struct {
	spinner *CustomSpinnerWriter
	lock    sync.Mutex
}

func (w *CustomStdout) Write(p []byte) (n int, err error) {
	w.lock.Lock()
	defer w.lock.Unlock()

	n, err = os.Stdout.Write([]byte(fmt.Sprintf("\033[2K\r%s", p)))
	if err != nil {
		return n, err
	}

	nn, err := os.Stdout.Write([]byte(w.spinner.currentSpinnerMsg))
	if err != nil {
		return n, err
	}

	n = nn + n

	return n, nil
}

func (w *CustomStdout) Printf(format string, a ...interface{}) (n int, err error) {
	str := fmt.Sprintf(format, a...)
	return w.Write([]byte(str))
}

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

func runCommand(command string, args []string, config Config, info pkg.Info) error {
	seekingHelp := false
	if len(args) > 0 && (args[len(args)-1] == "--help" || args[len(args)-1] == "-h") {
		seekingHelp = true
		args = args[:len(args)-1]
	}

	spinnerWriter := CustomSpinnerWriter{
		currentSpinnerMsg: "",
		lock:              sync.Mutex{},
	}

	loadingSpinner := spinner.New(spinner.CharSets[14], 100*time.Millisecond, spinner.WithWriter(&spinnerWriter))
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

	switch command {
	case "deploy":
		if seekingHelp {
			fmt.Println(`Usage:
			  flux deploy
			  
			Flux will deploy the app in the current directory, and start routing traffic to it.`)
			return nil
		}

		if _, err := os.Stat("flux.json"); err != nil {
			return fmt.Errorf("No flux.json found, please run flux init first")
		}

		loadingSpinner.Suffix = " Deploying"
		loadingSpinner.Start()

		buf, err := compressDirectory(info.Compression)
		if err != nil {
			return fmt.Errorf("Failed to compress directory: %v", err)
		}

		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		configPart, err := writer.CreateFormFile("config", "flux.json")

		if err != nil {
			return fmt.Errorf("Failed to create config part: %v", err)
		}

		fluxConfigFile, err := os.Open("flux.json")
		if err != nil {
			return fmt.Errorf("Failed to open flux.json: %v", err)
		}
		defer fluxConfigFile.Close()

		if _, err := io.Copy(configPart, fluxConfigFile); err != nil {
			return fmt.Errorf("Failed to write config part: %v", err)
		}

		codePart, err := writer.CreateFormFile("code", "code.tar.gz")
		if err != nil {
			return fmt.Errorf("Failed to create code part: %v", err)
		}

		if _, err := codePart.Write(buf); err != nil {
			return fmt.Errorf("Failed to write code part: %v", err)
		}

		if err := writer.Close(); err != nil {
			return fmt.Errorf("Failed to close writer: %v", err)
		}

		req, err := http.NewRequest("POST", config.DeamonURL+"/deploy", body)
		req.Header.Set("Content-Type", writer.FormDataContentType())

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("Failed to send request: %v", err)
		}
		defer resp.Body.Close()

		customWriter := &CustomStdout{
			spinner: &spinnerWriter,
			lock:    sync.Mutex{},
		}

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var event pkg.DeploymentEvent
				if err := json.Unmarshal([]byte(line[6:]), &event); err == nil {
					switch event.Stage {
					case "complete":
						loadingSpinner.Stop()
						var deploymentResponse struct {
							App pkg.App `json:"app"`
						}
						if err := json.Unmarshal([]byte(event.Message), &deploymentResponse); err != nil {
							return fmt.Errorf("Failed to parse deployment response: %v", err)
						}

						fmt.Printf("App %s deployed successfully!\n", deploymentResponse.App.Name)

						return nil
					case "cmd_output":
						customWriter.Printf("... %s\n", event.Message)
					case "error":
						loadingSpinner.Stop()
						return fmt.Errorf("Deployment failed: %s\n", event.Error)
					default:
						customWriter.Printf("%s\n", event.Message)
					}
				}
			}
		}

		if resp.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %v", err)
			}

			responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

			return fmt.Errorf("Deploy failed: %s", responseBody)
		}
	case "stop":
		if seekingHelp {
			fmt.Println(`Usage:
			  flux stop
			  
			Flux will stop the deployment of the app in the current directory.`)
			return nil
		}

		projectName, err := getProjectName(command, args)
		if err != nil {
			return err
		}

		req, err := http.Post(config.DeamonURL+"/stop/"+projectName, "application/json", nil)
		if err != nil {
			return fmt.Errorf("Failed to stop app: %v", err)
		}
		defer req.Body.Close()

		if req.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(req.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %v", err)
			}

			responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

			return fmt.Errorf("Stop failed: %s", responseBody)
		}

		fmt.Printf("Successfully stopped %s\n", projectName)
	case "start":
		if seekingHelp {
			fmt.Println(`Usage:
			  flux start
			  
			Flux will start the deployment of the app in the current directory.`)
			return nil
		}

		projectName, err := getProjectName(command, args)
		if err != nil {
			return err
		}

		req, err := http.Post(config.DeamonURL+"/start/"+projectName, "application/json", nil)
		if err != nil {
			return fmt.Errorf("Failed to start app: %v", err)
		}
		defer req.Body.Close()

		if req.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(req.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %v", err)
			}

			responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

			return fmt.Errorf("Start failed: %s", responseBody)
		}

		fmt.Printf("Successfully started %s\n", projectName)
	case "delete":
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
					return fmt.Errorf("Failed to delete deployments: %v", err)
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

		projectName, err := getProjectName(command, args)
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
	case "list":
		if seekingHelp {
			fmt.Println(`Usage:
			  flux list

			Flux will list all the apps in the daemon.`)
			return nil
		}

		resp, err := http.Get(config.DeamonURL + "/apps")
		if err != nil {
			return fmt.Errorf("failed to get apps: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %v", err)
			}

			responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

			return fmt.Errorf("list failed: %s", responseBody)
		}

		var apps []pkg.App
		if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
			return fmt.Errorf("failed to decode apps: %v", err)
		}

		if len(apps) == 0 {
			fmt.Println("No apps found")
			return nil
		}

		for _, app := range apps {
			fmt.Printf("%s (%s)\n", app.Name, app.DeploymentStatus)
		}
	case "init":
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
			strings.TrimPrefix(response, "http://")
			strings.TrimPrefix(response, "https://")
		}

		response = strings.Split(response, "/")[0]

		projectConfig.Url = response

		fmt.Println("What port does your project listen to?")
		fmt.Scanln(&response)
		port, err := strconv.ParseUint(response, 10, 16)
		projectConfig.Port = uint16(port)
		if err != nil || projectConfig.Port < 1 || projectConfig.Port > 65535 {
			return fmt.Errorf("That doesnt look like a valid port, try a number between 1 and 65535")
		}

		configBytes, err := json.MarshalIndent(projectConfig, "", "    ")
		if err != nil {
			return fmt.Errorf("failed to parse project config: %v", err)
		}

		os.WriteFile("flux.json", configBytes, 0644)

		fmt.Printf("Successfully initialized project %s\n", projectConfig.Name)
	default:
		return fmt.Errorf("unknown command: %s\n%s", command, helpStr)
	}

	return nil
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

	var config Config
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

	err = runCommand(command, args, config, info)
	if err != nil {
		fmt.Printf("%v\n", err)
		os.Exit(1)
	}
}
