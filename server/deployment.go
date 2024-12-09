package server

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/juls0730/fluxd/pkg"
)

var (
	deploymentInsertStmt *sql.Stmt
	containerInsertStmt  *sql.Stmt
	volumeInsertStmt     *sql.Stmt
	updateVolumeStmt     *sql.Stmt
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

		deployment.Proxy, err = NewDeploymentProxy(&deployment, headContainer)
		if err != nil {
			log.Printf("Failed to create deployment proxy: %v\n", err)
			return
		}

		app.Deployment = deployment

		am.AddApp(app.Name, &app)
	}
}

// Creates a deployment and containers in the database
func CreateDeployment(container Container, port uint16, appUrl string, db *sql.DB) (Deployment, error) {
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

	if containerInsertStmt == nil {
		containerInsertStmt, err = db.Prepare("INSERT INTO containers (container_id, deployment_id, head) VALUES ($1, $2, $3) RETURNING id, container_id, deployment_id, head")
		if err != nil {
			log.Printf("Failed to prepare statement: %v\n", err)
			return Deployment{}, err
		}
	}

	var containerIDString string
	err = containerInsertStmt.QueryRow(container.ContainerID[:], deployment.ID, true).Scan(&container.ID, &containerIDString, &container.DeploymentID, &container.Head)
	if err != nil {
		log.Printf("Failed to get container id: %v\n", err)
		return Deployment{}, err
	}
	copy(container.ContainerID[:], containerIDString)

	for i, volume := range container.Volumes {
		if volumeInsertStmt == nil {
			volumeInsertStmt, err = db.Prepare("INSERT INTO volumes (volume_id, container_id) VALUES (?, ?) RETURNING id, volume_id, container_id")
			if err != nil {
				log.Printf("Failed to prepare statement: %v\n", err)
				return Deployment{}, err
			}
		}

		if err := volumeInsertStmt.QueryRow(volume.VolumeID, container.ID).Scan(&container.Volumes[i].ID, &container.Volumes[i].VolumeID, &container.Volumes[i].ContainerID); err != nil {
			log.Printf("Failed to insert volume: %v\n", err)
			return Deployment{}, err
		}
	}

	container.Deployment = &deployment
	deployment.Containers = append(deployment.Containers, container)

	return deployment, nil
}

func (deployment *Deployment) Upgrade(ctx context.Context, projectConfig pkg.ProjectConfig, imageName string, projectPath string) error {
	existingContainers, err := findExistingDockerContainers(ctx, projectConfig.Name)
	if err != nil {
		return fmt.Errorf("Failed to find existing containers: %v", err)
	}

	// Deploy new container before deleting old one
	c, err := CreateDockerContainer(ctx, imageName, projectPath, projectConfig)
	if err != nil || c == nil {
		log.Printf("Failed to create container: %v\n", err)
		return err
	}

	var container Container = *c
	if containerInsertStmt == nil {
		containerInsertStmt, err = Flux.db.Prepare("INSERT INTO containers (container_id, deployment_id, head) VALUES ($1, $2, $3) RETURNING id, container_id, deployment_id, head")
		if err != nil {
			log.Printf("Failed to prepare statement: %v\n", err)
			return err
		}
	}

	var containerIDString string
	err = containerInsertStmt.QueryRow(container.ContainerID[:], deployment.ID, true).Scan(&container.ID, &containerIDString, &container.DeploymentID, &container.Head)
	if err != nil {
		log.Printf("Failed to get container id: %v\n", err)
		return err
	}
	container.Deployment = deployment

	// the space time complexity of this is pretty bad, but it works
	for _, existingContainer := range deployment.Containers {
		if !existingContainer.Head {
			continue
		}

		for _, volume := range existingContainer.Volumes {
			var targetVolume *Volume
			for i, volume := range container.Volumes {
				if volume.VolumeID == volume.VolumeID {
					targetVolume = &container.Volumes[i]
					break
				}
			}

			if updateVolumeStmt == nil {
				updateVolumeStmt, err = Flux.db.Prepare("UPDATE volumes SET container_id = ? WHERE id = ? RETURNING id, volume_id, container_id")
				if err != nil {
					log.Printf("Failed to prepare statement: %v\n", err)
					return err
				}
			}

			err := updateVolumeStmt.QueryRow(container.ID, volume.ID).Scan(&targetVolume.ID, &targetVolume.VolumeID, &targetVolume.ContainerID)
			if err != nil {
				log.Printf("Failed to update volume: %v\n", err)
				return err
			}
		}
	}

	copy(container.ContainerID[:], containerIDString)
	deployment.Containers = append(deployment.Containers, container)

	log.Printf("Starting container %s...\n", container.ContainerID[:])
	err = container.Start(ctx)
	if err != nil {
		log.Printf("Failed to start container: %v\n", err)
		return err
	}

	if err := container.Wait(ctx, projectConfig.Port); err != nil {
		log.Printf("Failed to wait for container: %v\n", err)
		return err
	}

	tx, err := Flux.db.Begin()
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

	// Create a new proxy that points to the new head, and replace the old one, but ensure that the old one is gracefully shutdown
	oldProxy := deployment.Proxy
	deployment.Proxy, err = NewDeploymentProxy(deployment, &container)
	if err != nil {
		log.Printf("Failed to create deployment proxy: %v\n", err)
		return err
	}

	tx, err = Flux.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		return err
	}

	var containers []Container
	var oldContainers []*Container
	for _, container := range deployment.Containers {
		if existingContainers[string(container.ContainerID[:])] {
			log.Printf("Deleting container from db: %s\n", container.ContainerID[0:12])

			_, err = tx.Exec("DELETE FROM containers WHERE id = ?", container.ID)
			oldContainers = append(oldContainers, &container)

			if err != nil {
				tx.Rollback()
				return err
			}

			continue
		}

		containers = append(containers, container)
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		return err
	}

	if oldProxy != nil {
		go oldProxy.GracefulShutdown(oldContainers)
	} else {
		for _, container := range oldContainers {
			err := RemoveDockerContainer(context.Background(), string(container.ContainerID[:]))
			if err != nil {
				log.Printf("Failed to remove container: %v\n", err)
			}
		}
	}

	deployment.Containers = containers

	Flux.proxy.AddDeployment(deployment)

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

func (d *Deployment) Remove(ctx context.Context) error {
	for _, container := range d.Containers {
		err := container.Remove(ctx)
		if err != nil {
			log.Printf("Failed to remove container (%s): %v\n", container.ContainerID[:12], err)
			return err
		}
	}

	_, err := Flux.db.Exec("DELETE FROM deployments WHERE id = ?", d.ID)
	if err != nil {
		log.Printf("Failed to delete deployment: %v\n", err)
		return err
	}

	return nil
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
