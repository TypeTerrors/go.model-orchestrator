package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/charmbracelet/log"
	openai "github.com/openai/openai-go"
	oaioption "github.com/openai/openai-go/option"

	"go.mcpwrapper/internal/config"
	"go.mcpwrapper/internal/discovery"
	"go.mcpwrapper/internal/logging"
	"go.mcpwrapper/internal/mcp"
	"go.mcpwrapper/internal/types"
)

func main() {
	logger := logging.FromEnv("[agent-wrapper]")

	cfg, err := config.LoadWrapper()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	logger.Info("configuration loaded",
		"port", cfg.Port,
		"backend_model", cfg.BackendModel,
		"api_model", cfg.APIModel,
		"base_url", cfg.BaseURL,
		"api_key_set", cfg.APIKey != "",
		"advertise", cfg.Advertise,
		"instance", cfg.Instance,
		"role", cfg.Role,
		"description", cfg.Description,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	mcpClient := mcp.NewClient(mcp.Options{})

	disc := discovery.New(discovery.Options{})
	if err := disc.Start(ctx); err != nil {
		logger.Error("failed to start discovery", "error", err)
		os.Exit(1)
	}
	defer disc.Stop()

	var announcer *discovery.Announcer
	if cfg.Advertise {
		text := map[string]string{
			"role":      cfg.Role,
			"model":     cfg.BackendModel,
			"api_model": cfg.APIModel,
		}
		if cfg.Description != "" {
			text["description"] = cfg.Description
		}
		announcer, err = discovery.NewAnnouncer(discovery.AnnounceOptions{
			Instance: cfg.Instance,
			Port:     cfg.Port,
			Text:     text,
		})
		if err != nil {
			logger.Error("failed to announce agent wrapper", "error", err)
			os.Exit(1)
		}
		defer announcer.Stop()
	}

	openaiClient := openai.NewClient(
		oaioption.WithBaseURL(cfg.BaseURL),
		oaioption.WithAPIKey(cfg.APIKey),
	)

	agentServer := newAgentToolServer(logger, &openaiClient, cfg)
	go agentServer.Run(ctx)

	wrapper := NewAgentWrapper(&openaiClient, cfg, disc, logger, mcpClient)
	go wrapper.Run(ctx)

	logger.Info("agent wrapper ready",
		"backend_model", cfg.BackendModel,
		"api_model", cfg.APIModel,
	)

	<-ctx.Done()
	logger.Info("agent wrapper stopped")
}

type AgentWrapper struct {
	cfg        config.Config
	discovery  *discovery.Discovery
	logger     *log.Logger
	toolClient *mcp.Client
}

func NewAgentWrapper(_ *openai.Client, cfg config.Config, disc *discovery.Discovery, logger *log.Logger, toolClient *mcp.Client) *AgentWrapper {
	return &AgentWrapper{
		cfg:        cfg,
		discovery:  disc,
		logger:     logger,
		toolClient: toolClient,
	}
}

func (a *AgentWrapper) Run(ctx context.Context) {
	events := a.discovery.Subscribe(64)
	defer a.discovery.Unsubscribe(events)

	state := make(map[string]*discovery.ServerInfo)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			a.handleEvent(ctx, evt, state)
		case <-ticker.C:
			a.logSummary(state)
		}
	}
}

func (a *AgentWrapper) handleEvent(ctx context.Context, evt discovery.Event, state map[string]*discovery.ServerInfo) {
	info := evt.Server
	if info == nil {
		return
	}

	fields := []any{
		"kind", info.Kind,
		"instance", info.Instance,
		"host", info.Host,
		"address", info.Address,
		"last_seen", info.LastSeen.Format(time.RFC3339),
	}
	if model := info.Text["model"]; model != "" {
		fields = append(fields, "model", model)
	}
	if api := info.Text["api_model"]; api != "" {
		fields = append(fields, "api_model", api)
	}
	if len(info.Text) > 0 {
		fields = append(fields, "meta", info.Text)
	}

	switch info.Kind {
	case discovery.ServerKindOrchestrator:
		a.logOrchestratorEvent(evt.Type, fields, state, info)
	case discovery.ServerKindTool:
		a.logToolEvent(ctx, evt.Type, fields, state, info)
	default:
		a.logGenericEvent(evt.Type, fields, state, info)
	}
}

