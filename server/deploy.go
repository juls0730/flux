package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os/exec"

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

func (s *FluxServer) DeployHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(10 << 30) // 10 GiB
	if err != nil {
		log.Printf("Failed to parse multipart form: %v\n", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// bind to DeployRequest struct
	var deployRequest DeployRequest
	deployRequest.Config, _, err = r.FormFile("config")
	if err != nil {
		http.Error(w, "No flux.json found", http.StatusBadRequest)
		return
	}
	defer deployRequest.Config.Close()

	deployRequest.Code, _, err = r.FormFile("code")
	if err != nil {
		http.Error(w, "No code archive found", http.StatusBadRequest)
		return
	}
	defer deployRequest.Code.Close()

	var projectConfig pkg.ProjectConfig
	if err := json.NewDecoder(deployRequest.Config).Decode(&projectConfig); err != nil {
		log.Printf("Failed to decode config: %v\n", err)
		http.Error(w, "Invalid flux.json", http.StatusBadRequest)
		return
	}

	if projectConfig.Name == "" || projectConfig.Url == "" || projectConfig.Port == 0 {
		http.Error(w, "Invalid flux.json, a name, url, and port must be specified", http.StatusBadRequest)
		return
	}

	log.Printf("Deploying project %s to %s\n", projectConfig.Name, projectConfig.Url)

	projectPath, err := s.UploadAppCode(deployRequest.Code, projectConfig)
	if err != nil {
		log.Printf("Failed to upload code: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("Preparing project %s...\n", projectConfig.Name)
	prepareCmd := exec.Command("go", "generate")
	prepareCmd.Dir = projectPath
	err = prepareCmd.Run()
	if err != nil {
		log.Printf("Failed to prepare project: %s\n", err)
		http.Error(w, fmt.Sprintf("Failed to prepare project: %s", err), http.StatusInternalServerError)
		return
	}

	log.Printf("Building image for project %s...\n", projectConfig.Name)
	imageName := fmt.Sprintf("flux_%s-image", projectConfig.Name)
	buildCmd := exec.Command("pack", "build", imageName, "--builder", s.config.Builder)
	buildCmd.Dir = projectPath
	err = buildCmd.Run()
	if err != nil {
		log.Printf("Failed to build image: %s\n", err)
		http.Error(w, fmt.Sprintf("Failed to build image: %s", err), http.StatusInternalServerError)
		return
	}

	if Flux.appManager == nil {
		panic("App manager is nil")
	}

	app := Flux.appManager.GetApp(projectConfig.Name)

	if app == nil {
		app, err = CreateApp(r.Context(), imageName, projectPath, projectConfig)
		if err != nil {
			log.Printf("Failed to create app: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		err = app.Upgrade(r.Context(), projectConfig, imageName, projectPath)
		if err != nil {
			log.Printf("Failed to upgrade deployment: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	log.Printf("App %s deployed successfully!\n", app.Name)

	json.NewEncoder(w).Encode(DeployResponse{
		App: *app,
	})
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
