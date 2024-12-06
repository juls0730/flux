package main

import (
	"log"
	"net/http"

	"github.com/juls0730/fluxd/server"
)

func main() {
	fluxServer := server.NewServer()
	server.InitReverseProxy()

	// go fluxServer.Proxy.Start()

	http.HandleFunc("POST /deploy", fluxServer.DeployHandler)
	http.HandleFunc("DELETE /deployments", fluxServer.DeleteAllDeploymentsHandler)
	http.HandleFunc("DELETE /deployments/{name}", fluxServer.DeleteDeployHandler)
	http.HandleFunc("POST /start/{name}", fluxServer.StartDeployHandler)
	http.HandleFunc("POST /stop/{name}", fluxServer.StopDeployHandler)
	http.HandleFunc("GET /apps", fluxServer.ListAppsHandler)

	log.Printf("Fluxd started on http://127.0.0.1:5647\n")
	log.Fatal(http.ListenAndServe(":5647", nil))
}