func (a *AgentWrapper) logOrchestratorEvent(eventType discovery.EventType, fields []any, state map[string]*discovery.ServerInfo, info *discovery.ServerInfo) {
	switch eventType {
	case discovery.EventAdded:
		state[info.Instance] = info
		a.logger.Info("orchestrator discovered", fields...)
	case discovery.EventUpdated:
		state[info.Instance] = info
		a.logger.Info("orchestrator heartbeat", fields...)
	case discovery.EventRemoved:
		delete(state, info.Instance)
		a.logger.Warn("orchestrator lost", fields...)
	default:
		a.logger.Debug("orchestrator event", append([]any{"event", eventType}, fields...)...)
	}
}

func (a *AgentWrapper) logToolEvent(ctx context.Context, eventType discovery.EventType, fields []any, state map[string]*discovery.ServerInfo, info *discovery.ServerInfo) {
	switch eventType {
	case discovery.EventAdded:
		state[info.Instance] = info
		a.logger.Info("tool server discovered", fields...)
		a.inspectTools(ctx, info)
	case discovery.EventUpdated:
		state[info.Instance] = info
		a.logger.Info("tool server heartbeat", fields...)
	case discovery.EventRemoved:
		delete(state, info.Instance)
		a.logger.Warn("tool server lost", fields...)
	default:
		a.logger.Debug("tool server event", append([]any{"event", eventType}, fields...)...)
	}
}

func (a *AgentWrapper) inspectTools(ctx context.Context, info *discovery.ServerInfo) {
	if a.toolClient == nil || info == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tools, err := a.toolClient.ListTools(ctx, info)
	if err != nil {
		a.logger.Warn("failed to list tools", "instance", info.Instance, "error", err)
		return
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	a.logger.Info("tool inventory updated", "instance", info.Instance, "tools", names)

	if hasHTTPGet(tools) {
		scheme := info.Text["scheme"]
		if strings.TrimSpace(scheme) == "" {
			scheme = "http"
		}
		target := fmt.Sprintf("%s://%s/healthz", scheme, info.Address)
		ctxProbe, cancelProbe := context.WithTimeout(ctx, 5*time.Second)
		defer cancelProbe()
		result, err := a.toolClient.CallTool(ctxProbe, info, "http_get", map[string]any{"url": target})
		if err != nil {
			a.logger.Warn("tool health probe failed", "instance", info.Instance, "error", err)
			return
		}
		a.logger.Info("tool health probe succeeded",
			"instance", info.Instance,
			"url", target,
			"status", result.Result["status"],
			"status_text", result.Result["status_text"],
		)
	}
}

func hasHTTPGet(tools []mcp.ToolDefinition) bool {
	for _, t := range tools {
		if strings.EqualFold(t.Name, "http_get") {
			return true
		}
	}
	return false
}

type agentToolServer struct {
	logger      *log.Logger
	client      *openai.Client
	cfg         config.Config
	toolName    string
	description string
	parameters  map[string]any
}

func newAgentToolServer(logger *log.Logger, client *openai.Client, cfg config.Config) *agentToolServer {
	name := sanitizeToolName(cfg.Instance)
	description := strings.TrimSpace(cfg.Description)

	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "Primary user prompt to send to the agent.",
			},
			"messages": map[string]any{
				"type":        "array",
				"description": "Optional chat history as an array of {role, content}.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"role": map[string]any{
							"type":        "string",
							"description": "Role of the message (system, user, assistant).",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "Message text.",
						},
					},
					"required": []string{"role", "content"},
				},
			},
		},
		"required": []string{"prompt"},
	}

	return &agentToolServer{
		logger:      logger,
		client:      client,
		cfg:         cfg,
		toolName:    name,
		description: description,
		parameters:  parameters,
	}
}

func (s *agentToolServer) Run(ctx context.Context) {
	if s.cfg.Port <= 0 {
		s.logger.Warn("agent tool server disabled: no port configured")
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /tools/list", s.handleListTools)
	mux.HandleFunc("POST /tools/call", s.handleCallTool)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.cfg.Port),
		Handler: toolLoggingMiddleware(s.logger, mux),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("agent tool server shutdown error", "error", err)
		}
	}()

	s.logger.Info("agent tool server listening",
		"addr", server.Addr,
		"tool", s.toolName,
		"description", s.description,
	)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.logger.Error("agent tool server error", "error", err)
	}
}

func (s *agentToolServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
}

func (s *agentToolServer) handleListTools(w http.ResponseWriter, r *http.Request) {
	payload := struct {
		Tools []mcp.ToolDefinition `json:"tools"`
	}{
		Tools: []mcp.ToolDefinition{
			{
				Name:        s.toolName,
				Description: s.description,
				Parameters:  s.parameters,
			},
		},
	}
	writeJSON(w, payload, http.StatusOK)
}

