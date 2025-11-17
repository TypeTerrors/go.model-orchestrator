# syntax=docker/dockerfile:1

FROM golang:1.22-bullseye AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/agent-orchestrator ./cmd/agent-orchestrator

FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=builder /out/agent-orchestrator /usr/local/bin/agent-orchestrator

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/agent-orchestrator"]
