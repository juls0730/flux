package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os/exec"
	"sync"

	"github.com/juls0730/flux/pkg"
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

func (s *FluxServer) DeployHandler(w http.ResponseWriter, r *http.Request) {
	if Flux.appManager == nil {
		panic("App manager is nil")
	}

	w.Header().Set("Content-Type", "test/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	err := r.ParseMultipartForm(10 << 30) // 10 GiB
	if err != nil {
		log.Printf("Failed to parse multipart form: %v\n", err)
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
		log.Printf("Failed to decode config: %v\n", err)

		http.Error(w, "Invalid flux.json", http.StatusBadRequest)
		return
	}

	ctx, err := deploymentLock.StartDeployment(projectConfig.Name, r.Context())
	if err != nil {
		// This will happen if the app is already being deployed
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	defer deploymentLock.CompleteDeployment(projectConfig.Name)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	eventChannel := make(chan pkg.DeploymentEvent, 10)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		for {
			select {
			case event, ok := <-eventChannel:
				if !ok {
					return
				}

				eventJSON, err := json.Marshal(event)
				if err != nil {
					fmt.Fprintf(w, "data: %s\n\n", err.Error())
					flusher.Flush()
					return
				}

				fmt.Fprintf(w, "data: %s\n\n", eventJSON)
				flusher.Flush()
			case <-ctx.Done():
				return
			}
		}
	}()

	eventChannel <- pkg.DeploymentEvent{
		Stage:   "start",
		Message: "Uploading code",
	}

	deployRequest.Code, _, err = r.FormFile("code")
	if err != nil {
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: "No code archive found",
			Error:   err.Error(),
		}
		http.Error(w, "No code archive found", http.StatusBadRequest)
		return
	}
	defer deployRequest.Code.Close()

	if projectConfig.Name == "" || projectConfig.Url == "" || projectConfig.Port == 0 {
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: "Invalid flux.json, a name, url, and port must be specified",
			Error:   "Invalid flux.json, a name, url, and port must be specified",
		}
		http.Error(w, "Invalid flux.json, a name, url, and port must be specified", http.StatusBadRequest)
		return
	}

	log.Printf("Deploying project %s to %s\n", projectConfig.Name, projectConfig.Url)

	projectPath, err := s.UploadAppCode(deployRequest.Code, projectConfig)
	if err != nil {
		log.Printf("Failed to upload code: %v\n", err)
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: "Failed to upload code",
			Error:   err.Error(),
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	streamPipe := func(pipe io.ReadCloser) {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			line := scanner.Text()
			eventChannel <- pkg.DeploymentEvent{
				Stage:   "cmd_output",
				Message: fmt.Sprintf("%s", line),
			}
		}

		if err := scanner.Err(); err != nil {
			eventChannel <- pkg.DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Failed to read pipe: %s", err),
			}
			log.Printf("Error reading pipe: %s\n", err)
		}
	}

	log.Printf("Preparing project %s...\n", projectConfig.Name)
	eventChannel <- pkg.DeploymentEvent{
		Stage:   "preparing",
		Message: "Preparing project",
	}

	prepareCmd := exec.Command("go", "generate")
	prepareCmd.Dir = projectPath
	cmdOut, err := prepareCmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to get stdout pipe: %v\n", err)
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to get stdout pipe: %s", err),
			Error:   err.Error(),
		}

		http.Error(w, fmt.Sprintf("Failed to get stdout pipe: %s", err), http.StatusInternalServerError)
		return
	}
	cmdErr, err := prepareCmd.StderrPipe()
	if err != nil {
		log.Printf("Failed to get stderr pipe: %v\n", err)
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to get stderr pipe: %s", err),
			Error:   err.Error(),
		}

		http.Error(w, fmt.Sprintf("Failed to get stderr pipe: %s", err), http.StatusInternalServerError)
		return
	}

	go streamPipe(cmdOut)
	go streamPipe(cmdErr)

	err = prepareCmd.Run()
	if err != nil {
		log.Printf("Failed to prepare project: %s\n", err)
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to prepare project: %s", err),
			Error:   err.Error(),
		}

		http.Error(w, fmt.Sprintf("Failed to prepare project: %s", err), http.StatusInternalServerError)
		return
	}
	cmdOut.Close()
	cmdErr.Close()

	eventChannel <- pkg.DeploymentEvent{
		Stage:   "building",
		Message: "Building project image",
	}

	log.Printf("Building image for project %s...\n", projectConfig.Name)
	imageName := fmt.Sprintf("flux_%s-image", projectConfig.Name)
	buildCmd := exec.Command("pack", "build", imageName, "--builder", s.config.Builder)
	buildCmd.Dir = projectPath
	cmdOut, err = buildCmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to get stdout pipe: %v\n", err)
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to get stdout pipe: %s", err),
			Error:   err.Error(),
		}

		http.Error(w, fmt.Sprintf("Failed to get stdout pipe: %s", err), http.StatusInternalServerError)
		return
	}
	cmdErr, err = buildCmd.StderrPipe()
	if err != nil {
		log.Printf("Failed to get stderr pipe: %v\n", err)
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to get stderr pipe: %s", err),
			Error:   err.Error(),
		}

		http.Error(w, fmt.Sprintf("Failed to get stderr pipe: %s", err), http.StatusInternalServerError)
		return
	}

	go streamPipe(cmdOut)
	go streamPipe(cmdErr)

	err = buildCmd.Run()
	if err != nil {
		log.Printf("Failed to build image: %s\n", err)
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to build image: %s", err),
			Error:   err.Error(),
		}

		http.Error(w, fmt.Sprintf("Failed to build image: %s", err), http.StatusInternalServerError)
		return
	}
	cmdOut.Close()
	cmdErr.Close()

	app := Flux.appManager.GetApp(projectConfig.Name)

	eventChannel <- pkg.DeploymentEvent{
		Stage:   "creating",
		Message: "Creating deployment",
	}

	if app == nil {
		app, err = CreateApp(ctx, imageName, projectPath, projectConfig)
		if err != nil {
			log.Printf("Failed to create app: %v", err)
			eventChannel <- pkg.DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Failed to create app: %s", err),
				Error:   err.Error(),
			}

			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		err = app.Upgrade(ctx, projectConfig, imageName, projectPath)
		if err != nil {
			log.Printf("Failed to upgrade app: %v", err)
			eventChannel <- pkg.DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Failed to upgrade app: %s", err),
				Error:   err.Error(),
			}

			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	responseJSON, err := json.Marshal(DeployResponse{
		App: *app,
	})
	if err != nil {
		log.Printf("Failed to marshal deploy response: %v\n", err)
		eventChannel <- pkg.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to marshal deploy response: %s", err),
			Error:   err.Error(),
		}

		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	eventChannel <- pkg.DeploymentEvent{
		Stage:   "complete",
		Message: fmt.Sprintf("%s", responseJSON),
	}

	log.Printf("App %s deployed successfully!\n", app.Name)

	close(eventChannel)

	// make sure all the events are flushed
	wg.Wait()
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
		var headContainer *Container
		for _, container := range app.Deployment.Containers {
			if container.Head {
				headContainer = &container
			}
		}

		app.Deployment.Proxy, _ = NewDeploymentProxy(&app.Deployment, headContainer)
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

	log.Printf("Deleting deployment %s...\n", name)

	err := Flux.appManager.DeleteApp(name)

	if err != nil {
		log.Printf("Failed to delete app: %v\n", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) DeleteAllDeploymentsHandler(w http.ResponseWriter, r *http.Request) {
	for _, app := range Flux.appManager.GetAllApps() {
		err := app.Remove(r.Context())
		if err != nil {
			log.Printf("Failed to remove app: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) ListAppsHandler(w http.ResponseWriter, r *http.Request) {
	// for each app, get the deployment status
	var apps []*pkg.App
	for _, app := range Flux.appManager.GetAllApps() {
		var extApp pkg.App
		deploymentStatus, err := app.Deployment.Status(r.Context())
		if err != nil {
			log.Printf("Failed to get deployment status: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		extApp.ID = app.ID
		extApp.Name = app.Name
		extApp.DeploymentID = app.DeploymentID
		extApp.DeploymentStatus = deploymentStatus
		apps = append(apps, &extApp)
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
