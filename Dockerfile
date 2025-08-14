FROM golang:1.21-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/eip-rotator ./cmd/eip-rotator

FROM ubuntu:22.04
SHELL ["/bin/bash", "-lc"]
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /out/eip-rotator /usr/local/bin/eip-rotator
COPY configs/tasks.example.json /app/tasks.json

# default: schedule mode with config
ENV CONFIG_PATH=/app/tasks.json
ENTRYPOINT ["/usr/local/bin/eip-rotator", "--mode", "schedule", "--config", "/app/tasks.json"]
