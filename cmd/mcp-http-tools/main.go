package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/charmbracelet/log"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.mcpwrapper/internal/config"
	"go.mcpwrapper/internal/discovery"
	"go.mcpwrapper/internal/logging"
)

func main() {
	logger := logging.FromEnv("[http-tools]")

	cfg, err := config.LoadToolServer()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	logger.Info("configuration loaded",
		"port", cfg.Port,
		"advertise", cfg.Advertise,
		"instance", cfg.Instance,
		"role", cfg.Role,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mux := http.NewServeMux()
	server := newToolServer(logger)
	server.register(mux)
	logger.Info("tools registered", "tools", server.toolNames())

	httpServer := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: loggingMiddleware(logger, mux),
	}

	var announcer *discovery.Announcer
	if cfg.Advertise {
		text := map[string]string{
			"role": cfg.Role,
		}
		announcer, err = discovery.NewAnnouncer(discovery.AnnounceOptions{
			Instance: cfg.Instance,
			Port:     cfg.Port,
			Text:     text,
		})
		if err != nil {
			logger.Error("failed to announce tool server", "error", err)
			os.Exit(1)
		}
		defer announcer.Stop()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown error", "error", err)
		}
	}()

	logger.Info("HTTP tools MCP server starting",
		"addr", httpServer.Addr,
		"advertise", cfg.Advertise,
		"role", cfg.Role,
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	logger.Info("HTTP tools MCP server stopped")
}

type toolServer struct {
	logger     *log.Logger
	httpClient *http.Client
	tools      []toolDefinition
}

func newToolServer(logger *log.Logger) *toolServer {
	return &toolServer{
		logger: logger,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		tools: []toolDefinition{
			makeToolDefinition(http.MethodGet),
			makeToolDefinition(http.MethodPost),
			makeToolDefinition(http.MethodPut),
			makeToolDefinition(http.MethodPatch),
			makeToolDefinition(http.MethodDelete),
		},
	}
}

func makeToolDefinition(method string) toolDefinition {
	description := fmt.Sprintf("Performs an HTTP %s request to a target URL.", strings.ToUpper(method))
	return toolDefinition{
		Name:        fmt.Sprintf("http_%s", strings.ToLower(method)),
		Description: description,
		Method:      method,
		Parameters: toolParameters{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Fully qualified URL to request.",
				},
				"headers": map[string]any{
					"type":        "object",
					"description": "Optional HTTP headers.",
					"additionalProperties": map[string]any{
						"type": "string",
					},
				},
				"body": map[string]any{
					"type":        "string",
					"description": "Optional request body (ignored for GET/DELETE).",
				},
			},
			"required": []string{"url"},
		},
	}
}

func (s *toolServer) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
	})

	mux.HandleFunc("GET /tools/list", s.handleListTools)
	mux.HandleFunc("POST /tools/call", s.handleCallTool)
}

func (s *toolServer) handleListTools(w http.ResponseWriter, r *http.Request) {
	response := struct {
		Tools []toolDefinition `json:"tools"`
	}{
		Tools: s.tools,
	}
	writeJSON(w, response, http.StatusOK)
}

func (s *toolServer) handleCallTool(w http.ResponseWriter, r *http.Request) {
	var req toolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	def, err := s.lookupTool(req.Name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	target, err := extractString(req.Arguments, "url")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	start := time.Now()
	ctx := r.Context()
	res, err := s.executeHTTPRequest(ctx, def.Method, target, req.Arguments)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	s.logger.Info("tool invocation complete",
		"tool", req.Name,
		"method", def.Method,
		"url", target,
		"status", res.Status,
		"duration_ms", time.Since(start).Milliseconds(),
	)

	writeJSON(w, map[string]any{
		"tool":   req.Name,
		"result": res,
	}, http.StatusOK)
}

func (s *toolServer) lookupTool(name string) (toolDefinition, error) {
	for _, tool := range s.tools {
		if tool.Name == name {
			return tool, nil
		}
	}
	return toolDefinition{}, fmt.Errorf("tool %q not found", name)
}

func (s *toolServer) executeHTTPRequest(ctx context.Context, method string, target string, args map[string]any) (httpToolResult, error) {
	if _, err := url.ParseRequestURI(target); err != nil {
		return httpToolResult{}, fmt.Errorf("invalid url: %w", err)
	}

	headers := extractStringMap(args, "headers")
	body := extractOptionalString(args, "body")
	var bodyReader io.Reader
	if body != "" && method != http.MethodGet && method != http.MethodDelete {
		bodyReader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return httpToolResult{}, fmt.Errorf("build request: %w", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return httpToolResult{}, fmt.Errorf("http %s request failed: %w", method, err)
	}
	defer resp.Body.Close()

	const maxBytes = 1 << 20 // 1 MiB
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return httpToolResult{}, fmt.Errorf("read response body: %w", err)
	}

	return httpToolResult{
		Status:     resp.StatusCode,
		StatusText: resp.Status,
		Headers:    resp.Header,
		Body:       string(bodyBytes),
	}, nil
}

type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Method      string         `json:"-"`
	Parameters  toolParameters `json:"parameters"`
}

type toolParameters map[string]any

type toolCallRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type httpToolResult struct {
	Status     int                 `json:"status"`
	StatusText string              `json:"status_text"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
}

func (s *toolServer) toolNames() []string {
	names := make([]string, 0, len(s.tools))
	for _, tool := range s.tools {
		names = append(names, tool.Name)
	}
	return names
}

func extractString(args map[string]any, key string) (string, error) {
	if args == nil {
		return "", fmt.Errorf("missing %s", key)
	}
	value, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing %s", key)
	}
	str, ok := value.(string)
	if !ok || strings.TrimSpace(str) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return str, nil
}

func extractOptionalString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	if value, ok := args[key]; ok {
		if str, ok := value.(string); ok {
			return str
		}
	}
	return ""
}

func extractStringMap(args map[string]any, key string) map[string]string {
	result := make(map[string]string)
	if args == nil {
		return result
	}
	value, ok := args[key]
	if !ok {
		return result
	}
	rawMap, ok := value.(map[string]any)
	if !ok {
		return result
	}
	for k, v := range rawMap {
		if str, ok := v.(string); ok {
			result[k] = str
		}
	}
	return result
}

func writeJSON(w http.ResponseWriter, payload any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"code":    status,
		},
	}, status)
}

func loggingMiddleware(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
