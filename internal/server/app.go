package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

type App struct {
	Id           uuid.UUID   `json:"id,omitempty"`
	Name         string      `json:"name,omitempty"`
	Deployment   *Deployment `json:"-"`
	DeploymentID int64       `json:"deployment_id,omitempty"`
	flux         *FluxServer
}

func (flux *FluxServer) GetAppByNameHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	app := flux.appManager.GetAppByName(name)
	if app == nil {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	var extApp pkg.App
	deploymentStatus, err := app.Deployment.Status(r.Context())
	if err != nil {
		logger.Errorw("Failed to get deployment status", zap.Error(err))
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

// Create the initial app row in the database and create and start the deployment. The app is the overarching data
// structure that contains all of the data for a project
func (flux *FluxServer) CreateApp(ctx context.Context, imageName string, projectPath string, projectConfig *pkg.ProjectConfig, id uuid.UUID) (*App, error) {
	app := &App{
		Id:   id,
		flux: flux,
	}
	logger.Debugw("Creating deployment", zap.String("id", app.Id.String()))

	deployment, err := flux.CreateDeployment(projectConfig.Port, projectConfig.Url)
	app.Deployment = deployment
	if err != nil {
		logger.Errorw("Failed to create deployment", zap.Error(err))
		return nil, err
	}

	for _, container := range projectConfig.Containers {
		c, err := flux.CreateContainer(ctx, &container, false, deployment, container.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to create container: %v", err)
		}

		c.Start(ctx, true)
	}

	headContainer := pkg.Container{
		ImageName:   imageName,
		Volumes:     projectConfig.Volumes,
		Environment: projectConfig.Environment,
	}

	// this call does a lot for us, see it's documentation for more info
	_, err = flux.CreateContainer(ctx, &headContainer, true, deployment, projectConfig.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %v", err)
	}

	// create app in the database
	var appIdBlob []byte
	err = appInsertStmt.QueryRow(id[:], projectConfig.Name, deployment.ID).Scan(&appIdBlob, &app.Name, &app.DeploymentID)
	if err != nil {
		return nil, fmt.Errorf("failed to insert app: %v", err)
	}
	app.Id, err = uuid.FromBytes(appIdBlob)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app id: %v", err)
	}

	err = deployment.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to start deployment: %v", err)
	}

	flux.appManager.AddApp(app.Id, app)

	return app, nil
}

func (app *App) Upgrade(ctx context.Context, imageName string, projectPath string, projectConfig *pkg.ProjectConfig) error {
	logger.Debugw("Upgrading deployment", zap.String("id", app.Id.String()))

	// if deploy is not started, start it
	deploymentStatus, err := app.Deployment.Status(ctx)
	if err != nil {
		return fmt.Errorf("failed to get deployment status: %v", err)
	}

	if deploymentStatus != "running" {
		err = app.Deployment.Start(ctx)
		if err != nil {
			return fmt.Errorf("failed to start deployment: %v", err)
		}
	}

	app.flux.db.Exec("UPDATE apps SET name = ? WHERE id = ?", projectConfig.Name, app.Id[:])

	err = app.Deployment.Upgrade(ctx, projectConfig, imageName, projectPath)
	if err != nil {
		return fmt.Errorf("failed to upgrade deployment: %v", err)
	}

	return nil
}

// delete an app and deployment from the database, and its project files from disk.
func (app *App) Remove(ctx context.Context) error {
	app.flux.appManager.RemoveApp(app.Id)

	err := app.Deployment.Remove(ctx)
	if err != nil {
		logger.Errorw("Failed to remove deployment", zap.Error(err))
		return err
	}

	_, err = app.flux.db.Exec("DELETE FROM apps WHERE id = ?", app.Id[:])
	if err != nil {
		logger.Errorw("Failed to delete app", zap.Error(err))
		return err
	}

	projectPath := filepath.Join(app.flux.rootDir, "apps", app.Id.String())
	err = os.RemoveAll(projectPath)
	if err != nil {
		return fmt.Errorf("failed to remove project directory: %v", err)
	}

	return nil
}

type AppManager struct {
	pkg.TypedMap[uuid.UUID, *App]
	nameIndex pkg.TypedMap[string, uuid.UUID]
}

