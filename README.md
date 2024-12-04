# Flux

Flux is a lightweight self-hosted pseudo-paas for golang web apps that emphasizes simplicity and speed. Flux is built on top of [Buildpacks](https://buildpacks.io/) and [Docker](https://docs.docker.com/get-docker/). It is designed on top of blue-green deployments designed with the "set it and forget it" principle in mind.

(I'll make this better later school starts in 30 minutes LMAO)

## Usage

To get started you'll want [ZQDGR](https://github.com/juls0730/zqdgr), and you can start the daemon either with:
```
zqdgr build:daemon
sudo ./fluxd
```
or with 
```
FLUXD_ROOT_DIR=$PWD/fluxdd zqdgr run:daemon
```

To get started with the cli you can run either
```
zqdgr build:cli
./flux list
```
or
```
zqdgr run:cli -- list
```

TODO: `go install` instructions and a docker image (sowwy)

### Daemon

The daemon is a HTTP server that listens for incoming HTTP requests. It handles deploying new apps and managing their containers.

To run the daemon, simply run `fluxd` in the root directory of this repository. The daemon will listen on port 5647, and the reverse proxy will listen on port 7465, but is configurable with the environment variable `FLUXD_PROXY_PORT`. Once you deploy an app, you must point the domain to the reverse proxy (make sure the Host header is sent).

#### Configuration

The daemon will look for a `config.json` in ~/.config/flux, all this file contains is the builder to use for building the app's image, by default this is `paketobuildpacks/builder-jammy-tiny`.

### CLI

The CLI is a command-line interface for interacting with the daemon.

```
flux <command>
```

The following commands are available:

- `init`: Initialize a new project
- `deploy`: Deploy an app
- `start`: Start a deployed app (apps are automatically started when deployed)
- `stop`: Stop a deployed app
- `delete`: Delete a deployed app
- `list`: List all deployed apps

#### Configuration

The CLI will look for a `config.json` in ~/.config/flux, all this file contains is the URL of the daemon, by default this is http://127.0.0.1:5647 but for most real use cases, this will be a server.

#### flux.json

flux.json is the configuration file for a project, it contains the name of the project, the URL it should listen to, and the port it should listen to. You can also specify an env file and environment variables to set. All the available options are shown below:

- `name`: The name of the project
- `url`: The URL the project should listen to
- `port`: The port the web server is listening on
- `env_file`: The path to an env file to load environment variables from (relative to the project directory)
- `environment`: An array of environment variables to set

## Dependencies

- [Go](https://golang.org/dl/)
- [Buildpacks](https://buildpacks.io/) (daemon only)
- [Docker](https://docs.docker.com/get-docker/) (daemon only)

## Contributing
Found a bug, or have something you think would make Flux better? Submit an issue or pull request.

## License
Flux is licensed with the MIT license
