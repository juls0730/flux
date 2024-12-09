package main

import (
	"archive/tar"
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
	"strconv"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/juls0730/fluxd/pkg"
)

//go:embed config.json
var config []byte

var configPath = filepath.Join(os.Getenv("HOME"), "/.config/flux")

type Config struct {
	DeamonURL string `json:"deamon_url"`
}

func compressDirectory(compression pkg.Compression) ([]byte, error) {
	var buf bytes.Buffer
	var err error

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

		if path == "flux.json" || info.IsDir() {
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

func getProjectName(command string, args []string) (string, error) {
	var projectName string

	if len(args) == 0 {
		if _, err := os.Stat("flux.json"); err != nil {
			return "", fmt.Errorf("Usage: flux %[1]s <app name>, or run flux %[1]s in the project directory\n", command)
		}

		fluxConfigFile, err := os.Open("flux.json")
		if err != nil {
			return "", fmt.Errorf("Failed to open flux.json: %v\n", err)
		}
		defer fluxConfigFile.Close()

		var config pkg.ProjectConfig
		if err := json.NewDecoder(fluxConfigFile).Decode(&config); err != nil {
			return "", fmt.Errorf("Failed to decode flux.json: %v\n", err)
		}

		projectName = config.Name
	} else {
		projectName = args[0]
	}

	return projectName, nil
}

func runCommand(command string, args []string, config Config, info pkg.Info) error {
	loadingSpinner := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
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
		if _, err := os.Stat("flux.json"); err != nil {
			return fmt.Errorf("No flux.json found, please run flux init first\n")
		}

		loadingSpinner.Suffix = " Deploying"
		loadingSpinner.Start()

		buf, err := compressDirectory(info.Compression)
		if err != nil {
			return fmt.Errorf("Failed to compress directory: %v\n", err)
		}

		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		configPart, err := writer.CreateFormFile("config", "flux.json")

		if err != nil {
			return fmt.Errorf("Failed to create config part: %v\n", err)
		}

		fluxConfigFile, err := os.Open("flux.json")
		if err != nil {
			return fmt.Errorf("Failed to open flux.json: %v\n", err)
		}
		defer fluxConfigFile.Close()

		if _, err := io.Copy(configPart, fluxConfigFile); err != nil {
			return fmt.Errorf("Failed to write config part: %v\n", err)
		}

		codePart, err := writer.CreateFormFile("code", "code.tar.gz")
		if err != nil {
			return fmt.Errorf("Failed to create code part: %v\n", err)
		}

		if _, err := codePart.Write(buf); err != nil {
			return fmt.Errorf("Failed to write code part: %v\n", err)
		}

		if err := writer.Close(); err != nil {
			return fmt.Errorf("Failed to close writer: %v\n", err)
		}

		resp, err := http.Post(config.DeamonURL+"/deploy", "multipart/form-data; boundary="+writer.Boundary(), body)
		if err != nil {
			return fmt.Errorf("Failed to send request: %v\n", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %v\n", err)
			}

			if len(responseBody) > 0 && responseBody[len(responseBody)-1] == '\n' {
				responseBody = responseBody[:len(responseBody)-1]
			}

			return fmt.Errorf("Deploy failed: %s\n", responseBody)
		}

		loadingSpinner.Stop()
		fmt.Println("Deployed successfully!")
	case "stop":
		projectName, err := getProjectName(command, args)
		if err != nil {
			return err
		}

		req, err := http.Post(config.DeamonURL+"/stop/"+projectName, "application/json", nil)
		if err != nil {
			return fmt.Errorf("Failed to stop app: %v\n", err)
		}
		defer req.Body.Close()

		if req.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(req.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %v\n", err)
			}

			if len(responseBody) > 0 && responseBody[len(responseBody)-1] == '\n' {
				responseBody = responseBody[:len(responseBody)-1]
			}

			return fmt.Errorf("Stop failed: %s\n", responseBody)
		}

		fmt.Printf("Successfully stopped %s\n", projectName)
	case "start":
		projectName, err := getProjectName(command, args)
		if err != nil {
			return err
		}

		req, err := http.Post(config.DeamonURL+"/start/"+projectName, "application/json", nil)
		if err != nil {
			return fmt.Errorf("Failed to start app: %v\n", err)
		}
		defer req.Body.Close()

		if req.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(req.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %v\n", err)
			}

			if len(responseBody) > 0 && responseBody[len(responseBody)-1] == '\n' {
				responseBody = responseBody[:len(responseBody)-1]
			}

			return fmt.Errorf("Start failed: %s\n", responseBody)
		}

		fmt.Printf("Successfully started %s\n", projectName)
	case "delete":
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
					return fmt.Errorf("Failed to delete deployments: %v\n", err)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return fmt.Errorf("failed to delete deployments: %v", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					responseBody, err := io.ReadAll(resp.Body)
					if err != nil {
						return fmt.Errorf("error reading response body: %v\n", err)
					}

					if len(responseBody) > 0 && responseBody[len(responseBody)-1] == '\n' {
						responseBody = responseBody[:len(responseBody)-1]
					}

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

			if len(responseBody) > 0 && responseBody[len(responseBody)-1] == '\n' {
				responseBody = responseBody[:len(responseBody)-1]
			}

			return fmt.Errorf("delete failed: %s", responseBody)
		}

		fmt.Printf("Successfully deleted %s\n", projectName)
	case "list":
		resp, err := http.Get(config.DeamonURL + "/apps")
		if err != nil {
			return fmt.Errorf("failed to get apps: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return fmt.Errorf("error reading response body: %v", err)
			}

			if len(responseBody) > 0 && responseBody[len(responseBody)-1] == '\n' {
				responseBody = responseBody[:len(responseBody)-1]
			}

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
		return fmt.Errorf("unknown command: %s", command)
	}

	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: flux <command>")
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
