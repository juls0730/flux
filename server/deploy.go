package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os/exec"
	"strings"

	"github.com/juls0730/fluxd/models"
)

type DeployRequest struct {
	Config multipart.File `form:"config"`
	Code   multipart.File `form:"code"`
}

type DeployResponse struct {
	AppID int64 `json:"app_id"`
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

	var projectConfig models.ProjectConfig
	if err := json.NewDecoder(deployRequest.Config).Decode(&projectConfig); err != nil {
		log.Printf("Failed to decode config: %v\n", err)
		http.Error(w, "Invalid flux.json", http.StatusBadRequest)
		return
	}

	if projectConfig.Name == "" {
		http.Error(w, "No project name specified", http.StatusBadRequest)
		return
	}

	if projectConfig.Urls == nil || len(projectConfig.Urls) == 0 {
		http.Error(w, "No deployment urls specified", http.StatusBadRequest)
		return
	}

	if projectConfig.Port == 0 {
		http.Error(w, "No port specified", http.StatusBadRequest)
		return
	}

	log.Printf("Deploying project %s to %s\n", projectConfig.Name, projectConfig.Urls)

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
	imageName := fmt.Sprintf("%s-image", projectConfig.Name)
	buildCmd := exec.Command("pack", "build", imageName, "--builder", s.config.Builder)
	buildCmd.Dir = projectPath
	err = buildCmd.Run()
	if err != nil {
		log.Printf("Failed to build image: %s\n", err)
		http.Error(w, fmt.Sprintf("Failed to build image: %s", err), http.StatusInternalServerError)
		return
	}

	containerID, err := s.containerManager.DeployContainer(r.Context(), imageName, projectConfig.Name, projectPath, projectConfig)
	if err != nil {
		log.Printf("Failed to deploy container: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	deploymentResult, err := s.db.Exec("INSERT INTO deployments (urls) VALUES (?)", strings.Join(projectConfig.Urls, ","))
	if err != nil {
		log.Printf("Failed to insert deployment: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	deploymentID, err := deploymentResult.LastInsertId()
	if err != nil {
		log.Printf("Failed to get deployment id: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = s.db.Exec("INSERT INTO containers (container_id, deployment_id, status) VALUES (?, ?, ?)", containerID, deploymentID, "pending")
	if err != nil {
		log.Printf("Failed to get container id: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var app struct {
		id            int
		name          string
		deployment_id int
	}
	s.db.QueryRow("SELECT id, name, deployment_id FROM apps WHERE name = ?", projectConfig.Name).Scan(&app.id, &app.name, &app.deployment_id)
	configBytes, err := json.Marshal(projectConfig)
	if err != nil {
		log.Printf("Failed to marshal project config: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var appResult sql.Result
	if app.id == 0 {
		// create app in the database
		appResult, err = tx.Exec("INSERT INTO apps (name, image, project_path, project_config, deployment_id) VALUES (?, ?, ?, ?, ?)", projectConfig.Name, imageName, projectPath, configBytes, deploymentID)
		if err != nil {
			tx.Rollback()
			log.Printf("Failed to insert app: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		_, err = tx.Exec("DELETE FROM deployments WHERE id = ?", app.deployment_id)
		if err != nil {
			tx.Rollback()
			log.Printf("Failed to delete old deployment: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		_, err = tx.Exec("DELETE FROM containers WHERE deployment_id = ?", app.deployment_id)
		if err != nil {
			tx.Rollback()
			log.Printf("Failed to delete old containers: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// update app in the database
		appResult, err = tx.Exec("UPDATE apps SET project_config = ?, deployment_id = ? WHERE name = ?", configBytes, deploymentID, projectConfig.Name)
		if err != nil {
			tx.Rollback()
			log.Printf("Failed to update app: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	appId, err := appResult.LastInsertId()
	if err != nil {
		log.Printf("Failed to get app id: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(DeployResponse{
		AppID: appId,
	})
}

func (s *FluxServer) DeleteDeployHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var app struct {
		id            int
		name          string
		deployment_id int
	}
	s.db.QueryRow("SELECT id, name, deployment_id FROM apps WHERE name = ?", name).Scan(&app.id, &app.name, &app.deployment_id)

	if app.id == 0 {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	var containerId []string
	rows, err := s.db.Query("SELECT container_id FROM containers WHERE deployment_id = ?", app.deployment_id)
	if err != nil {
		log.Printf("Failed to query containers: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var newContainerId string
		if err := rows.Scan(&newContainerId); err != nil {
			log.Printf("Failed to scan container id: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		containerId = append(containerId, newContainerId)
	}

	log.Printf("Deleting deployment %s...\n", name)

	for _, container := range containerId {
		s.containerManager.RemoveContainer(r.Context(), container)
	}

	s.containerManager.RemoveVolume(r.Context(), fmt.Sprintf("%s-volume", name))

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM deployments WHERE id = ?", app.deployment_id)
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to delete deployment: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM containers WHERE deployment_id = ?", app.deployment_id)
	if err != nil {
		tx.Rollback()
		log.Printf("Failed to delete containers: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = tx.Exec("DELETE FROM apps WHERE id = ?", app.id)
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

	w.WriteHeader(http.StatusOK)
}

func (s *FluxServer) ListAppsHandler(w http.ResponseWriter, r *http.Request) {
	// Implement app listing logic
	var apps []models.App
	rows, err := s.db.Query("SELECT * FROM apps")
	if err != nil {
		log.Printf("Failed to query apps: %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var app models.App
		var configBytes string
		if err := rows.Scan(&app.ID, &app.Name, &app.Image, &app.ProjectPath, &configBytes, &app.DeploymentID, &app.CreatedAt); err != nil {
			log.Printf("Failed to scan app: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		err = json.Unmarshal([]byte(configBytes), &app.ProjectConfig)
		if err != nil {
			log.Printf("Failed to unmarshal project config: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		apps = append(apps, app)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(apps)
}
