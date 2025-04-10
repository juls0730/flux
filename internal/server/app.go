package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

type App struct {
	ID           int64       `json:"id,omitempty"`
	Deployment   *Deployment `json:"-"`
	Name         string      `json:"name,omitempty"`
	DeploymentID int64       `json:"deployment_id,omitempty"`
}

// Create the initial app row in the database and create and start the deployment. The app is the overarching data
// structure that contains all of the data for a project
func CreateApp(ctx context.Context, imageName string, projectPath string, projectConfig *pkg.ProjectConfig) (*App, error) {
	app := &App{
		Name: projectConfig.Name,
	}
	logger.Debugw("Creating deployment", zap.String("name", app.Name))

	deployment, err := CreateDeployment(projectConfig.Port, projectConfig.Url, Flux.db)
	app.Deployment = deployment
	if err != nil {
		logger.Errorw("Failed to create deployment", zap.Error(err))
		return nil, err
	}

	for _, container := range projectConfig.Containers {
		c, err := CreateContainer(ctx, &container, projectConfig.Name, false, deployment)
		if err != nil {
			return nil, fmt.Errorf("failed to create container: %v", err)
		}

		c.Start(ctx, true)
	}

	headContainer := &pkg.Container{
		Name:        projectConfig.Name,
		ImageName:   imageName,
		Volumes:     projectConfig.Volumes,
		Environment: projectConfig.Environment,
	}

	// this call does a lot for us, see it's documentation for more info
	_, err = CreateContainer(ctx, headContainer, projectConfig.Name, true, deployment)
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %v", err)
	}

	// create app in the database
	err = appInsertStmt.QueryRow(projectConfig.Name, deployment.ID).Scan(&app.ID, &app.Name, &app.DeploymentID)
	if err != nil {
		return nil, fmt.Errorf("failed to insert app: %v", err)
	}

	err = deployment.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to start deployment: %v", err)
	}

	Flux.appManager.AddApp(app.Name, app)

	return app, nil
}

func (app *App) Upgrade(ctx context.Context, imageName string, projectPath string, projectConfig *pkg.ProjectConfig) error {
	logger.Debugw("Upgrading deployment", zap.String("name", app.Name))

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

	err = app.Deployment.Upgrade(ctx, projectConfig, imageName, projectPath)
	if err != nil {
		return fmt.Errorf("failed to upgrade deployment: %v", err)
	}

	return nil
}

// delete an app and deployment from the database, and its project files from disk.
func (app *App) Remove(ctx context.Context) error {
	Flux.appManager.RemoveApp(app.Name)

	err := app.Deployment.Remove(ctx)
	if err != nil {
		logger.Errorw("Failed to remove deployment", zap.Error(err))
		return err
	}

	_, err = Flux.db.Exec("DELETE FROM apps WHERE id = ?", app.ID)
	if err != nil {
		logger.Errorw("Failed to delete app", zap.Error(err))
		return err
	}

	projectPath := filepath.Join(Flux.rootDir, "apps", app.Name)
	err = os.RemoveAll(projectPath)
	if err != nil {
		return fmt.Errorf("failed to remove project directory: %v", err)
	}

	return nil
}

type AppManager struct {
	sync.Map
}

func (am *AppManager) GetApp(name string) *App {
	app, exists := am.Load(name)
	if !exists {
		return nil
	}

	return app.(*App)
}

func (am *AppManager) GetAllApps() []*App {
	var apps []*App
	am.Range(func(key, value interface{}) bool {
		if app, ok := value.(*App); ok {
			apps = append(apps, app)
		}
		return true
	})
	return apps
}

// removes an app from the app manager
func (am *AppManager) RemoveApp(name string) {
	am.Delete(name)
}

// add a given app to the app manager
func (am *AppManager) AddApp(name string, app *App) {
	if app.Deployment.Containers == nil || app.Deployment.Head == nil || len(app.Deployment.Containers) == 0 {
		panic("nil containers")
	}

	am.Store(name, app)
}

// nukes an app completely
func (am *AppManager) DeleteApp(name string) error {
	app := am.GetApp(name)
	if app == nil {
		return fmt.Errorf("app not found")
	}

	err := app.Remove(context.Background())
	if err != nil {
		return err
	}

	am.Delete(name)

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
		if err := rows.Scan(&app.ID, &app.Name, &app.DeploymentID); err != nil {
			logger.Warnw("Failed to scan app", zap.Error(err))
			return
		}
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
		am.AddApp(app.Name, &app)

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
