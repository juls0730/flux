package commands

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	util "github.com/juls0730/flux/internal/util/cli"
	"github.com/juls0730/flux/pkg"
	"github.com/juls0730/flux/pkg/API"
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

func compressDirectory(compressionLevel int) ([]byte, error) {
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
	if compressionLevel > 0 {
		gzWriter, err = gzip.NewWriterLevel(&buf, compressionLevel)
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

func preprocessEnvFile(envFile string, target *[]string) error {
	envBytes, err := os.Open(envFile)
	if err != nil {
		return fmt.Errorf("failed to open env file: %v", err)
	}
	defer envBytes.Close()

	envVars, err := godotenv.Parse(envBytes)
	if err != nil {
		return fmt.Errorf("failed to parse env file: %v", err)
	}

	for key, value := range envVars {
		*target = append(*target, fmt.Sprintf("%s=%s", key, value))
	}

	return nil
}

var deployUsage = `Usage:
  flux deploy [flags]

Flags:
  -help, -h: Show this help message
%s

Flux will deploy or redeploy the app in the current directory.
`

func DeployCommand(ctx CommandCtx, args []string) error {
	if _, err := os.Stat("flux.json"); err != nil {
		return fmt.Errorf("no flux.json found, please run flux init first")
	}

	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	fs.Usage = func() {
		var buf bytes.Buffer
		// Redirect flagset to print to buffer instead of stdout
		fs.SetOutput(&buf)
		fs.PrintDefaults()

		fmt.Println(deployUsage, strings.TrimRight(buf.String(), "\n"))
	}

	quiet := fs.Bool("q", false, "Don't print the deployment logs")

	err := fs.Parse(args)
	if err != nil {
		return err
	}

	spinnerWriter := util.NewCustomSpinnerWriter()

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

	loadingSpinner.Suffix = " Deploying"
	loadingSpinner.Start()

	buf, err := compressDirectory(ctx.Info.CompressionLevel)
	if err != nil {
		return fmt.Errorf("failed to compress directory: %v", err)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if _, err := os.Stat(".fluxid"); err == nil {
		idPart, err := writer.CreateFormField("id")
		if err != nil {
			return fmt.Errorf("failed to create id part: %v", err)
		}

		idFile, err := os.Open(".fluxid")
		if err != nil {
			return fmt.Errorf("failed to open .fluxid: %v", err)
		}
		defer idFile.Close()

		var idBytes []byte
		if idBytes, err = io.ReadAll(idFile); err != nil {
			return fmt.Errorf("failed to read .fluxid: %v", err)
		}

		if _, err := uuid.Parse(string(idBytes)); err != nil {
			return fmt.Errorf(".fluxid does not contain a valid uuid")
		}

		idPart.Write(idBytes)
	}

	configPart, err := writer.CreateFormField("config")

	if err != nil {
		return fmt.Errorf("failed to create config part: %v", err)
	}

	type FluxContainers struct {
		pkg.Container
		EnvFile string `json:"env_file,omitempty"`
	}

	type FluxConfig struct {
		pkg.ProjectConfig
		EnvFile    string           `json:"env_file,omitempty"`
		Containers []FluxContainers `json:"containers,omitempty"`
	}

	fluxConfigFile, err := os.Open("flux.json")
	if err != nil {
		return fmt.Errorf("failed to open flux.json: %v", err)
	}
	defer fluxConfigFile.Close()

	// Read the entire JSON file into a byte slice
	byteValue, err := io.ReadAll(fluxConfigFile)
	if err != nil {
		return fmt.Errorf("failed to read flux.json: %v", err)
	}

	var fluxConfig FluxConfig
	err = json.Unmarshal(byteValue, &fluxConfig)
	if err != nil {
		return fmt.Errorf("failed to unmarshal flux.json: %v", err)
	}

	if fluxConfig.EnvFile != "" {
		if err := preprocessEnvFile(fluxConfig.EnvFile, &fluxConfig.Environment); err != nil {
			return fmt.Errorf("failed to preprocess env file: %v", err)
		}
	}

	for _, container := range fluxConfig.Containers {
		if container.EnvFile != "" {
			if err := preprocessEnvFile(container.EnvFile, &container.Environment); err != nil {
				return fmt.Errorf("failed to preprocess env file: %v", err)
			}
		}
	}

	// write the pre-processed flux.json to the config part
	if err := json.NewEncoder(configPart).Encode(fluxConfig); err != nil {
		return fmt.Errorf("failed to encode flux.json: %v", err)
	}

	var codeFileName string
	if ctx.Info.CompressionLevel > 0 {
		codeFileName = "code.tar.gz"
	} else {
		codeFileName = "code.tar"
	}

	codePart, err := writer.CreateFormFile("code", codeFileName)
	if err != nil {
		return fmt.Errorf("failed to create code part: %v", err)
	}

	if _, err := codePart.Write(buf); err != nil {
		return fmt.Errorf("failed to write code part: %v", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("failed to close writer: %v", err)
	}

	req, err := http.NewRequest("POST", ctx.Config.DaemonURL+"/deploy", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %v", err)
	}
	defer resp.Body.Close()

	customWriter := util.NewCustomStdout(spinnerWriter)

	scanner := bufio.NewScanner(resp.Body)
	var event string
	var data API.DeploymentEvent
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
				fmt.Printf("App %s deployed successfully!\n", data.Message.(map[string]any)["name"])
				if _, err := os.Stat(".fluxid"); os.IsNotExist(err) {
					idFile, err := os.Create(".fluxid")
					if err != nil {
						return fmt.Errorf("failed to create .fluxid: %v", err)
					}
					defer idFile.Close()

					id := data.Message.(map[string]any)["id"].(string)
					if _, err := idFile.Write([]byte(id)); err != nil {
						return fmt.Errorf("failed to write .fluxid: %v", err)
					}
				}
				return nil
			case "cmd_output":
				// suppress the command output if the quiet flag is set
				if quiet == nil || !*quiet {
					customWriter.Printf("... %s\n", data.Message)
				}
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
