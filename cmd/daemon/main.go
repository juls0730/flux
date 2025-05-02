package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/juls0730/flux/internal/handlers"
)

func main() {
	fluxServer := handlers.NewServer()
	defer fluxServer.Stop()

	http.HandleFunc("POST /deploy", fluxServer.DeployNewApp)

	http.HandleFunc("GET /apps", fluxServer.GetAllApps)
	http.HandleFunc("GET /app/by-name/{name}", fluxServer.GetAppByName)
	http.HandleFunc("GET /app/by-id/{id}", fluxServer.GetAppById)

	http.HandleFunc("PUT /app/{id}/start", fluxServer.StartApp)
	http.HandleFunc("PUT /app/{id}/stop", fluxServer.StopApp)

	http.HandleFunc("DELETE /apps", fluxServer.DeleteAllDeploymentsHandler)
	http.HandleFunc("DELETE /app/{id}", fluxServer.DeleteDeployHandler)

	http.HandleFunc("GET /heartbeat", fluxServer.DaemonInfoHandler)

	err := fluxServer.ListenAndServe()
	if err != nil {
		fmt.Printf("Failed to start server: %v\n", err)
		os.Exit(1)
	}
}
