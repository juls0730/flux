package server

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"embed"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schema embed.FS

var DefaultConfig = FluxServerConfig{
	Builder: "paketobuildpacks/builder-jammy-tiny",
}

type FluxServerConfig struct {
	Builder string `json:"builder"`
}

type FluxServer struct {
	containerManager *ContainerManager
	config           FluxServerConfig
	db               *sql.DB
}

var rootDir string

func init() {
	rootDir = os.Getenv("FLUXD_ROOT_DIR")
	if rootDir == "" {
		rootDir = "/var/fluxd"
	}
}

func NewServer() *FluxServer {
	containerManager := NewContainerManager()

	var serverConfig FluxServerConfig

	// parse config, if it doesnt exist, create it and use the default config
	configPath := filepath.Join(rootDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		if err := os.MkdirAll(rootDir, 0755); err != nil {
			log.Fatalf("Failed to create fluxd directory: %v", err)
		}

		configBytes, err := json.Marshal(DefaultConfig)
		if err != nil {
			log.Fatalf("Failed to marshal default config: %v", err)
		}

		log.Printf("Config file not found, creating default config file at %s", configPath)
		if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
			log.Fatalf("Failed to write config file: %v", err)
		}
	}

	configFile, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("Failed to read config file: %v", err)
	}

	if err := json.Unmarshal(configFile, &serverConfig); err != nil {
		log.Fatalf("Failed to parse config file: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(rootDir, "apps"), 0755); err != nil {
		log.Fatalf("Failed to create apps directory: %v", err)
	}

	db, err := sql.Open("sqlite3", filepath.Join(rootDir, "fluxd.db"))
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}

	// create database schema
	schemaBytes, err := schema.ReadFile("schema.sql")
	if err != nil {
		log.Fatalf("Failed to read schema file: %v", err)
	}

	_, err = db.Exec(string(schemaBytes))
	if err != nil {
		log.Fatalf("Failed to create database schema: %v", err)
	}

	return &FluxServer{
		containerManager: containerManager,
		config:           serverConfig,
		db:               db,
	}
}

func (s *FluxServer) UploadAppCode(code io.Reader, projectConfig ProjectConfig) (string, error) {
	projectPath := filepath.Join(rootDir, "apps", projectConfig.Name)
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		log.Printf("Failed to create project directory: %v", err)
		return "", err
	}

	gzReader, err := gzip.NewReader(code)
	if err != nil {
		log.Printf("Failed to create gzip reader: %v", err)
		return "", err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)

	// Extract files
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Failed to read tar header: %v", err)
			return "", err
		}

		// Construct full path
		path := filepath.Join(projectPath, header.Name)

		// Handle different file types
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0755); err != nil {
				log.Printf("Failed to extract directory: %v", err)
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				log.Printf("Failed to extract directory: %v", err)
				return "", err
			}

			outFile, err := os.Create(path)
			if err != nil {
				log.Printf("Failed to extract file: %v", err)
				return "", err
			}
			defer outFile.Close()

			if _, err := io.Copy(outFile, tarReader); err != nil {
				log.Printf("Failed to copy file during extraction: %v", err)
				return "", err
			}
		}
	}

	return projectPath, nil
}
