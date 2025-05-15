package handlers

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
	"time"

	_ "embed"

	"github.com/docker/docker/api/types/image"
	"github.com/google/uuid"
	"github.com/juls0730/flux/internal/docker"
	"github.com/juls0730/flux/internal/services/appManagerService"
	"github.com/juls0730/sentinel"

	"github.com/juls0730/flux/pkg"
	"github.com/juls0730/flux/pkg/API"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	//go:embed schema.sql
	schema        string
	DefaultConfig = pkg.DaemonConfig{
		Builder:          "paketobuildpacks/builder-jammy-tiny",
		CompressionLevel: 0,
		DaemonHost:       "0.0.0.0:5647",
		ProxyHost:        "0.0.0.0:7465",
	}
)

type FluxServer struct {
	db *sql.DB

	docker *docker.DockerClient

	proxy      *sentinel.ProxyManager
	appManager *appManagerService.AppManager

	rootDir string
	config  pkg.DaemonConfig
	logger  *zap.SugaredLogger
}

func NewServer() *FluxServer {
	flux := &FluxServer{}

	verbosity, err := strconv.Atoi(os.Getenv("FLUXD_VERBOSITY"))
	if err != nil {
		verbosity = 0
	}

	config := zap.NewProductionConfig()

	debug, err := strconv.ParseBool(os.Getenv("DEBUG"))
	if err != nil {
		debug = false
	}

	if debug {
		config = zap.NewDevelopmentConfig()
		verbosity = -1
	}

	config.Level = zap.NewAtomicLevelAt(zapcore.Level(verbosity))

	lameLogger, err := config.Build()
	if err != nil {
		fmt.Printf("Failed to create logger: %v\n", err)
		os.Exit(1)
	}

	flux.logger = lameLogger.Sugar()

	rootDir := os.Getenv("FLUXD_ROOT_DIR")
	if rootDir == "" {
		rootDir = "/var/fluxd"
	}

	flux.rootDir = rootDir

	configPath := filepath.Join(flux.rootDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		if err := os.MkdirAll(rootDir, 0755); err != nil {
			flux.logger.Fatalw("Failed to create fluxd directory", zap.Error(err))
		}

		configBytes, err := json.Marshal(DefaultConfig)
		if err != nil {
			flux.logger.Fatalw("Failed to marshal default config", zap.Error(err))
		}

		fmt.Printf("Config file not found creating default config file at %s\n", configPath)
		if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
			flux.logger.Fatalw("Failed to write config file", zap.Error(err))
		}
	}

	configFile, err := os.ReadFile(configPath)
	if err != nil {
		flux.logger.Fatalw("Failed to read config file", zap.Error(err))
	}

	// apply the config file over the default config, this way if we have missing fields, they will be filled in with
	// the default values
	flux.config = DefaultConfig
	if err := json.Unmarshal(configFile, &flux.config); err != nil {
		flux.logger.Fatalw("Failed to parse config file", zap.Error(err))
	}

	dbPath := filepath.Join(flux.rootDir, "fluxd.db")

	flux.db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		flux.logger.Fatalw("Failed to open database", zap.Error(err))
	}

	err = flux.db.Ping()
	if err != nil {
		flux.logger.Fatalw("Failed to ping database", zap.Error(err))
	}

	_, err = flux.db.Exec(schema)
	if err != nil {
		flux.logger.Fatalw("Failed to create database schema", zap.Error(err))
	}

	flux.docker = docker.NewDocker(nil, flux.logger)
	if err != nil {
		flux.logger.Fatalw("Failed to create docker client", zap.Error(err))
	}

	flux.logger.Infof("Pulling builder image %s this may take a while...", flux.config.Builder)
	events, err := flux.docker.ImagePull(context.Background(), fmt.Sprintf("%s:latest", flux.config.Builder), image.PullOptions{})
	if err != nil {
		flux.logger.Fatalw("Failed to pull builder image", zap.Error(err))
	}

	// blocking until the iamge is pulled
	io.Copy(io.Discard, events)

	requestLogger := &RequestLogger{logger: flux.logger}
	flux.proxy = sentinel.NewProxyManager(requestLogger)

	flux.appManager = appManagerService.NewAppManager(flux.db, flux.docker, flux.proxy, flux.logger)
	err = flux.appManager.Init()
	if err != nil {
		flux.logger.Fatalw("Failed to initialize apps", zap.Error(err))
	}

	return flux
}

func (s *FluxServer) Stop() {
	s.logger.Sync()
}

func (s *FluxServer) ListenAndServe() error {
	s.logger.Infow("Starting server", zap.String("daemon_host", s.config.DaemonHost), zap.String("proxy_host", s.config.ProxyHost))

	go func() {
		err := s.proxy.ListenAndServe(s.config.ProxyHost)
		if err != nil {
			s.logger.Errorw("Failed to start proxy", zap.Error(err))
		}
	}()
	return http.ListenAndServe(s.config.DaemonHost, nil)
}

func (s *FluxServer) DaemonInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(API.Info{
		CompressionLevel: s.config.CompressionLevel,
		Version:          pkg.Version,
	})
}

// This extracts and uploads a tar file to a temporary directory, and returns the path to the directory
func (s *FluxServer) UploadAppCode(code io.Reader, appId uuid.UUID) (string, error) {
	var err error
	outputPath, err := os.MkdirTemp(os.TempDir(), appId.String())
	if err != nil {
		s.logger.Errorw("Failed to create project directory", zap.Error(err))
		return "", err
	}

	var gzReader *gzip.Reader
	defer func() {
		if gzReader != nil {
			gzReader.Close()
		}
	}()

	if s.config.CompressionLevel > 0 {
		gzReader, err = gzip.NewReader(code)
		if err != nil {
			s.logger.Infow("Failed to create gzip reader", zap.Error(err))
			return "", err
		}
	}
	var tarReader *tar.Reader

	if gzReader != nil {
		tarReader = tar.NewReader(gzReader)
	} else {
		tarReader = tar.NewReader(code)
	}

	s.logger.Infow("Extracting files for project", zap.String("project", outputPath))
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.logger.Debugw("Failed to read tar header", zap.Error(err))
			return "", err
		}

		// Construct full path
		path := filepath.Join(outputPath, header.Name)

		// Handle different file types
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(path, 0755); err != nil {
				s.logger.Debugw("Failed to extract directory", zap.Error(err))
				return "", err
			}
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				s.logger.Debugw("Failed to extract directory", zap.Error(err))
				return "", err
			}

			outFile, err := os.Create(path)
			if err != nil {
				s.logger.Debugw("Failed to extract file", zap.Error(err))
				return "", err
			}
			defer outFile.Close()

			if _, err = io.Copy(outFile, tarReader); err != nil {
				s.logger.Debugw("Failed to copy file during extraction", zap.Error(err))
				return "", err
			}
		}
	}

	return outputPath, nil
}

type RequestLogger struct {
	logger *zap.SugaredLogger
}

func (r *RequestLogger) LogRequest(host string, status int, latency time.Duration, ip, method, path string) {
	r.logger.Infow("Request", zap.String("host", host), zap.Int("status", status), zap.Duration("latency", latency), zap.String("ip", ip), zap.String("method", method), zap.String("path", path))
}
