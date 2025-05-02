package appManagerService

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/juls0730/flux/internal/docker"
	models "github.com/juls0730/flux/internal/models"
	proxyManagerService "github.com/juls0730/flux/internal/services/proxy"
	"github.com/juls0730/flux/internal/util"
	"github.com/juls0730/flux/pkg"
	"go.uber.org/zap"
)

type AppManager struct {
	util.TypedMap[uuid.UUID, *models.App]
	nameIndex    util.TypedMap[string, uuid.UUID]
	logger       *zap.SugaredLogger
	proxyManager *proxyManagerService.ProxyManager
	dockerClient *docker.DockerClient
	db           *sql.DB
}

func NewAppManager(db *sql.DB, dockerClient *docker.DockerClient, proxyManager *proxyManagerService.ProxyManager, logger *zap.SugaredLogger) *AppManager {
	return &AppManager{
		db:           db,
		dockerClient: dockerClient,
		proxyManager: proxyManager,
		logger:       logger,
	}
}

func (appManager *AppManager) CreateApp(ctx context.Context, imageName string, projectConfig *pkg.ProjectConfig, id uuid.UUID) (*models.App, error) {
	app := models.App{
		Id: id,
	}
	appManager.logger.Debugw("Creating deployment", zap.String("id", app.Id.String()))

	app.Deployment = models.NewDeployment()
	if app.Deployment == nil {
		appManager.logger.Errorw("Failed to create deployment")
		return nil, fmt.Errorf("failed to create deployment")
	}

	if err := appManager.db.QueryRowContext(ctx, "INSERT INTO deployments (url, port) VALUES ($1, $2) RETURNING id, url, port", projectConfig.Url, projectConfig.Port).Scan(&app.Deployment.ID, &app.Deployment.URL, &app.Deployment.Port); err != nil {
		appManager.logger.Errorw("Failed to create deployment", zap.Error(err))
		return nil, err
	}

	for _, container := range projectConfig.Containers {
		// Create a container given a container configuration and a deployment. This will do a few things:
		// 1. Create the container in the docker daemon
		// 2. Create the volumes for the container
		// 3. Insert the container and volumes into the database
		c, err := models.CreateContainer(ctx, container.ImageName, container.Name, false, container.Environment, container.Volumes, app.Deployment, appManager.logger, appManager.dockerClient, appManager.db)
		if err != nil {
			appManager.logger.Errorw("Failed to create container", zap.Error(err))
			return nil, fmt.Errorf("failed to create container: %v", err)
		}

		c.Start(ctx, true, appManager.db, appManager.dockerClient, appManager.logger)
	}

	_, err := models.CreateContainer(ctx, imageName, projectConfig.Name, true, projectConfig.Environment, projectConfig.Volumes, app.Deployment, appManager.logger, appManager.dockerClient, appManager.db)
	if err != nil {
		appManager.logger.Errorw("Failed to create container", zap.Error(err))
		return nil, fmt.Errorf("failed to create container: %v", err)
	}

	var appIdBlob []byte
	err = appManager.db.QueryRow("INSERT INTO apps (id, name, deployment_id) VALUES ($1, $2, $3) RETURNING id, name, deployment_id", app.Id[:], projectConfig.Name, app.Deployment.ID).Scan(&appIdBlob, &app.Name, &app.DeploymentID)
	if err != nil {
		appManager.logger.Errorw("Failed to insert app", zap.Error(err))
		return nil, fmt.Errorf("failed to insert app: %v", err)
	}
	app.Id = uuid.Must(uuid.FromBytes(appIdBlob))

	err = app.Deployment.Start(ctx, appManager.dockerClient)
	if err != nil {
		appManager.logger.Errorw("Failed to start deployment", zap.Error(err))
		return nil, fmt.Errorf("failed to start deployment: %v", err)
	}

	appManager.AddApp(app.Id, app)
	appPtr := appManager.GetApp(app.Id)

	deploymentInternalUrl, err := app.Deployment.GetInternalUrl(appManager.dockerClient)
	if err != nil {
		appManager.logger.Errorw("Failed to get internal url", zap.Error(err))
		return nil, fmt.Errorf("failed to get internal url: %v", err)
	}

	newProxy, err := proxyManagerService.NewDeploymentProxy(*deploymentInternalUrl)
	if err != nil {
		appManager.logger.Errorw("Failed to create deployment proxy", zap.Error(err))
		return nil, fmt.Errorf("failed to create deployment proxy: %v", err)
	}

	appManager.proxyManager.AddProxy(appPtr.Deployment.URL, newProxy)

	return appPtr, nil
}

func (appManager *AppManager) Upgrade(ctx context.Context, appId uuid.UUID, imageName string, projectConfig *pkg.ProjectConfig) error {
	appManager.logger.Debugw("Upgrading app", zap.String("app_id", appId.String()), zap.String("image_name", imageName))

	app := appManager.GetApp(appId)
	if app == nil {
		appManager.logger.Errorw("App not found, but upgrade called", zap.String("app_id", appId.String()))
		return fmt.Errorf("failed to get app")
	}

	deploymentStatus, err := app.Deployment.Status(ctx, appManager.dockerClient, appManager.logger)
	if err != nil {
		appManager.logger.Errorw("Failed to get deployment status", zap.Error(err))
		return fmt.Errorf("failed to get deployment status: %v", err)
	}

	if deploymentStatus != "running" {
		err = app.Deployment.Start(ctx, appManager.dockerClient)
		if err != nil {
			appManager.logger.Errorw("Failed to start deployment", zap.Error(err))
			return fmt.Errorf("failed to start deployment: %v", err)
		}
	}

	err = app.Deployment.Upgrade(ctx, projectConfig, imageName, appManager.dockerClient, appManager.proxyManager, appManager.db, appManager.logger)
	if err != nil {
		appManager.logger.Errorw("Failed to upgrade deployment", zap.Error(err))
		return fmt.Errorf("failed to upgrade deployment: %v", err)
	}

	return nil
}

