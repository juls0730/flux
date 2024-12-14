FROM golang:1.23-bookworm as builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN GOOS=linux go build -o fluxd cmd/fluxd/main.go

RUN (curl -sSL "https://github.com/buildpacks/pack/releases/download/v0.36.0/pack-v0.36.0-linux.tgz" | tar -C /usr/local/bin/ --no-same-owner -xzv pack)
RUN apt-get install -y ca-certificates

EXPOSE 5647 7465

VOLUME [ "/var/run/docker.sock" ]

CMD ["/app/fluxd"]
