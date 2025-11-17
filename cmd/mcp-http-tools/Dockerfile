# syntax=docker/dockerfile:1

FROM golang:1.22-bullseye AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/mcp-http-tools ./cmd/mcp-http-tools

FROM gcr.io/distroless/base-debian12
WORKDIR /app
COPY --from=builder /out/mcp-http-tools /usr/local/bin/mcp-http-tools

EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/mcp-http-tools"]
