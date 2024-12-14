# Flux

Flux is a lightweight self-hosted pseudo-PaaS for hosting Golang web apps with ease. Built on top of [Buildpacks](https://buildpacks.io/) and [Docker](https://docs.docker.com/get-docker/), Flux simplifies the deployment process with a focus on similicity, speed, and reliability.

## Features

- **Blue-Green Deployments**: Deploy new versions of your app without downtime
- **Simplify Deployment**: Flux takes care of the deployment process, so you can focus on writing your app
- **Flexible Configuration**: Easily configure your app with `flux.json`
- **Automatic Container Management**: Steamline your app with automatic container management

## Dependencies

- [Go](https://golang.org/dl/)
- [ZQDGR](https://github.com/juls0730/zqdgr)
- [Buildpacks](https://buildpacks.io/) (daemon only)
- [Docker](https://docs.docker.com/get-docker/) (daemon only)

## Intallation

### Daemon

To install and start the Flux daemon using ZQDGR, run the following command:

> [!IMPORTANT]
> CGO is required to build the daemon due to the use of [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)

#### Method 1: ZQDGR

```bash
go install github.com/juls0730/zqdgr@latest

git clone https://github.com/juls0730/flux.git
cd flux

# either
zqdgr build:daemon
sudo ./fluxd

# or
FLUXD_ROOT_DIR=$PWD/fluxdd zqdgr run:daemon
```

#### Method 2: Docker

```bash
docker run -d --name fluxd --network host -v /var/run/docker.sock:/var/run/docker.sock -v fluxd-data:/var/fluxd -p 5647:5647 -p 7465:7465 zoeissleeping/fluxd:latest
```

#### Method 3: Systemd

```bash
go install github.com/juls0730/zqdgr@latest

git clone https://github.com/juls0730/flux.git
cd flux

zqdgr build:daemon
sudo mv fluxd /usr/local/bin/

sudo cat <<EOF > /etc/systemd/system/fluxd.service
[Unit]
Description=Flux Daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/fluxd
WorkingDirectory=/var/fluxd
User=fluxuser
Group=fluxgroup
Restart=always
Environment=FLUXD_ROOT_DIR=/var/fluxd

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now fluxd
```

### CLI

Install the CLI using the following command:

```bash
go install github.com/juls0730/flux/cmd/flux@latest
```

## Configuration

### Daemon

Flux daemon looks for a confgiuration file in `/var/fluxd/config.json` but can be configured by setting `$FLUXD_ROOT_DIR` to the directory where you want all fluxd files to be stored.

```json
{
  "builder": "paketobuildpacks/builder-jammy-tiny"
}
```

- `builder`: The buildpack builder to use (default: `paketobuildpacks/builder-jammy-tiny`)

#### Daemon Settings

- **Default port**: 5647 (Daemon server)
- **Reverse Proxy Port**: 7465 (configurable via `FLUXD_PROXY_PORT` environment variable)

### CLI

The CLI looks for a configuration file in `~/.config/flux/config.json`:

```json
{
  "daemon_url": "http://127.0.0.1:5647"
}
```

- `daemon_url`: The URL of the daemon to connect to (default: `http://127.0.0.1:5647`)

### Commands

```bash
Flux <command>
```

Available commands:

- `init`: Initialize a new project
- `deploy`: Deploy an application
- `start`: Start an application
- `stop`: Stop an application
- `delete`: Delete an application
- `list`: View application logs

### Project Configuration (`flux.json`)

flux.json is the configuration file in the root of your proejct that defines deployment settings:

```json
{
  "name": "my-app",
  "url": "myapp.example.com",
  "port": 8080,
  "env_file": ".env",
  "environment": ["DEBUG=true"]
}
```

#### Configuration Options

- `name`: The name of the project
- `url`: Domain for the application
- `port`: Web server's listening port
- `env_file`: Path to environment variable file
- `environment`: Additional environment variables

## Deployment Notes

- After deploying an app, point your domain to the Flux reverse proxy
- Ensure the Host header is sent with your requests

## Contributing

Found a bug, or have something you think would make Flux better? Submit an issue or pull request.

## License

Flux is licensed with the MIT license
