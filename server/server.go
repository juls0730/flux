package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	_ "embed"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/juls0730/flux/pkg"
	_ "github.com/mattn/go-sqlite3"
)

var (
	//go:embed schema.sql
	schemaBytes   []byte
	DefaultConfig = FluxServerConfig{
		Builder: "paketobuildpacks/builder-jammy-tiny",
		Compression: pkg.Compression{
			Enabled: false,
			Level:   0,
		},
	}
	Flux *FluxServer
)

type FluxServerConfig struct {
	Builder     string          `json:"builder"`
	Compression pkg.Compression `json:"compression"`
}

type FluxServer struct {
	config       FluxServerConfig
	db           *sql.DB
	proxy        *Proxy
	rootDir      string
	appManager   *AppManager
	dockerClient *client.Client
}

func NewServer() *FluxServer {
	Flux = new(FluxServer)

	var serverConfig FluxServerConfig

	Flux.rootDir = os.Getenv("FLUXD_ROOT_DIR")
	if Flux.rootDir == "" {
		Flux.rootDir = "/var/fluxd"
	}

	// parse config, if it doesnt exist, create it and use the default config
	configPath := filepath.Join(Flux.rootDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		if err := os.MkdirAll(Flux.rootDir, 0755); err != nil {
			log.Fatalf("Failed to create fluxd directory: %v\n", err)
		}

		configBytes, err := json.Marshal(DefaultConfig)
		if err != nil {
			log.Fatalf("Failed to marshal default config: %v\n", err)
		}

		log.Printf("Config file not found, creating default config file at %s\n", configPath)
		if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
			log.Fatalf("Failed to write config file: %v\n", err)
		}
	}

	configFile, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("Failed to read config file: %v\n", err)
	}

	if err := json.Unmarshal(configFile, &serverConfig); err != nil {
		log.Fatalf("Failed to parse config file: %v\n", err)
	}

	Flux.config = serverConfig

	Flux.dockerClient, err = client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		log.Fatalf("Failed to create docker client: %v\n", err)
	}

	log.Printf("Pulling builder image %s, this may take a while...\n", serverConfig.Builder)

	events, err := Flux.dockerClient.ImagePull(context.Background(), fmt.Sprintf("%s:latest", serverConfig.Builder), image.PullOptions{})
	if err != nil {
		log.Fatalf("Failed to pull builder image: %v\n", err)
	}

	// wait for the iamge to be pulled
	io.Copy(io.Discard, events)

	log.Printf("Successfully pulled builder image %s\n", serverConfig.Builder)

	if err := os.MkdirAll(filepath.Join(Flux.rootDir, "apps"), 0755); err != nil {
		log.Fatalf("Failed to create apps directory: %v\n", err)
	}

	Flux.db, err = sql.Open("sqlite3", filepath.Join(Flux.rootDir, "fluxd.db"))
	if err != nil {
		log.Fatalf("Failed to open database: %v\n", err)
	}

	_, err = Flux.db.Exec(string(schemaBytes))
	if err != nil {
		log.Fatalf("Failed to create database schema: %v\n", err)
	}

	Flux.appManager = new(AppManager)
	Flux.appManager.Init()

	Flux.proxy = &Proxy{}

	port := os.Getenv("FLUXD_PROXY_PORT")
	if port == "" {
		port = "7465"
	}

	go func() {
		log.Printf("Proxy server starting on http://127.0.0.1:%s\n", port)
		if err := http.ListenAndServe(fmt.Sprintf(":%s", port), Flux.proxy); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Proxy server error: %v", err)
		}
	}()

	return Flux
}

func (s *FluxServer) UploadAppCode(code io.Reader, projectConfig pkg.ProjectConfig) (string, error) {
	var err error
	projectPath := filepath.Join(s.rootDir, "apps", projectConfig.Name)
	if err = os.MkdirAll(projectPath, 0755); err != nil {
		log.Printf("Failed to create project directory: %v\n", err)
		return "", err
	}

	var gzReader *gzip.Reader
	defer func() {
		if gzReader != nil {
			gzReader.Close()
		}
	}()

	if s.config.Compression.Enabled {
		gzReader, err = gzip.NewReader(code)
		if err != nil {
			log.Printf("Failed to create gzip reader: %v\n", err)
			return "", err
		}
	}
	var tarReader *tar.Reader

	if gzReader != nil {
		tarReader = tar.NewReader(gzReader)
	} else {
		tarReader = tar.NewReader(code)
	}

	log.Printf("Extracting files for %s...\n", projectPath)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Failed to read tar header: %v\n", err)
			return "", err
		}

		// Construct full path
		path := filepath.Join(projectPath, header.Name)

		// Handle different file types
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(path, 0755); err != nil {
				log.Printf("Failed to extract directory: %v\n", err)
				return "", err
			}
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				log.Printf("Failed to extract directory: %v\n", err)
				return "", err
			}

			outFile, err := os.Create(path)
			if err != nil {
				log.Printf("Failed to extract file: %v\n", err)
				return "", err
			}
			defer outFile.Close()

			if _, err = io.Copy(outFile, tarReader); err != nil {
				log.Printf("Failed to copy file during extraction: %v\n", err)
				return "", err
			}
		}
	}

	return projectPath, nil
}
