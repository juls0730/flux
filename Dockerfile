FROM golang:1.23-bookworm AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build -o fluxd ./cmd/fluxd/main.go

FROM golang:1.23-bookworm

RUN (curl -sSL "https://github.com/buildpacks/pack/releases/download/v0.36.0/pack-v0.36.0-linux.tgz" | tar -C /usr/local/bin/ --no-same-owner -xzv pack)

COPY --from=builder /app/fluxd /usr/local/bin/fluxd

ENV PATH="/usr/local/bin:${PATH}"

VOLUME ["/var/run/docker.sock"]

EXPOSE 5647 7465

ENTRYPOINT ["fluxd"]