func (s *agentToolServer) handleCallTool(w http.ResponseWriter, r *http.Request) {
	var req agentToolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" && len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("prompt or messages are required"))
		return
	}

	params := openai.ChatCompletionNewParams{
		Model:    s.cfg.BackendModel,
		Messages: buildAgentToolMessages(s.description, req.Messages, req.Prompt),
	}

	resp, err := s.client.Chat.Completions.New(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if resp == nil || len(resp.Choices) == 0 {
		writeError(w, http.StatusInternalServerError, errors.New("empty response from provider"))
		return
	}
	choice := resp.Choices[0]

	result := map[string]any{
		"content":            choice.Message.Content,
		"model":              s.cfg.BackendModel,
		"prompt_tokens":      resp.Usage.PromptTokens,
		"completion_tokens":  resp.Usage.CompletionTokens,
		"total_tokens":       resp.Usage.TotalTokens,
		"messages_submitted": len(req.Messages) + 1,
	}

	writeJSON(w, map[string]any{
		"tool":   s.toolName,
		"result": result,
	}, http.StatusOK)

	s.logger.Info("agent tool invocation complete",
		"tool", s.toolName,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
	)
}

type agentToolCallRequest struct {
	Prompt   string              `json:"prompt"`
	Messages []types.ChatMessage `json:"messages"`
}

func sanitizeToolName(instance string) string {
	s := strings.ToLower(strings.TrimSpace(instance))
	if s == "" {
		return "agent_wrapper"
	}
	replacer := strings.NewReplacer(" ", "_", "-", "_", ".", "_")
	s = replacer.Replace(s)
	return fmt.Sprintf("agent_%s", s)
}

func toolLoggingMiddleware(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Debug("agent tool request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func buildAgentToolMessages(description string, history []types.ChatMessage, prompt string) []openai.ChatCompletionMessageParamUnion {
	result := make([]openai.ChatCompletionMessageParamUnion, 0, len(history)+2)
	if strings.TrimSpace(description) != "" {
		result = append(result, openai.SystemMessage(description))
	}
	result = append(result, convertChatMessages(history)...)
	if strings.TrimSpace(prompt) != "" {
		result = append(result, openai.UserMessage(prompt))
	}
	return result
}

func convertChatMessages(msgs []types.ChatMessage) []openai.ChatCompletionMessageParamUnion {
	res := make([]openai.ChatCompletionMessageParamUnion, 0, len(msgs))
	for _, msg := range msgs {
		switch strings.ToLower(msg.Role) {
		case "system":
			res = append(res, openai.SystemMessage(msg.Content))
		case "assistant":
			res = append(res, openai.ChatCompletionMessageParamOfAssistant(msg.Content))
		case "user":
			res = append(res, openai.UserMessage(msg.Content))
		default:
			res = append(res, openai.UserMessage(msg.Content))
		}
	}
	return res
}

func buildToolDescription(userDescription, modelName string) string {
	parts := []string{}
	if userDescription != "" {
		parts = append(parts, userDescription)
	}
	parts = append(parts,
		fmt.Sprintf("Invokes the dedicated agent wrapper backed by model %s.", modelName),
		"Use to delegate complex multi-step reasoning or conversations that should run on this specialised agent.",
		"Accepts `prompt` (string) and optional `messages` (chat history) mirroring OpenAI Chat payloads.",
	)
	return strings.Join(parts, " ")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
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

func (a *AgentWrapper) logGenericEvent(eventType discovery.EventType, fields []any, state map[string]*discovery.ServerInfo, info *discovery.ServerInfo) {
	switch eventType {
	case discovery.EventAdded:
		state[info.Instance] = info
		a.logger.Info("service discovered", fields...)
	case discovery.EventUpdated:
		state[info.Instance] = info
		a.logger.Info("service heartbeat", fields...)
	case discovery.EventRemoved:
		delete(state, info.Instance)
		a.logger.Warn("service lost", fields...)
	default:
		a.logger.Debug("service event", append([]any{"event", eventType}, fields...)...)
	}
}

func (a *AgentWrapper) logSummary(state map[string]*discovery.ServerInfo) {
	if len(state) == 0 {
		a.logger.Info("discovery heartbeat", "services", 0)
		return
	}

	orchestrators := 0
	tools := 0
	for _, info := range state {
		switch info.Kind {
		case discovery.ServerKindOrchestrator:
			orchestrators++
		case discovery.ServerKindTool:
			tools++
		}
	}

	a.logger.Info("discovery heartbeat",
		"services", len(state),
		"orchestrators", orchestrators,
		"tools", tools,
	)
}
