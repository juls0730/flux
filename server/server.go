package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	_ "embed"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/juls0730/flux/pkg"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	Flux   *FluxServer
	logger *zap.SugaredLogger
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
	Logger       *zap.SugaredLogger
}

func NewFluxServer() *FluxServer {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		logger.Fatalw("Failed to create docker client", zap.Error(err))
	}

	rootDir := os.Getenv("FLUXD_ROOT_DIR")
	if rootDir == "" {
		rootDir = "/var/fluxd"
	}

	if err := os.MkdirAll(rootDir, 0755); err != nil {
		logger.Fatalw("Failed to create fluxd directory", zap.Error(err))
	}

	db, err := sql.Open("sqlite3", filepath.Join(rootDir, "fluxd.db"))
	if err != nil {
		logger.Fatalw("Failed to open database", zap.Error(err))
	}

	_, err = db.Exec(string(schemaBytes))
	if err != nil {
		logger.Fatalw("Failed to create database schema", zap.Error(err))
	}

	return &FluxServer{
		db:           db,
		proxy:        &Proxy{},
		appManager:   new(AppManager),
		rootDir:      rootDir,
		dockerClient: dockerClient,
	}
}

func (s *FluxServer) Stop() {
	s.Logger.Sync()
}

func NewServer() *FluxServer {
	verbosity, err := strconv.Atoi(os.Getenv("FLUXD_VERBOSITY"))
	if err != nil {
		verbosity = 0
	}

	config := zap.NewProductionConfig()

	if os.Getenv("DEBUG") == "true" {
		config = zap.NewDevelopmentConfig()
		verbosity = -1
	}

	config.Level = zap.NewAtomicLevelAt(zapcore.Level(verbosity))

	lameLogger, err := config.Build()
	logger = lameLogger.Sugar()

	if err != nil {
		logger.Fatalw("Failed to create logger", zap.Error(err))
	}

	Flux = NewFluxServer()
	Flux.Logger = logger

	var serverConfig FluxServerConfig

	// parse config, if it doesnt exist, create it and use the default config
	configPath := filepath.Join(Flux.rootDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		if err := os.MkdirAll(Flux.rootDir, 0755); err != nil {
			logger.Fatalw("Failed to create fluxd directory", zap.Error(err))
		}

		configBytes, err := json.Marshal(DefaultConfig)
		if err != nil {
			logger.Fatalw("Failed to marshal default config", zap.Error(err))
		}

		logger.Debugw("Config file not found creating default config file at", zap.String("path", configPath))
		if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
			logger.Fatalw("Failed to write config file", zap.Error(err))
		}
	}

	configFile, err := os.ReadFile(configPath)
	if err != nil {
		logger.Fatalw("Failed to read config file", zap.Error(err))
	}

	if err := json.Unmarshal(configFile, &serverConfig); err != nil {
		logger.Fatalw("Failed to parse config file", zap.Error(err))
	}

	Flux.config = serverConfig

	logger.Infof("Pulling builder image %s this may take a while...", serverConfig.Builder)
	events, err := Flux.dockerClient.ImagePull(context.Background(), fmt.Sprintf("%s:latest", serverConfig.Builder), image.PullOptions{})
	if err != nil {
		logger.Fatalw("Failed to pull builder image", zap.Error(err))
	}

	// blocking wait for the iamge to be pulled
	io.Copy(io.Discard, events)

	logger.Infow("Successfully pulled builder image", zap.String("image", serverConfig.Builder))

	if err := os.MkdirAll(filepath.Join(Flux.rootDir, "apps"), 0755); err != nil {
		logger.Fatalw("Failed to create apps directory", zap.Error(err))
	}

	Flux.appManager.Init()

	port := os.Getenv("FLUXD_PROXY_PORT")
	if port == "" {
		port = "7465"
	}

	go func() {
		logger.Infof("Proxy server starting on http://127.0.0.1:%s", port)
		if err := http.ListenAndServe(fmt.Sprintf(":%s", port), Flux.proxy); err != nil && err != http.ErrServerClosed {
			logger.Fatalw("Proxy server error", zap.Error(err))
		}
	}()

	return Flux
}

func (s *FluxServer) UploadAppCode(code io.Reader, projectConfig pkg.ProjectConfig) (string, error) {
	var err error
	projectPath := filepath.Join(s.rootDir, "apps", projectConfig.Name)
	if err = os.MkdirAll(projectPath, 0755); err != nil {
		logger.Errorw("Failed to create project directory", zap.Error(err))
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
			logger.Infow("Failed to create gzip reader", zap.Error(err))
			return "", err
		}
	}
	var tarReader *tar.Reader

	if gzReader != nil {
		tarReader = tar.NewReader(gzReader)
	} else {
		tarReader = tar.NewReader(code)
	}

	logger.Infow("Extracting files for project", zap.String("project", projectPath))
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			logger.Debugw("Failed to read tar header", zap.Error(err))
			return "", err
		}

		// Construct full path
		path := filepath.Join(projectPath, header.Name)

		// Handle different file types
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(path, 0755); err != nil {
				logger.Debugw("Failed to extract directory", zap.Error(err))
				return "", err
			}
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				logger.Debugw("Failed to extract directory", zap.Error(err))
				return "", err
			}

			outFile, err := os.Create(path)
			if err != nil {
				logger.Debugw("Failed to extract file", zap.Error(err))
				return "", err
			}
			defer outFile.Close()

			if _, err = io.Copy(outFile, tarReader); err != nil {
				logger.Debugw("Failed to copy file during extraction", zap.Error(err))
				return "", err
			}
		}
	}

	return projectPath, nil
}