func (am *AppManager) GetAppByName(name string) *models.App {
	id, ok := am.nameIndex.Load(name)
	if !ok {
		return nil
	}

	return am.GetApp(id)
}

func (am *AppManager) GetApp(id uuid.UUID) *models.App {
	app, exists := am.Load(id)
	if !exists {
		return nil
	}

	return app
}

func (am *AppManager) GetAllApps() []*models.App {
	var apps []*models.App
	am.Range(func(key uuid.UUID, app *models.App) bool {
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
func (am *AppManager) AddApp(id uuid.UUID, app models.App) {
	if app.Deployment == nil || app.Deployment.Containers() == nil || app.Deployment.Head() == nil || len(app.Deployment.Containers()) == 0 || app.Name == "" {
		panic("invalid app")
	}

	am.nameIndex.Store(app.Name, id)
	am.Store(id, &app)
}

// nukes an app completely
func (am *AppManager) DeleteApp(id uuid.UUID) error {
	app := am.GetApp(id)
	if app == nil {
		return fmt.Errorf("app not found")
	}

	am.logger.Debugw("Deleting app", zap.String("id", id.String()))

	// calls RemoveApp
	err := app.Remove(context.Background(), am.dockerClient, am.db, am.logger)
	if err != nil {
		am.logger.Errorw("Failed to remove app", zap.Error(err))
		return err
	}

	return nil
}

// Scan every app in the database, and create in memory structures if the deployment is already running
func (am *AppManager) Init() {
	am.logger.Info("Initializing deployments")

	if am.db == nil {
		am.logger.Panic("DB is nil")
	}

	appRows, err := am.db.Query("SELECT id, name, deployment_id FROM apps")
	if err != nil {
		am.logger.Errorw("Failed to get apps", zap.Error(err))
		return
	}
	defer appRows.Close()

	var apps []models.App
	for appRows.Next() {
		var app models.App
		var appIdBlob []byte
		if err := appRows.Scan(&appIdBlob, &app.Name, &app.DeploymentID); err != nil {
			am.logger.Warnw("Failed to scan app", zap.Error(err))
			return
		}
		app.Id = uuid.Must(uuid.FromBytes(appIdBlob))
		app.Deployment = models.NewDeployment()
		if app.Deployment == nil {
			am.logger.Errorw("Failed to create deployment")
			return
		}

		err := am.db.QueryRow("SELECT id, url, port FROM deployments WHERE id = ?", app.DeploymentID).Scan(&app.Deployment.ID, &app.Deployment.URL, &app.Deployment.Port)
		if err != nil {
			am.logger.Errorw("Failed to get deployment", zap.Error(err))
			return
		}
		am.logger.Debugw("Found deployment", zap.Int64("id", app.Deployment.ID))

		containerRows, err := am.db.Query("SELECT id, container_id, deployment_id, head FROM containers WHERE deployment_id = ?", app.DeploymentID)
		if err != nil {
			am.logger.Warnw("Failed to query containers", zap.Error(err))
			return
		}
		defer containerRows.Close()

		for containerRows.Next() {
			var container models.Container
			containerRows.Scan(&container.ID, &container.ContainerID, &container.DeploymentID, &container.Head)
			container.Deployment = app.Deployment

			volumeRows, err := am.db.Query("SELECT id, volume_id, container_id, mountpoint FROM volumes WHERE container_id = ?", container.ContainerID[:])
			if err != nil {
				am.logger.Warnw("Failed to query volumes", zap.Error(err))
				return
			}
			defer volumeRows.Close()

			for volumeRows.Next() {
				volume := new(models.Volume)
				volumeRows.Scan(&volume.ID, &volume.VolumeID, &volume.ContainerID, &volume.Mountpoint)
				container.Volumes = append(container.Volumes, volume)
			}

			app.Deployment.AppendContainer(&container)
		}

		// TODO: Store state of deployment in database and start it if it's stopped and should be started

		apps = append(apps, app)
	}

	for _, app := range apps {
		am.AddApp(app.Id, app)
		am.logger.Debugw("Added app", zap.String("id", app.Id.String()))
		status, err := app.Deployment.Status(context.Background(), am.dockerClient, am.logger)
		if err != nil {
			am.logger.Warnw("Failed to get deployment status", zap.Error(err))
			continue
		}

		if status != "running" {
			continue
		}

		proxyURL, err := app.Deployment.GetInternalUrl(am.dockerClient)
		if err != nil {
			am.logger.Errorw("Failed to parse proxy url", zap.Error(err))
			continue
		}

		proxy, err := proxyManagerService.NewDeploymentProxy(*proxyURL)
		if err != nil {
			am.logger.Errorw("Failed to create proxy", zap.Error(err))
			continue
		}

		am.proxyManager.AddProxy(app.Deployment.URL, proxy)
		am.logger.Debugw("Created proxy", zap.String("id", app.Id.String()))
	}
}
