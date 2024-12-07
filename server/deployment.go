package server

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"

	"github.com/juls0730/fluxd/pkg"
)

var (
	Apps                 *AppManager = new(AppManager)
	deploymentInsertStmt *sql.Stmt
	containerInsertStmt  *sql.Stmt
)

type AppManager struct {
	sync.Map
}

type App struct {
	ID           int64      `json:"id,omitempty"`
	Deployment   Deployment `json:"-"`
	Name         string     `json:"name,omitempty"`
	DeploymentID int64      `json:"deployment_id,omitempty"`
}

type Deployment struct {
	ID         int64            `json:"id"`
	Containers []Container      `json:"-"`
	Proxy      *DeploymentProxy `json:"-"`
	URL        string           `json:"url"`
	Port       uint16           `json:"port"`
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

func (am *AppManager) DeleteApp(name string) {
	am.Delete(name)
}

func (am *AppManager) Init() {
	log.Printf("Initializing deployments...\n")

	if DB == nil {
		log.Panicf("DB is nil")
	}

	rows, err := DB.Query("SELECT id, name, deployment_id FROM apps")
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
		DB.QueryRow("SELECT id, url, port FROM deployments WHERE id = ?", app.DeploymentID).Scan(&deployment.ID, &deployment.URL, &deployment.Port)
		deployment.Containers = make([]Container, 0)

		rows, err := DB.Query("SELECT id, container_id, deployment_id, head FROM containers WHERE deployment_id = ?", app.DeploymentID)
		if err != nil {
			log.Printf("Failed to query containers: %v\n", err)
			return
		}

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

		deployment.Proxy, err = NewDeploymentProxy(&deployment, headContainer)
		if err != nil {
			log.Printf("Failed to create deployment proxy: %v\n", err)
			return
		}

		app.Deployment = deployment

		Apps.AddApp(app.Name, &app)
	}
}

// Creates a deployment and containers in the database
func CreateDeployment(containerID string, port uint16, appUrl string, db *sql.DB) (Deployment, error) {
	var deployment Deployment
	var err error

	if deploymentInsertStmt == nil {
		deploymentInsertStmt, err = db.Prepare("INSERT INTO deployments (url, port) VALUES ($1, $2) RETURNING id, url, port")
		if err != nil {
			log.Printf("Failed to prepare statement: %v\n", err)
			return Deployment{}, err
		}
	}

	err = deploymentInsertStmt.QueryRow(appUrl, port).Scan(&deployment.ID, &deployment.URL, &deployment.Port)
	if err != nil {
		log.Printf("Failed to insert deployment: %v\n", err)
		return Deployment{}, err
	}

	var container Container
	if containerInsertStmt == nil {
		containerInsertStmt, err = db.Prepare("INSERT INTO containers (container_id, deployment_id, head) VALUES ($1, $2, $3) RETURNING id, container_id, deployment_id, head")
		if err != nil {
			log.Printf("Failed to prepare statement: %v\n", err)
			return Deployment{}, err
		}
	}

	var containerIDString string
	err = containerInsertStmt.QueryRow(containerID, deployment.ID, true).Scan(&container.ID, &containerIDString, &container.DeploymentID, &container.Head)
	if err != nil {
		log.Printf("Failed to get container id: %v\n", err)
		return Deployment{}, err
	}
	copy(container.ContainerID[:], containerIDString)

	container.Deployment = &deployment
	deployment.Containers = append(deployment.Containers, container)

	return deployment, nil
}

