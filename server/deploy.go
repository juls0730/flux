package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"sync"

	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

var (
	appInsertStmt *sql.Stmt
)

type DeployRequest struct {
	Config multipart.File `form:"config"`
	Code   multipart.File `form:"code"`
}

type DeployResponse struct {
	App App `json:"app"`
}

type DeploymentLock struct {
	mu       sync.Mutex
	deployed map[string]context.CancelFunc
}

func NewDeploymentLock() *DeploymentLock {
	return &DeploymentLock{
		deployed: make(map[string]context.CancelFunc),
	}
}

func (dt *DeploymentLock) StartDeployment(appName string, ctx context.Context) (context.Context, error) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	// Check if the app is already being deployed
	if _, exists := dt.deployed[appName]; exists {
		return nil, fmt.Errorf("app %s is already being deployed", appName)
	}

	// Create a context that can be cancelled
	ctx, cancel := context.WithCancel(ctx)

	// Store the cancel function
	dt.deployed[appName] = cancel

	return ctx, nil
}

func (dt *DeploymentLock) CompleteDeployment(appName string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	// Remove the app from deployed tracking
	if cancel, exists := dt.deployed[appName]; exists {
		// Cancel the context
		cancel()
		// Remove from map
		delete(dt.deployed, appName)
	}
}

var deploymentLock = NewDeploymentLock()

type DeploymentEvent struct {
	Stage      string      `json:"stage"`
	Message    interface{} `json:"message"`
	StatusCode int         `json:"status,omitempty"`
}

func (s *FluxServer) DeployHandler(w http.ResponseWriter, r *http.Request) {
	if Flux.appManager == nil {
		panic("App manager is nil")
	}

	w.Header().Set("Content-Type", "test/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	err := r.ParseMultipartForm(10 << 30) // 10 GiB
	if err != nil {
		logger.Errorw("Failed to parse multipart form", zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var deployRequest DeployRequest
	deployRequest.Config, _, err = r.FormFile("config")
	if err != nil {
		http.Error(w, "No flux.json found", http.StatusBadRequest)
		return
	}
	defer deployRequest.Config.Close()

	var projectConfig pkg.ProjectConfig
	if err := json.NewDecoder(deployRequest.Config).Decode(&projectConfig); err != nil {
		logger.Errorw("Failed to decode config", zap.Error(err))

		http.Error(w, "Invalid flux.json", http.StatusBadRequest)
		return
	}

	ctx, err := deploymentLock.StartDeployment(projectConfig.Name, r.Context())
	if err != nil {
		// This will happen if the app is already being deployed
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	go func() {
		<-ctx.Done()
		deploymentLock.CompleteDeployment(projectConfig.Name)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusMultiStatus)

	eventChannel := make(chan DeploymentEvent, 10)
	defer close(eventChannel)

	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Add(1)
	go func(w http.ResponseWriter, flusher http.Flusher) {
		defer wg.Done()

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-eventChannel:
				if !ok {
					return
				}

				ev := pkg.DeploymentEvent{
					Message: event.Message,
				}

				eventJSON, err := json.Marshal(ev)
				if err != nil {
					// Write error directly to ResponseWriter
					jsonErr := json.NewEncoder(w).Encode(err)
					if jsonErr != nil {
						fmt.Fprint(w, "data: {\"message\": \"Error encoding error\"}\n\n")
						return
					}

					fmt.Fprintf(w, "data: %s\n\n", err.Error())
					if flusher != nil {
						flusher.Flush()
					}
					return
				}

				fmt.Fprintf(w, "event: %s\n", event.Stage)
				fmt.Fprintf(w, "data: %s\n\n", eventJSON)
				if flusher != nil {
					flusher.Flush()
				}

				if event.Stage == "error" || event.Stage == "complete" {
					return
				}
			}
		}
	}(w, flusher)

	eventChannel <- DeploymentEvent{
		Stage:   "start",
		Message: "Uploading code",
	}

	deployRequest.Code, _, err = r.FormFile("code")
	if err != nil {
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    "No code archive found",
			StatusCode: http.StatusBadRequest,
		}
		return
	}
	defer deployRequest.Code.Close()

	if projectConfig.Name == "" || projectConfig.Url == "" || projectConfig.Port == 0 {
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    "Invalid flux.json, a name, url, and port must be specified",
			StatusCode: http.StatusBadRequest,
		}
		return
	}

	logger.Infow("Deploying project", zap.String("name", projectConfig.Name), zap.String("url", projectConfig.Url))

	projectPath, err := s.UploadAppCode(deployRequest.Code, projectConfig)
	if err != nil {
		logger.Infow("Failed to upload code", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to upload code: %s", err),
			StatusCode: http.StatusInternalServerError,
		}
		return
	}

	// Streams the each line of the pipe into the eventChannel, this closes the pipe when the function exits
	var pipeGroup sync.WaitGroup

	streamPipe := func(pipe io.ReadCloser) {
		pipeGroup.Add(1)
		defer pipeGroup.Done()

		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			line := scanner.Text()
			eventChannel <- DeploymentEvent{
				Stage:   "cmd_output",
				Message: line,
			}
		}

		if err := scanner.Err(); err != nil {
			eventChannel <- DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Failed to read pipe: %s", err),
			}
			logger.Errorw("Error reading pipe", zap.Error(err))
		}
	}

	logger.Debugw("Preparing project", zap.String("name", projectConfig.Name))
	eventChannel <- DeploymentEvent{
		Stage:   "preparing",
		Message: "Preparing project",
	}

	prepareCmd := exec.Command("go", "generate")
	prepareCmd.Dir = projectPath
	cmdOut, err := prepareCmd.StdoutPipe()
	if err != nil {
		logger.Errorw("Failed to get stdout pipe", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to get stdout pipe: %s", err),
			StatusCode: http.StatusInternalServerError,
		}

		return
	}
	cmdErr, err := prepareCmd.StderrPipe()
	if err != nil {
		logger.Errorw("Failed to get stderr pipe", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to get stderr pipe: %s", err),
			StatusCode: http.StatusInternalServerError,
		}
		return
	}

	err = prepareCmd.Start()
	if err != nil {
		logger.Errorw("Failed to prepare project", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to prepare project: %s", err),
			StatusCode: http.StatusInternalServerError,
		}

		return
	}

	go streamPipe(cmdOut)
	go streamPipe(cmdErr)

	pipeGroup.Wait()

	err = prepareCmd.Wait()
	if err != nil {
		logger.Errorw("Failed to prepare project", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to prepare project: %s", err),
			StatusCode: http.StatusInternalServerError,
		}

		return
	}

	eventChannel <- DeploymentEvent{
		Stage:   "building",
		Message: "Building project image",
	}

	logger.Debugw("Building image for project", zap.String("name", projectConfig.Name))
	imageName := fmt.Sprintf("flux_%s-image", projectConfig.Name)
	buildCmd := exec.Command("pack", "build", imageName, "--builder", s.config.Builder)
	buildCmd.Dir = projectPath
	cmdOut, err = buildCmd.StdoutPipe()
	if err != nil {
		logger.Errorw("Failed to get stdout pipe", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to get stdout pipe: %s", err),
			StatusCode: http.StatusInternalServerError,
		}

		return
	}
	cmdErr, err = buildCmd.StderrPipe()
	if err != nil {
		logger.Errorw("Failed to get stderr pipe", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to get stderr pipe: %s", err),
			StatusCode: http.StatusInternalServerError,
		}

		return
	}

	err = buildCmd.Start()
	if err != nil {
		logger.Errorw("Failed to build image", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to build image: %s", err),
			StatusCode: http.StatusInternalServerError,
		}

		return
	}

	go streamPipe(cmdOut)
	go streamPipe(cmdErr)

	pipeGroup.Wait()

	err = buildCmd.Wait()
	if err != nil {
		logger.Errorw("Failed to build image", zap.Error(err))
		eventChannel <- DeploymentEvent{
			Stage:      "error",
			Message:    fmt.Sprintf("Failed to build image: %s", err),
			StatusCode: http.StatusInternalServerError,
		}

		return
	}

	app := Flux.appManager.GetApp(projectConfig.Name)

	eventChannel <- DeploymentEvent{
		Stage:   "creating",
		Message: "Creating deployment",
	}

	if app == nil {
		app, err = CreateApp(ctx, imageName, projectPath, projectConfig)
		if err != nil {
			logger.Errorw("Failed to create app", zap.Error(err))
			eventChannel <- DeploymentEvent{
				Stage:      "error",
				Message:    fmt.Sprintf("Failed to create app: %s", err),
				StatusCode: http.StatusInternalServerError,
			}

			return
		}
	} else {
		err = app.Upgrade(ctx, projectConfig, imageName, projectPath)
		if err != nil {
			logger.Errorw("Failed to upgrade app", zap.Error(err))
			eventChannel <- DeploymentEvent{
				Stage:      "error",
				Message:    fmt.Sprintf("Failed to upgrade app: %s", err),
				StatusCode: http.StatusInternalServerError,
			}

			return
		}
	}

	eventChannel <- DeploymentEvent{
		Stage:   "complete",
		Message: app,
	}

	logger.Infow("App deployed successfully", zap.String("name", app.Name))
}

