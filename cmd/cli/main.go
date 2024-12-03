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
	"path/filepath"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/juls0730/fluxd/models"
)

//go:embed config.json
var config []byte

var configPath = filepath.Join(os.Getenv("HOME"), "/.config/flux")

type Config struct {
	DeamonURL string `json:"deamon_url"`
}

func compressDirectory() ([]byte, error) {
	var buf bytes.Buffer
	gzWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzWriter)

	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
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

		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		if !info.IsDir() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			if _, err := io.Copy(tarWriter, file); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gzWriter.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: flux <command>")
		os.Exit(1)
	}

	command := os.Args[1]

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

	switch command {
	case "deploy":
		loadingSpinner := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
		loadingSpinner.Suffix = " Deploying"
		loadingSpinner.Start()

		buf, err := compressDirectory()
		if err != nil {
			loadingSpinner.Stop()

			fmt.Printf("Failed to compress directory: %v\n", err)
			os.Exit(1)
		}

		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)
		configPart, err := writer.CreateFormFile("config", "flux.json")

		if err != nil {
			loadingSpinner.Stop()

			fmt.Printf("Failed to create config part: %v\n", err)
			os.Exit(1)
		}

		fluxConfigFile, err := os.Open("flux.json")
		if err != nil {
			loadingSpinner.Stop()

			fmt.Printf("Failed to open flux.json: %v\n", err)
			os.Exit(1)
		}
		defer fluxConfigFile.Close()

		if _, err := io.Copy(configPart, fluxConfigFile); err != nil {
			loadingSpinner.Stop()

			fmt.Printf("Failed to write config part: %v\n", err)
			os.Exit(1)
		}

		codePart, err := writer.CreateFormFile("code", "code.tar.gz")
		if err != nil {
			loadingSpinner.Stop()

			fmt.Printf("Failed to create code part: %v\n", err)
			os.Exit(1)
		}

		if _, err := codePart.Write(buf); err != nil {
			loadingSpinner.Stop()

			fmt.Printf("Failed to write code part: %v\n", err)
			os.Exit(1)
		}

		if err := writer.Close(); err != nil {
			loadingSpinner.Stop()

			fmt.Printf("Failed to close writer: %v\n", err)
			os.Exit(1)
		}

		resp, err := http.Post(config.DeamonURL+"/deploy", "multipart/form-data; boundary="+writer.Boundary(), body)
		if err != nil {
			loadingSpinner.Stop()

			fmt.Printf("Failed to send request: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			loadingSpinner.Stop()

			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Printf("error reading response body: %v\n", err)
				os.Exit(1)
			}

			if len(responseBody) > 0 && responseBody[len(responseBody)-1] == '\n' {
				responseBody = responseBody[:len(responseBody)-1]
			}

			fmt.Printf("Deploy failed: %s\n", responseBody)
			os.Exit(1)
		}

		loadingSpinner.Stop()
		fmt.Println("Deployed successfully!")
	case "delete":
		var projectName string

		if len(os.Args) < 3 {
			if _, err := os.Stat("flux.json"); err != nil {
				fmt.Printf("Usage: flux delete <app name>, or run flux delete in the project directory\n")
				os.Exit(1)
			}

			fluxConfigFile, err := os.Open("flux.json")
			if err != nil {
				fmt.Printf("Failed to open flux.json: %v\n", err)
				os.Exit(1)
			}
			defer fluxConfigFile.Close()

			var config models.ProjectConfig
			if err := json.NewDecoder(fluxConfigFile).Decode(&config); err != nil {
				fmt.Printf("Failed to decode flux.json: %v\n", err)
				os.Exit(1)
			}

			projectName = config.Name
		} else {
			projectName = os.Args[2]
		}

		// ask for confirmation
		fmt.Printf("Are you sure you want to delete %s? this will delete all volumes and containers associated with the deployment, and cannot be undone. \n[y/N]", projectName)
		var response string
		fmt.Scanln(&response)

		if strings.ToLower(response) != "y" {
			fmt.Println("Aborting...")
			os.Exit(0)
		}

		req, err := http.NewRequest("DELETE", config.DeamonURL+"/deploy/"+projectName, nil)
		if err != nil {
			fmt.Printf("Failed to delete app: %v\n", err)
			os.Exit(1)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Printf("Failed to delete app: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Printf("error reading response body: %v\n", err)
				os.Exit(1)
			}

			if len(responseBody) > 0 && responseBody[len(responseBody)-1] == '\n' {
				responseBody = responseBody[:len(responseBody)-1]
			}

			fmt.Printf("Delete failed: %s\n", responseBody)
			os.Exit(1)
		}

		fmt.Printf("Successfully deleted %s\n", projectName)
	case "list":
		resp, err := http.Get(config.DeamonURL + "/apps")
		if err != nil {
			fmt.Printf("Failed to get apps: %v\n", err)
			os.Exit(1)
		}

		var apps []models.App
		if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
			fmt.Printf("Failed to decode apps: %v\n", err)
			os.Exit(1)
		}

		for _, app := range apps {
			fmt.Printf("%s\n", app.Name)
		}
	default:
		fmt.Println("Unknown command:", command)
	}
}
