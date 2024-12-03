# Flux

Flux is a lightweight self-hosted pseudo-paas for golang web apps. Inside this repository you will find the daemon and cli binaries.

## Usage

### Daemon

The daemon is a HTTP server that listens for incoming HTTP requests. It handles deploying new apps and managing their containers.

### CLI

The CLI is a command-line interface for interacting with the daemon.

## Dependencies

- [Go](https://golang.org/dl/)
- [Buildpacks](https://buildpacks.io/) (daemon only)
- [Docker](https://docs.docker.com/get-docker/) (daemon only)