func (s *FluxServer) StartDeployHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	app := Flux.appManager.GetApp(name)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	status, err := app.Deployment.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if status == "running" {
		http.Error(w, "App is already running", http.StatusBadRequest)
		return
	}

	err = app.Deployment.Start(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if app.Deployment.Proxy == nil {
		app.Deployment.Proxy, _ = app.Deployment.NewDeploymentProxy()
	}

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) StopDeployHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	app := Flux.appManager.GetApp(name)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	status, err := app.Deployment.Status(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if status == "stopped" {
		http.Error(w, "App is already stopped", http.StatusBadRequest)
		return
	}

	err = app.Deployment.Stop(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) DeleteDeployHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	logger.Debugw("Deleting deployment", zap.String("name", name))

	err := Flux.appManager.DeleteApp(name)

	if err != nil {
		logger.Errorw("Failed to delete app", zap.Error(err))
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) DeleteAllDeploymentsHandler(w http.ResponseWriter, r *http.Request) {
	for _, app := range Flux.appManager.GetAllApps() {
		err := Flux.appManager.DeleteApp(app.Name)
		if err != nil {
			logger.Errorw("Failed to remove app", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) ListAppsHandler(w http.ResponseWriter, r *http.Request) {
	// for each app, get the deployment status
	var apps []pkg.App
	for _, app := range Flux.appManager.GetAllApps() {
		var extApp pkg.App
		deploymentStatus, err := app.Deployment.Status(r.Context())
		if err != nil {
			logger.Errorw("Failed to get deployment status", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		extApp.ID = app.ID
		extApp.Name = app.Name
		extApp.DeploymentID = app.DeploymentID
		extApp.DeploymentStatus = deploymentStatus
		apps = append(apps, extApp)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apps)
}

func (s *FluxServer) DaemonInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pkg.Info{
		Compression: s.config.Compression,
	})
}
