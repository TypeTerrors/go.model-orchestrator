package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/charmbracelet/log"
	openai "github.com/openai/openai-go"
	oaioption "github.com/openai/openai-go/option"

	"go.mcpwrapper/internal/api"
	"go.mcpwrapper/internal/config"
	"go.mcpwrapper/internal/discovery"
	"go.mcpwrapper/internal/logging"
	"go.mcpwrapper/internal/mcp"
	"go.mcpwrapper/internal/mediator"
)

func main() {
	logger := logging.FromEnv("[orchestrator]")

	cfg, err := config.LoadOrchestrator()
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

	openaiClient := openai.NewClient(
		oaioption.WithBaseURL(cfg.BaseURL),
		oaioption.WithAPIKey(cfg.APIKey),
	)

	disc := discovery.New(discovery.Options{})
	if err := disc.Start(ctx); err != nil {
		logger.Error("failed to start discovery", "error", err)
		os.Exit(1)
	}
	defer disc.Stop()

	eventsCh := disc.Subscribe(64)
	defer disc.Unsubscribe(eventsCh)
	go monitorDiscovery(ctx, logger, eventsCh, mcpClient)

	med := mediator.New(disc, mediator.Options{
		ModelName:     cfg.APIModel,
		ProviderModel: cfg.BackendModel,
		OpenAIClient:  &openaiClient,
		AllowedKinds:  []string{discovery.ServerKindTool, discovery.ServerKindAgentWrapper},
		ToolClient:    mcpClient,
	})

	handler := api.NewServer(med)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: handler.Handler(),
	}

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
			logger.Error("failed to announce orchestrator", "error", err)
			os.Exit(1)
		}
		defer announcer.Stop()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("http shutdown error", "error", err)
		}
	}()

	logger.Info("API server starting",
		"addr", server.Addr,
		"api_model", cfg.APIModel,
		"backend_model", cfg.BackendModel,
		"base_url", cfg.BaseURL,
		"advertise", cfg.Advertise,
		"role", cfg.Role,
	)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	logger.Info("API server stopped")
}

func monitorDiscovery(ctx context.Context, logger *log.Logger, ch <-chan discovery.Event, toolClient *mcp.Client) {
	state := make(map[string]*discovery.ServerInfo)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			handleEvent(ctx, logger, state, evt, toolClient)
		case <-ticker.C:
			logSummary(logger, state)
		}
	}
}

func handleEvent(ctx context.Context, logger *log.Logger, state map[string]*discovery.ServerInfo, evt discovery.Event, toolClient *mcp.Client) {
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
	case discovery.ServerKindAgentWrapper:
		handleAgentWrapperEvent(logger, state, evt.Type, info, fields)
	case discovery.ServerKindTool:
		handleToolEvent(ctx, logger, state, evt.Type, info, fields, toolClient)
	default:
		handleGenericEvent(logger, state, evt.Type, info, fields)
	}
}

func handleAgentWrapperEvent(logger *log.Logger, state map[string]*discovery.ServerInfo, eventType discovery.EventType, info *discovery.ServerInfo, fields []any) {
	switch eventType {
	case discovery.EventAdded:
		state[info.Instance] = info
		logger.Info("agent wrapper discovered", fields...)
	case discovery.EventUpdated:
		state[info.Instance] = info
		logger.Info("agent wrapper heartbeat", fields...)
	case discovery.EventRemoved:
		delete(state, info.Instance)
		logger.Warn("agent wrapper lost", fields...)
	default:
		logger.Debug("agent wrapper event", append([]any{"event", eventType}, fields...)...)
	}
}

func handleToolEvent(ctx context.Context, logger *log.Logger, state map[string]*discovery.ServerInfo, eventType discovery.EventType, info *discovery.ServerInfo, fields []any, client *mcp.Client) {
	switch eventType {
	case discovery.EventAdded:
		state[info.Instance] = info
		logger.Info("tool server discovered", fields...)
		logToolDefinitions(ctx, logger, client, info)
	case discovery.EventUpdated:
		state[info.Instance] = info
		logger.Info("tool server heartbeat", fields...)
	case discovery.EventRemoved:
		delete(state, info.Instance)
		logger.Warn("tool server lost", fields...)
	default:
		logger.Debug("tool server event", append([]any{"event", eventType}, fields...)...)
	}
}

func logToolDefinitions(ctx context.Context, logger *log.Logger, client *mcp.Client, server *discovery.ServerInfo) {
	if client == nil || server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tools, err := client.ListTools(ctx, server)
	if err != nil {
		logger.Warn("failed to list tools", "instance", server.Instance, "error", err)
		return
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	logger.Info("tool inventory updated", "instance", server.Instance, "tools", names)
}

func handleGenericEvent(logger *log.Logger, state map[string]*discovery.ServerInfo, eventType discovery.EventType, info *discovery.ServerInfo, fields []any) {
	switch eventType {
	case discovery.EventAdded:
		state[info.Instance] = info
		logger.Info("service discovered", fields...)
	case discovery.EventUpdated:
		state[info.Instance] = info
		logger.Info("service heartbeat", fields...)
	case discovery.EventRemoved:
		delete(state, info.Instance)
		logger.Warn("service lost", fields...)
	default:
		logger.Debug("service event", append([]any{"event", eventType}, fields...)...)
	}
}

func logSummary(logger *log.Logger, state map[string]*discovery.ServerInfo) {
	if len(state) == 0 {
		logger.Info("discovery heartbeat",
			"services", 0,
			"agent_wrappers", 0,
			"tool_servers", 0,
			"other", 0,
		)
		return
	}

	agentWrappers := 0
	tools := 0
	other := 0
	for _, info := range state {
		switch info.Kind {
		case discovery.ServerKindAgentWrapper:
			agentWrappers++
		case discovery.ServerKindTool:
			tools++
		default:
			other++
		}
	}

	logger.Info("discovery heartbeat",
		"services", len(state),
		"agent_wrappers", agentWrappers,
		"tool_servers", tools,
		"other", other,
	)
}
