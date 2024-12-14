package main

import (
	"net/http"
	_ "net/http/pprof"

	"github.com/juls0730/flux/server"
	"go.uber.org/zap"
)

func main() {
	fluxServer := server.NewServer()
	defer fluxServer.Stop()

	http.HandleFunc("POST /deploy", fluxServer.DeployHandler)
	http.HandleFunc("DELETE /deployments", fluxServer.DeleteAllDeploymentsHandler)
	http.HandleFunc("DELETE /deployments/{name}", fluxServer.DeleteDeployHandler)
	http.HandleFunc("POST /start/{name}", fluxServer.StartDeployHandler)
	http.HandleFunc("POST /stop/{name}", fluxServer.StopDeployHandler)
	http.HandleFunc("GET /apps", fluxServer.ListAppsHandler)
	http.HandleFunc("GET /heartbeat", fluxServer.DaemonInfoHandler)

	fluxServer.Logger.Info("Fluxd started on http://127.0.0.1:5647")
	err := http.ListenAndServe(":5647", nil)
	if err != nil {
		fluxServer.Logger.Fatalf("Failed to start server: %v", zap.Error(err))
	}
}
