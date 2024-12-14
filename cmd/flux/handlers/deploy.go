package handlers

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/briandowns/spinner"
	"github.com/juls0730/flux/cmd/flux/models"
	"github.com/juls0730/flux/pkg"
)

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

func DeployCommand(seekingHelp bool, config models.Config, info pkg.Info, loadingSpinner *spinner.Spinner, spinnerWriter *models.CustomSpinnerWriter, args []string) error {
	if seekingHelp {
		fmt.Println(`Usage:
		  flux deploy
		  
		Flux will deploy the app in the current directory, and start routing traffic to it.`)
		return nil
	}

	if _, err := os.Stat("flux.json"); err != nil {
		return fmt.Errorf("no flux.json found, please run flux init first")
	}

	loadingSpinner.Suffix = " Deploying"
	loadingSpinner.Start()

	buf, err := compressDirectory(info.Compression)
	if err != nil {
		return fmt.Errorf("failed to compress directory: %v", err)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	configPart, err := writer.CreateFormFile("config", "flux.json")

	if err != nil {
		return fmt.Errorf("failed to create config part: %v", err)
	}

	fluxConfigFile, err := os.Open("flux.json")
	if err != nil {
		return fmt.Errorf("failed to open flux.json: %v", err)
	}
	defer fluxConfigFile.Close()

	if _, err := io.Copy(configPart, fluxConfigFile); err != nil {
		return fmt.Errorf("failed to write config part: %v", err)
	}

	codePart, err := writer.CreateFormFile("code", "code.tar.gz")
	if err != nil {
		return fmt.Errorf("failed to create code part: %v", err)
	}

	if _, err := codePart.Write(buf); err != nil {
		return fmt.Errorf("failed to write code part: %v", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close writer: %v", err)
	}

	req, err := http.NewRequest("POST", config.DeamonURL+"/deploy", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	customWriter := models.NewCustomStdout(spinnerWriter)

	scanner := bufio.NewScanner(resp.Body)
	var event string
	var data pkg.DeploymentEvent
	var line string
	for scanner.Scan() {
		line = scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(line[6:]), &data); err != nil {
				return fmt.Errorf("failed to parse deployment event: %v", err)
			}

			switch event {
			case "complete":
				loadingSpinner.Stop()
				fmt.Printf("App %s deployed successfully!\n", data.Message.(map[string]interface{})["name"])
				return nil
			case "cmd_output":
				customWriter.Printf("... %s\n", data.Message)
			case "error":
				loadingSpinner.Stop()
				return fmt.Errorf("deployment failed: %s", data.Message)
			default:
				customWriter.Printf("%s\n", data.Message)
			}
			event = ""
		} else if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
	}

	// the stream closed, but we didnt get a "complete" event
	line = strings.TrimSuffix(line, "\n")
	return fmt.Errorf("deploy failed: %s", line)
}
