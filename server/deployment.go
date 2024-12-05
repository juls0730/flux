package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/juls0730/fluxd/models"
)

// Creates a deployment and containers in the database
func (s *FluxServer) CreateDeployment(ctx context.Context, projectConfig models.ProjectConfig, containerID string) (int64, error) {
	deploymentResult, err := s.db.Exec("INSERT INTO deployments (url) VALUES (?)", projectConfig.Url)
	if err != nil {
		log.Printf("Failed to insert deployment: %v\n", err)
		return 0, err
	}

	deploymentID, err := deploymentResult.LastInsertId()
	if err != nil {
		log.Printf("Failed to get deployment id: %v\n", err)
		return 0, err
	}

	_, err = s.db.Exec("INSERT INTO containers (container_id, deployment_id) VALUES (?, ?)", containerID, deploymentID)
	if err != nil {
		log.Printf("Failed to get container id: %v\n", err)
		return 0, err
	}

	return deploymentID, nil
}

func (s *FluxServer) UpgradeDeployment(ctx context.Context, deploymentID int64, projectConfig models.ProjectConfig, imageName string, projectPath string) error {
	configBytes, err := json.Marshal(projectConfig)
	if err != nil {
		log.Printf("Failed to marshal project config: %v\n", err)
		return err
	}

	existingContainers, err := s.containerManager.findExistingContainers(ctx, projectConfig.Name)
	if err != nil {
		return fmt.Errorf("Failed to find existing containers: %v", err)
	}

	fmt.Printf("There are %d existing containers\n", len(existingContainers))

	// Deploy new container before deleting old one
	containerID, err := s.containerManager.CreateContainer(ctx, imageName, projectPath, projectConfig)
	if err != nil {
		log.Printf("Failed to create container: %v\n", err)
		return err
	}

	// calls AddContainer in proxy
	err = s.containerManager.StartContainer(ctx, containerID)
	if err != nil {
		log.Printf("Failed to start container: %v\n", err)
		return err
	}

	if err := s.containerManager.WaitForContainer(ctx, containerID, projectConfig.Port); err != nil {
		log.Printf("Failed to wait for container: %v\n", err)
		return err
	}

	s.db.Exec("INSERT INTO containers (container_id, deployment_id) VALUES (?, ?)", containerID, deploymentID)

	// update app in the database
	if _, err := s.db.Exec("UPDATE apps SET project_config = ?, deployment_id = ? WHERE name = ?", configBytes, deploymentID, projectConfig.Name); err != nil {
		log.Printf("Failed to update app: %v\n", err)
		return err
	}

	// TODO: swap containers if they are running and have the same image so that we can have a constant uptime
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		return err
	}

	for _, existingContainer := range existingContainers {
		log.Printf("Stopping existing container: %s\n", existingContainer[0:12])

		tx.Exec("DELETE FROM containers WHERE container_id = ?", existingContainer)
		err = s.containerManager.GracefullyRemoveContainer(ctx, existingContainer)
		if err != nil {
			tx.Rollback()
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		return err
	}

	return nil
}

func (s *FluxServer) StartDeployment(ctx context.Context, deploymentID int64) error {
	var containerIds []string
	rows, err := s.db.Query("SELECT container_id FROM containers WHERE deployment_id = ?", deploymentID)
	if err != nil {
		log.Printf("Failed to query containers: %v\n", err)
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var newContainerId string
		if err := rows.Scan(&newContainerId); err != nil {
			log.Printf("Failed to scan container id: %v\n", err)
			return err
		}

		containerIds = append(containerIds, newContainerId)
	}

	var projectConfigStr []byte
	s.db.QueryRow("SELECT project_config FROM apps WHERE deployment_id = ?", deploymentID).Scan(&projectConfigStr)
	var projectConfig models.ProjectConfig
	if err := json.Unmarshal(projectConfigStr, &projectConfig); err != nil {
		return err
	}
	if projectConfig.Name == "" {
		return fmt.Errorf("No project config found for deployment")
	}

	for _, containerId := range containerIds {
		err := s.containerManager.StartContainer(ctx, containerId)
		s.Proxy.AddContainer(projectConfig, containerId)
		if err != nil {
			log.Printf("Failed to start container: %v\n", err)
			return err
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		return err
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		return err
	}

	return nil
}

func (s *FluxServer) StopDeployment(ctx context.Context, deploymentID int64) error {
	var containerIds []string
	rows, err := s.db.Query("SELECT container_id FROM containers WHERE deployment_id = ?", deploymentID)
	if err != nil {
		log.Printf("Failed to query containers: %v\n", err)
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var newContainerId string
		if err := rows.Scan(&newContainerId); err != nil {
			log.Printf("Failed to scan container id: %v\n", err)
			return err
		}

		containerIds = append(containerIds, newContainerId)
	}

	var projectConfigStr []byte
	s.db.QueryRow("SELECT project_config FROM apps WHERE deployment_id = ?", deploymentID).Scan(&projectConfigStr)
	var projectConfig models.ProjectConfig
	if err := json.Unmarshal(projectConfigStr, &projectConfig); err != nil {
		return err
	}
	if projectConfig.Name == "" {
		return fmt.Errorf("No project config found for deployment")
	}

	for _, containerId := range containerIds {
		err := s.containerManager.StopContainer(ctx, containerId)
		s.Proxy.RemoveContainer(containerId)
		if err != nil {
			log.Printf("Failed to start container: %v\n", err)
			return err
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("Failed to begin transaction: %v\n", err)
		return err
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Failed to commit transaction: %v\n", err)
		return err
	}

	return nil
}

func (s *FluxServer) GetStatus(ctx context.Context, containerID string) (string, error) {
	containerJSON, err := s.containerManager.dockerClient.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", err
	}

	return containerJSON.State.Status, nil
}

func (s *FluxServer) GetDeploymentStatus(ctx context.Context, deploymentID int64) (string, error) {
	var deployment models.Deployments
	s.db.QueryRow("SELECT id, url FROM deployments WHERE id = ?", deploymentID).Scan(&deployment.ID, &deployment.URL)

	var containerIds []string
	rows, err := s.db.Query("SELECT container_id FROM containers WHERE deployment_id = ?", deploymentID)
	if err != nil {
		log.Printf("Failed to query containers: %v\n", err)
		return "", err
	}
	defer rows.Close()

	for rows.Next() {
		var newContainerId string
		if err := rows.Scan(&newContainerId); err != nil {
			log.Printf("Failed to scan container id: %v\n", err)
			return "", err
		}

		containerIds = append(containerIds, newContainerId)
	}

	var status string
	for _, containerId := range containerIds {
		containerStatus, err := s.GetStatus(ctx, containerId)
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
