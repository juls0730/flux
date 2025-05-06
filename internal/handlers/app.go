package handlers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/docker/pkg/namesgenerator"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	proxyManagerService "github.com/juls0730/flux/internal/services/proxy"
	"github.com/juls0730/flux/internal/util"
	"github.com/juls0730/flux/pkg"
	"github.com/juls0730/flux/pkg/API"
	"go.uber.org/zap"
)

var deploymentLock *util.MutexLock[uuid.UUID] = util.NewMutexLock[uuid.UUID]()

func (flux *FluxServer) DeployNewApp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "test/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	err := r.ParseMultipartForm(10 << 32) // 10 GiB
	if err != nil {
		flux.logger.Errorw("Failed to parse multipart form", zap.Error(err))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var deployRequest API.DeployRequest
	projectConfig := new(pkg.ProjectConfig)
	if err := json.Unmarshal([]byte(r.FormValue("config")), &projectConfig); err != nil {
		flux.logger.Errorw("Failed to decode config", zap.Error(err))

		http.Error(w, "Invalid flux.json", http.StatusBadRequest)
		return
	}

	deployRequest.Config = *projectConfig
	idStr := r.FormValue("id")

	if idStr == "" {
		id, err := uuid.NewRandom()
		if err != nil {
			flux.logger.Errorw("Failed to generate uuid", zap.Error(err))
			http.Error(w, "Failed to generate uuid", http.StatusInternalServerError)
			return
		}

		deployRequest.Id = id
	} else {
		deployRequest.Id, err = uuid.Parse(idStr)
		if err != nil {
			flux.logger.Errorw("Failed to parse uuid", zap.Error(err))
			http.Error(w, "Failed to parse uuid", http.StatusBadRequest)
			return
		}

		// make sure the id exists in the database
		app := flux.appManager.GetApp(deployRequest.Id)
		if app == nil {
			http.Error(w, "App not found", http.StatusNotFound)
			return
		}
	}

	ctx, err := deploymentLock.Lock(deployRequest.Id, r.Context())
	if err != nil && err == util.ErrLocked {
		// This will happen if the app is already being deployed
		http.Error(w, "Cannot deploy app, it's already being deployed", http.StatusConflict)
		return
	}

	go func() {
		<-ctx.Done()
		deploymentLock.Unlock(deployRequest.Id)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusMultiStatus)

	eventChannel := make(chan API.DeploymentEvent, 10)
	defer close(eventChannel)

	var wg sync.WaitGroup
	// make sure the connection doesnt close while there are SSE events being sent
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

				ev := API.DeploymentEvent{
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

	eventChannel <- API.DeploymentEvent{
		Stage:   "start",
		Message: "Uploading code",
	}

	deployRequest.Code, _, err = r.FormFile("code")
	if err != nil {
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: "No code archive found",
		}
		return
	}
	defer deployRequest.Code.Close()

	if projectConfig.Name == "" || projectConfig.Url == "" || projectConfig.Port == 0 {
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: "Invalid flux.json, a name, url, and port must be specified",
		}
		return
	}

	if projectConfig.Name == "all" {
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: "Reserved name 'all' is not allowed",
		}
		return
	}

	flux.logger.Infow("Deploying project", zap.String("name", projectConfig.Name), zap.String("url", projectConfig.Url), zap.String("id", deployRequest.Id.String()))

	projectPath, err := flux.UploadAppCode(deployRequest.Code, deployRequest.Id)
	if err != nil {
		flux.logger.Infow("Failed to upload code", zap.Error(err))
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to upload code: %s", err),
		}
		return
	}

	if projectConfig.EnvFile != "" {
		envPath := filepath.Join(projectPath, projectConfig.EnvFile)
		// prevent path traversal
		realEnvPath, err := filepath.EvalSymlinks(envPath)
		if err != nil {
			flux.logger.Errorw("Failed to eval symlinks", zap.Error(err))
			eventChannel <- API.DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Failed to eval symlinks: %s", err),
			}
			return
		}

		if !strings.HasPrefix(realEnvPath, projectPath) {
			flux.logger.Errorw("Env file is not in project directory", zap.String("env_file", projectConfig.EnvFile))
			eventChannel <- API.DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Env file is not in project directory: %s", projectConfig.EnvFile),
			}
			return
		}

		envBytes, err := os.Open(realEnvPath)
		if err != nil {
			flux.logger.Errorw("Failed to open env file", zap.Error(err))
			eventChannel <- API.DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Failed to open env file: %v", err),
			}
			return
		}
		defer envBytes.Close()

		envVars, err := godotenv.Parse(envBytes)
		if err != nil {
			flux.logger.Errorw("Failed to parse env file", zap.Error(err))
			eventChannel <- API.DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Failed to parse env file: %v", err),
			}
			return
		}

		for key, value := range envVars {
			projectConfig.Environment = append(projectConfig.Environment, fmt.Sprintf("%s=%s", key, value))
		}
	}

	// pipe the output of the build process to the event channel
	pipeGroup := sync.WaitGroup{}
	streamPipe := func(pipe io.ReadCloser) {
		pipeGroup.Add(1)
		defer pipeGroup.Done()
		defer pipe.Close()

		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			line := scanner.Text()
			eventChannel <- API.DeploymentEvent{
				Stage:   "cmd_output",
				Message: line,
			}
		}

		if err := scanner.Err(); err != nil {
			eventChannel <- API.DeploymentEvent{
				Stage:   "error",
				Message: fmt.Sprintf("Failed to read pipe: %s", err),
			}
			flux.logger.Errorw("Error reading pipe", zap.Error(err))
		}
	}

	flux.logger.Debugw("Preparing project", zap.String("name", projectConfig.Name), zap.String("id", deployRequest.Id.String()))
	eventChannel <- API.DeploymentEvent{
		Stage:   "preparing",
		Message: "Preparing project",
	}

	// redirect stdout and stderr to the event channel
	reader, writer := io.Pipe()
	prepareCmd := exec.Command("go", "generate")
	prepareCmd.Dir = projectPath
	prepareCmd.Stdout = writer
	prepareCmd.Stderr = writer

	err = prepareCmd.Start()
	if err != nil {
		flux.logger.Errorw("Failed to prepare project", zap.Error(err))
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to prepare project: %s", err),
		}

		return
	}

	go streamPipe(reader)

	pipeGroup.Wait()

	err = prepareCmd.Wait()
	if err != nil {
		flux.logger.Errorw("Failed to prepare project", zap.Error(err))
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to prepare project: %s", err),
		}

		return
	}

	writer.Close()

	eventChannel <- API.DeploymentEvent{
		Stage:   "building",
		Message: "Building project image",
	}

	reader, writer = io.Pipe()
	flux.logger.Debugw("Building image for project", zap.String("name", projectConfig.Name))
	imageName := fmt.Sprintf("fluxi-%s", namesgenerator.GetRandomName(0))
	buildCmd := exec.Command("pack", "build", imageName, "--builder", flux.config.Builder)
	buildCmd.Dir = projectPath
	buildCmd.Stdout = writer
	buildCmd.Stderr = writer

	err = buildCmd.Start()
	if err != nil {
		flux.logger.Errorw("Failed to build image", zap.Error(err))
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to build image: %s", err),
		}

		return
	}

	go streamPipe(reader)

	pipeGroup.Wait()

	err = buildCmd.Wait()
	if err != nil {
		flux.logger.Errorw("Failed to build image", zap.Error(err))
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to build image: %s", err),
		}

		return
	}

	app := flux.appManager.GetApp(deployRequest.Id)

	if app == nil {
		eventChannel <- API.DeploymentEvent{
			Stage:   "creating",
			Message: "Creating app, this might take a while...",
		}

		app, err = flux.appManager.CreateApp(r.Context(), imageName, projectConfig, deployRequest.Id)
	} else {
		eventChannel <- API.DeploymentEvent{
			Stage:   "upgrading",
			Message: "Upgrading app, this might take a while...",
		}

		// we dont need to change `app` since this upgrade will use the same app and update it in place
		err = flux.appManager.Upgrade(r.Context(), app.Id, imageName, projectConfig)
	}

	if err != nil {
		flux.logger.Errorw("Failed to deploy app", zap.Error(err))
		eventChannel <- API.DeploymentEvent{
			Stage:   "error",
			Message: fmt.Sprintf("Failed to upgrade app: %s", err),
		}

		return
	}

	var extApp API.App
	extApp.Id = app.Id
	extApp.Name = app.Name
	extApp.DeploymentID = app.DeploymentID

	eventChannel <- API.DeploymentEvent{
		Stage:   "complete",
		Message: extApp,
	}

	flux.logger.Infow("App deployed successfully", zap.String("id", app.Id.String()))
}

