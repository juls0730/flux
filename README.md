# Flux

Flux is a lightweight self-hosted micro-PaaS for hosting Golang web apps with ease. Built on top of [Buildpacks](https://buildpacks.io/) and [Docker](https://docs.docker.com/get-docker/), Flux simplifies the deployment process with a focus on similicity, speed, and reliability.

**Goals**:

- Automatic deployment of Golang web apps, simply run `flux init`, and run `flux deploy` to deploy your app!
- Zero-downtime deployments with blue-green deployments
- Simple but powerful configuration, flux should be able to handle most use cases, from a micro web app to a fullstack app with databases, caching layers, full text search, etc.

**What is flux not?**

- Flux is not meant to be used as a multi-tenant PaaS, it is meant to be used by trusted individuals, while flux will still have security in mind, certain things are not secure. For example, anyone can delete all your apps, so be careful, anyone who has access to your flux server can do a lot of damage.

**Limitations**:
- Theoretically flux is likely limited by the amount of containers can fit in the bridge network, but I haven't tested this
- Containers are not particularly isolated, if one malicious container wanted to scan all containers, or interact with other containers it tectically shouldnt, it totally just can (todo?)

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

cat <<EOF
[Unit]
Description=Flux Daemon
After=network.target

[Service]
ExecStart=/usr/local/bin/fluxd
Restart=always
Environment=GOPATH=/var/fluxd/go

[Install]
WantedBy=multi-user.target
EOF | sudo tee /etc/systemd/system/fluxd.service

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
  "builder": "paketobuildpacks/builder-jammy-tiny",
  "disable_delete_all": false,
  "compression": {
    "enabled": false
  }
}
```

- `builder`: The buildpack builder to use (default: `paketobuildpacks/builder-jammy-tiny`)
- `disable_delete_all`: Disable the delete all deployments endpoint (default: `false`)
- `compression`: Compression settings
  - `enabled`: Enable compression (default: `false`)
  - `level`: Compression level

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
  "containers": [
    {
        "name": "redis",
        "image": "redis:latest",
        "volumes": [
            {
                "mountpoint": "/data"
            }
        ],
    }
  ],
  "env_file": ".env",
  "environment": ["DEBUG=true"]
}
```

The project config files has the following options:

| field | description | required |
| ----- | ----------- | -------- |
| `name` | The name of the project | true |
| `url` | Domain for the application | true |
| `port` | Web server's listening port | true |
| `env_file` | Path to environment variable file | false |
| `environment` | Additional environment variables | false |
| `containers` | Supplemental containers to run alongside the app | false |
| `volumes` | Volumes to mount to the app's containers | false |

## Deployment Notes

- After deploying an app, point your domain to the Flux reverse proxy
- Ensure the Host header is sent with your requests

## Contributing

Found a bug, or have something you think would make Flux better? Submit an issue or pull request.

## License

Flux is licensed with the MIT license