func (am *AppManager) GetAppByName(name string) *App {
	id, ok := am.nameIndex.Load(name)
	if !ok {
		return nil
	}

	return am.GetApp(id)
}

func (am *AppManager) GetApp(id uuid.UUID) *App {
	app, exists := am.Load(id)
	if !exists {
		return nil
	}

	return app
}

func (am *AppManager) GetAllApps() []*App {
	var apps []*App
	am.Range(func(key uuid.UUID, app *App) bool {
		apps = append(apps, app)
		return true
	})
	return apps
}

// removes an app from the app manager
func (am *AppManager) RemoveApp(id uuid.UUID) {
	app, ok := am.Load(id)
	if !ok {
		return
	}

	am.nameIndex.Delete(app.Name)
	am.Delete(id)
}

// add a given app to the app manager
func (am *AppManager) AddApp(id uuid.UUID, app *App) {
	if app.Deployment.Containers == nil || app.Deployment.Head == nil || len(app.Deployment.Containers) == 0 || app.Name == "" {
		panic("invalid app")
	}

	am.nameIndex.Store(app.Name, id)
	am.Store(id, app)
}

// nukes an app completely
func (am *AppManager) DeleteApp(id uuid.UUID) error {
	app := am.GetApp(id)
	if app == nil {
		return fmt.Errorf("app not found")
	}

	// calls RemoveApp
	err := app.Remove(context.Background())
	if err != nil {
		return err
	}

	return nil
}

// Scan every app in the database, and create in memory structures if the deployment is already running
func (am *AppManager) Init() {
	logger.Info("Initializing deployments")

	if Flux.db == nil {
		logger.Panic("DB is nil")
	}

	rows, err := Flux.db.Query("SELECT id, name, deployment_id FROM apps")
	if err != nil {
		logger.Warnw("Failed to query apps", zap.Error(err))
		return
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var app App
		var appIdBlob []byte
		if err := rows.Scan(&appIdBlob, &app.Name, &app.DeploymentID); err != nil {
			logger.Warnw("Failed to scan app", zap.Error(err))
			return
		}
		app.Id = uuid.Must(uuid.FromBytes(appIdBlob))
		app.flux = Flux
		apps = append(apps, app)
	}

	for _, app := range apps {
		deployment := &Deployment{}
		var headContainer *Container
		Flux.db.QueryRow("SELECT id, url, port FROM deployments WHERE id = ?", app.DeploymentID).Scan(&deployment.ID, &deployment.URL, &deployment.Port)
		deployment.Containers = make([]*Container, 0)

		rows, err = Flux.db.Query("SELECT id, container_id, deployment_id, head FROM containers WHERE deployment_id = ?", app.DeploymentID)
		if err != nil {
			logger.Warnw("Failed to query containers", zap.Error(err))
			return
		}
		defer rows.Close()

		for rows.Next() {
			var container Container
			var containerIDString string
			rows.Scan(&container.ID, &containerIDString, &container.DeploymentID, &container.Head)
			container.Deployment = deployment
			copy(container.ContainerID[:], containerIDString)

			if container.Head {
				if headContainer != nil {
					logger.Fatal("Several containers are marked as head")
				}

				headContainer = &container
			}

			rows, err := Flux.db.Query("SELECT id, volume_id, container_id, mountpoint FROM volumes WHERE container_id = ?", container.ContainerID[:])
			if err != nil {
				logger.Warnw("Failed to query volumes", zap.Error(err))
				return
			}
			defer rows.Close()

			for rows.Next() {
				volume := new(Volume)
				rows.Scan(&volume.ID, &volume.VolumeID, &volume.ContainerID, &volume.Mountpoint)
				container.Volumes = append(container.Volumes, volume)
			}

			deployment.Containers = append(deployment.Containers, &container)
		}

		if headContainer == nil {
			logger.Fatal("head container is nil!")
		}

		deployment.Head = headContainer
		app.Deployment = deployment
		am.AddApp(app.Id, &app)

		status, err := deployment.Status(context.Background())
		if err != nil {
			logger.Warnw("Failed to get deployment status", zap.Error(err))
			continue
		}

		if status != "running" {
			continue
		}

		deployment.Proxy, _ = deployment.NewDeploymentProxy()
		Flux.proxy.AddDeployment(deployment)
	}
}
