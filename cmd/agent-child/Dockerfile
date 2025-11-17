# syntax=docker/dockerfile:1

FROM golang:1.22-bullseye AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/agent-child ./cmd/agent-child

FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=builder /out/agent-child /usr/local/bin/agent-child

EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/agent-child"]
