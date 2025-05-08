package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	proxyManagerService "github.com/juls0730/flux/internal/services/proxy"
	"github.com/juls0730/flux/pkg/API"
	"go.uber.org/zap"
)

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
