package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/juls0730/fluxd/pkg"
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

	app := Apps.GetApp(projectConfig.Name)

	if app == nil {
		app = &App{
			Name: projectConfig.Name,
		}
		log.Printf("Creating deployment %s...\n", app.Name)

		containerID, err := CreateDockerContainer(r.Context(), imageName, projectPath, projectConfig)
		if err != nil {
			log.Printf("Failed to create container: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		deployment, err := CreateDeployment(containerID, projectConfig.Port, projectConfig.Url, s.db)
		app.Deployment = deployment
		if err != nil {
			log.Printf("Failed to create deployment: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if appInsertStmt == nil {
			appInsertStmt, err = s.db.Prepare("INSERT INTO apps (name, deployment_id) VALUES ($1, $2) RETURNING id, name, deployment_id")
			if err != nil {
				log.Printf("Failed to prepare statement: %v\n", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// create app in the database
		err = appInsertStmt.QueryRow(projectConfig.Name, deployment.ID).Scan(&app.ID, &app.Name, &app.DeploymentID)
		if err != nil {
			log.Printf("Failed to insert app: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		err = deployment.Start(r.Context())
		if err != nil {
			log.Printf("Failed to start deployment: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		Apps.AddApp(app.Name, app)
	} else {
		log.Printf("Upgrading deployment %s...\n", app.Name)

		// if deploy is not started, start it
		deploymentStatus, err := app.Deployment.Status(r.Context())
		if deploymentStatus != "running" || err != nil {
			err = app.Deployment.Start(r.Context())
			if err != nil {
				log.Printf("Failed to start deployment: %v\n", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		err = app.Deployment.Upgrade(r.Context(), projectConfig, imageName, projectPath, s)
		if err != nil {
			log.Printf("Failed to upgrade deployment: %v\n", err)
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

	app := Apps.GetApp(name)
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

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) StopDeployHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	app := Apps.GetApp(name)
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
	var err error

	app := Apps.GetApp(name)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	log.Printf("Deleting deployment %s...\n", name)

	for _, container := range app.Deployment.Containers {
		err = RemoveDockerContainer(r.Context(), string(container.ContainerID[:]))
		if err != nil {
			log.Printf("Failed to remove container: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	err = RemoveVolume(r.Context(), fmt.Sprintf("flux_%s-volume", name))
	if err != nil {
		log.Printf("Failed to remove volume: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM deployments WHERE id = ?", app.DeploymentID)
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to delete deployment: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM containers WHERE deployment_id = ?", app.DeploymentID)
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to delete containers: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM apps WHERE id = ?", app.ID)
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to delete app: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	projectPath := filepath.Join(s.rootDir, "apps", name)
	err = os.RemoveAll(projectPath)
	if err != nil {
		log.Printf("Failed to remove project directory: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	Apps.DeleteApp(name)

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) DeleteAllDeploymentsHandler(w http.ResponseWriter, r *http.Request) {
	var err error

	for _, app := range Apps.GetAllApps() {
		for _, container := range app.Deployment.Containers {
			err = RemoveDockerContainer(r.Context(), string(container.ContainerID[:]))
			if err != nil {
				log.Printf("Failed to remove container: %v\n", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		err = RemoveVolume(r.Context(), fmt.Sprintf("flux_%s-volume", app.Name))
		if err != nil {
			log.Printf("Failed to remove volume: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM deployments")
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to delete deployments: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM containers")
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to delete containers: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM apps")
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to delete apps: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := os.RemoveAll(filepath.Join(s.rootDir, "apps")); err != nil {
		log.Printf("Failed to remove apps directory: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := os.RemoveAll(filepath.Join(s.rootDir, "deployments")); err != nil {
		log.Printf("Failed to remove deployments directory: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) ListAppsHandler(w http.ResponseWriter, r *http.Request) {
	// for each app, get the deployment status
	var apps []*pkg.App
	for _, app := range Apps.GetAllApps() {
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