func (deployment *Deployment) Upgrade(ctx context.Context, projectConfig pkg.ProjectConfig, imageName string, projectPath string, s *FluxServer) error {
	existingContainers, err := findExistingDockerContainers(ctx, projectConfig.Name)
	if err != nil {
		return fmt.Errorf("Failed to find existing containers: %v", err)
	}

	// Deploy new container before deleting old one
	containerID, err := CreateDockerContainer(ctx, imageName, projectPath, projectConfig)
	if err != nil {
		log.Printf("Failed to create container: %v\n", err)
		return err
	}

	var container Container
	if containerInsertStmt == nil {
		containerInsertStmt, err = DB.Prepare("INSERT INTO containers (container_id, deployment_id, head) VALUES ($1, $2, $3) RETURNING id, container_id, deployment_id, head")
		if err != nil {
			log.Printf("Failed to prepare statement: %v\n", err)
			return err
		}
	}

	var containerIDString string
	err = containerInsertStmt.QueryRow(containerID, deployment.ID, true).Scan(&container.ID, &containerIDString, &container.DeploymentID, &container.Head)
	if err != nil {
		log.Printf("Failed to get container id: %v\n", err)
		return err
	}
	container.Deployment = deployment

	copy(container.ContainerID[:], containerIDString)
	deployment.Containers = append(deployment.Containers, container)

	log.Printf("Starting container %s...\n", containerID)
	err = container.Start(ctx)
	if err != nil {
		log.Printf("Failed to start container: %v\n", err)
		return err
	}

	if err := container.Wait(ctx, projectConfig.Port); err != nil {
		log.Printf("Failed to wait for container: %v\n", err)
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		return err
	}

	if _, err := tx.Exec("UPDATE deployments SET url = ?, port = ? WHERE id = ?", projectConfig.Url, projectConfig.Port, deployment.ID); err != nil {
		log.Printf("Failed to update deployment: %v\n", err)
		tx.Rollback()
		return err
	}

	if _, err := tx.Exec("UPDATE apps SET deployment_id = ? WHERE name = ?", deployment.ID, projectConfig.Name); err != nil {
		log.Printf("Failed to update app: %v\n", err)
		tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		return err
	}

	tx, err = s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		return err
	}

	// Create a new proxy that points to the new head, and replace the old one, but ensure that the old one is gracefully shutdown
	oldProxy := deployment.Proxy
	deployment.Proxy, err = NewDeploymentProxy(deployment, &container)
	if err != nil {
		log.Printf("Failed to create deployment proxy: %v\n", err)
		return err
	}

	var containers []Container
	var oldContainers []*Container
	for _, container := range deployment.Containers {
		if existingContainers[string(container.ContainerID[:])] {
			log.Printf("Stopping existing container: %s\n", container.ContainerID[0:12])

			_, err = tx.Exec("DELETE FROM containers WHERE container_id = ?", string(container.ContainerID[:]))
			oldContainers = append(oldContainers, &container)

			if err != nil {
				tx.Rollback()
				return err
			}

			continue
		}

		containers = append(containers, container)
	}

	if oldProxy != nil {
		go oldProxy.GracefulShutdown(oldContainers)
	}

	deployment.Containers = containers

	ReverseProxy.AddDeployment(deployment)

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		return err
	}

	return nil
}

func arrayContains(arr []string, str string) bool {
	for _, a := range arr {
		if a == str {
			return true
		}
	}

	return false
}

func (d *Deployment) Start(ctx context.Context) error {
	for _, container := range d.Containers {
		err := container.Start(ctx)
		if err != nil {
			log.Printf("Failed to start container: %v\n", err)
			return err
		}
	}

	return nil
}

func (d *Deployment) Stop(ctx context.Context) error {
	for _, container := range d.Containers {
		err := container.Stop(ctx)
		if err != nil {
			log.Printf("Failed to start container: %v\n", err)
			return err
		}
	}

	return nil
}

func (c *Container) GetStatus(ctx context.Context) (string, error) {
	containerJSON, err := dockerClient.ContainerInspect(ctx, string(c.ContainerID[:]))
	if err != nil {
		return "", err
	}

	return containerJSON.State.Status, nil
}

func (d *Deployment) Status(ctx context.Context) (string, error) {
	var status string
	if d == nil {
		fmt.Printf("Deployment is nil\n")
		return "stopped", nil
	}

	if d.Containers == nil {
		fmt.Printf("Containers are nil\n")
		return "stopped", nil
	}

	for _, container := range d.Containers {
		containerStatus, err := container.GetStatus(ctx)
		if err != nil {
			log.Printf("Failed to get container status: %v\n", err)
			return "", err
		}

		// if not all containers are in the same state
		if status != "" && status != containerStatus {
			return "", fmt.Errorf("Malformed deployment")
		}

		status = containerStatus
	}

	switch status {
	case "running":
		return "running", nil
	case "exited":
		return "stopped", nil
	default:
		return "pending", nil
	}
}