func (flux *FluxServer) GetAllApps(w http.ResponseWriter, r *http.Request) {
	var apps []API.App
	for _, app := range flux.appManager.GetAllApps() {
		var extApp API.App
		deploymentStatus, err := app.Deployment.Status(r.Context(), flux.docker, flux.logger)
		if err != nil {
			flux.logger.Errorw("Failed to get deployment status", zap.Error(err))
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		extApp.Id = app.Id
		extApp.Name = app.Name
		extApp.DeploymentID = app.DeploymentID
		extApp.DeploymentStatus = deploymentStatus
		apps = append(apps, extApp)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apps)
}

func (flux *FluxServer) GetAppById(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app id", http.StatusBadRequest)
		return
	}

	app := flux.appManager.GetApp(id)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	var extApp API.App
	deploymentStatus, err := app.Deployment.Status(r.Context(), flux.docker, flux.logger)
	if err != nil {
		flux.logger.Errorw("Failed to get deployment status", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	extApp.Id = app.Id
	extApp.Name = app.Name
	extApp.DeploymentID = app.DeploymentID
	extApp.DeploymentStatus = deploymentStatus

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(extApp)
}

func (flux *FluxServer) GetAppByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	app := flux.appManager.GetAppByName(name)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	var extApp API.App
	deploymentStatus, err := app.Deployment.Status(r.Context(), flux.docker, flux.logger)
	if err != nil {
		flux.logger.Errorw("Failed to get deployment status", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	extApp.Id = app.Id
	extApp.Name = app.Name
	extApp.DeploymentID = app.DeploymentID
	extApp.DeploymentStatus = deploymentStatus

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(extApp)
}

func (flux *FluxServer) StartApp(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app id", http.StatusBadRequest)
		return
	}

	app := flux.appManager.GetApp(id)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	status, err := app.Deployment.Status(r.Context(), flux.docker, flux.logger)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if status == "running" {
		http.Error(w, "App is already running", http.StatusNotModified)
		return
	}

	app.State = "running"
	_, err = flux.db.ExecContext(r.Context(), "UPDATE apps SET state = ? WHERE id = ?", app.State, app.Id[:])
	if err != nil {
		flux.logger.Errorw("Failed to update app state", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = app.Deployment.Start(r.Context(), flux.docker)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	deploymentInternalUrl, err := app.Deployment.GetInternalUrl(flux.docker)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	newProxy, err := proxyManagerService.NewDeploymentProxy(*deploymentInternalUrl)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	flux.proxy.AddProxy(app.Deployment.URL, newProxy)

	w.WriteHeader(http.StatusOK)
}

func (flux *FluxServer) StopApp(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app id", http.StatusBadRequest)
		return
	}

	app := flux.appManager.GetApp(id)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	status, err := app.Deployment.Status(r.Context(), flux.docker, flux.logger)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if status == "stopped" || status == "failed" {
		http.Error(w, "App is already stopped", http.StatusNotModified)
		return
	}

	app.State = "stopped"
	_, err = flux.db.ExecContext(r.Context(), "UPDATE apps SET state = ? WHERE id = ?", app.State, app.Id[:])
	if err != nil {
		flux.logger.Errorw("Failed to update app state", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = app.Deployment.Stop(r.Context(), flux.docker)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	flux.proxy.RemoveDeployment(app.Deployment.URL)

	w.WriteHeader(http.StatusOK)
}

func (flux *FluxServer) DeleteAllDeploymentsHandler(w http.ResponseWriter, r *http.Request) {
	if flux.config.DisableDeleteAll {
		http.Error(w, "Delete all deployments is disabled", http.StatusForbidden)
		return
	}

	apps := flux.appManager.GetAllApps()
	for _, app := range apps {
		err := flux.appManager.DeleteApp(app.Id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (flux *FluxServer) DeleteDeployHandler(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid app id", http.StatusBadRequest)
		return
	}

	app := flux.appManager.GetApp(id)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	status, err := app.Deployment.Status(r.Context(), flux.docker, flux.logger)
	if err != nil {
		flux.logger.Errorw("Failed to get deployment status", zap.Error(err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if status != "stopped" {
		app.Deployment.Stop(r.Context(), flux.docker)
		flux.proxy.RemoveDeployment(app.Deployment.URL)
	}

	err = flux.appManager.DeleteApp(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
