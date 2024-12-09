package server

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/juls0730/flux/pkg"
)

type App struct {
	ID           int64      `json:"id,omitempty"`
	Deployment   Deployment `json:"-"`
	Name         string     `json:"name,omitempty"`
	DeploymentID int64      `json:"deployment_id,omitempty"`
}

func CreateApp(ctx context.Context, imageName string, projectPath string, projectConfig pkg.ProjectConfig) (*App, error) {
	app := &App{
		Name: projectConfig.Name,
	}
	log.Printf("Creating deployment %s...\n", app.Name)

	container, err := CreateDockerContainer(ctx, imageName, projectPath, projectConfig)
	if err != nil || container == nil {

		return nil, fmt.Errorf("Failed to create container: %v", err)
	}

	deployment, err := CreateDeployment(*container, projectConfig.Port, projectConfig.Url, Flux.db)
	app.Deployment = deployment
	if err != nil {
		log.Printf("Failed to create deployment: %v", err)
		return nil, err
	}

	if appInsertStmt == nil {
		appInsertStmt, err = Flux.db.Prepare("INSERT INTO apps (name, deployment_id) VALUES ($1, $2) RETURNING id, name, deployment_id")
		if err != nil {
			return nil, fmt.Errorf("Failed to prepare statement: %v", err)
		}
	}

	// create app in the database
	err = appInsertStmt.QueryRow(projectConfig.Name, deployment.ID).Scan(&app.ID, &app.Name, &app.DeploymentID)
	if err != nil {
		return nil, fmt.Errorf("Failed to insert app: %v", err)
	}

	err = deployment.Start(ctx)
	if err != nil {
		return nil, fmt.Errorf("Failed to start deployment: %v", err)
	}

	var headContainer *Container
	for _, container := range deployment.Containers {
		if container.Head {
			headContainer = &container
		}
	}

	deployment.Proxy, err = NewDeploymentProxy(&deployment, headContainer)
	if err != nil {
		return nil, fmt.Errorf("Failed to create deployment proxy: %v", err)
	}

	Flux.proxy.AddDeployment(&deployment)

	Flux.appManager.AddApp(app.Name, app)

	return app, nil
}

func (app *App) Upgrade(ctx context.Context, projectConfig pkg.ProjectConfig, imageName string, projectPath string) error {
	log.Printf("Upgrading deployment %s...\n", app.Name)

	// if deploy is not started, start it
	deploymentStatus, err := app.Deployment.Status(ctx)
	if deploymentStatus != "running" || err != nil {
		err = app.Deployment.Start(ctx)
		if err != nil {
			return fmt.Errorf("Failed to start deployment: %v", err)
		}
	}

	err = app.Deployment.Upgrade(ctx, projectConfig, imageName, projectPath)
	if err != nil {
		return fmt.Errorf("Failed to upgrade deployment: %v", err)
	}

	return nil
}

func (app *App) Remove(ctx context.Context) error {
	err := app.Deployment.Remove(ctx)
	if err != nil {
		log.Printf("Failed to remove deployment: %v\n", err)
		return err
	}

	_, err = Flux.db.Exec("DELETE FROM apps WHERE id = ?", app.ID)
	if err != nil {
		log.Printf("Failed to delete app: %v\n", err)
		return err
	}

	projectPath := filepath.Join(Flux.rootDir, "apps", app.Name)
	err = os.RemoveAll(projectPath)
	if err != nil {
		return fmt.Errorf("Failed to remove project directory: %v", err)
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

func (am *AppManager) AddApp(name string, app *App) {
	am.Store(name, app)
}

func (am *AppManager) DeleteApp(name string) error {
	app := am.GetApp(name)
	if app == nil {
		return fmt.Errorf("App not found")
	}

	err := app.Remove(context.Background())
	if err != nil {
		return err
	}

	am.Delete(name)

	return nil
}

func (am *AppManager) Init(db *sql.DB) {
	log.Printf("Initializing deployments...\n")

	if db == nil {
		log.Panicf("DB is nil")
	}

	rows, err := db.Query("SELECT id, name, deployment_id FROM apps")
	if err != nil {
		log.Printf("Failed to query apps: %v\n", err)
		return
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		var app App
		if err := rows.Scan(&app.ID, &app.Name, &app.DeploymentID); err != nil {
			log.Printf("Failed to scan app: %v\n", err)
			return
		}
		apps = append(apps, app)
	}

	for _, app := range apps {
		var deployment Deployment
		var headContainer *Container
		db.QueryRow("SELECT id, url, port FROM deployments WHERE id = ?", app.DeploymentID).Scan(&deployment.ID, &deployment.URL, &deployment.Port)
		deployment.Containers = make([]Container, 0)

		rows, err = db.Query("SELECT id, container_id, deployment_id, head FROM containers WHERE deployment_id = ?", app.DeploymentID)
		if err != nil {
			log.Printf("Failed to query containers: %v\n", err)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var container Container
			var containerIDString string
			rows.Scan(&container.ID, &containerIDString, &container.DeploymentID, &container.Head)
			container.Deployment = &deployment
			copy(container.ContainerID[:], containerIDString)

			if container.Head {
				headContainer = &container
			}

			deployment.Containers = append(deployment.Containers, container)
		}

		for i, container := range deployment.Containers {
			var volumes []Volume
			rows, err := db.Query("SELECT id, volume_id, container_id FROM volumes WHERE container_id = ?", container.ID)
			if err != nil {
				log.Printf("Failed to query volumes: %v\n", err)
				return
			}
			defer rows.Close()

			for rows.Next() {
				var volume Volume
				rows.Scan(&volume.ID, &volume.VolumeID, &volume.ContainerID)
				volumes = append(volumes, volume)
			}

			deployment.Containers[i].Volumes = volumes
		}

		deployment.Proxy, _ = NewDeploymentProxy(&deployment, headContainer)

		app.Deployment = deployment

		am.AddApp(app.Name, &app)
	}
}
