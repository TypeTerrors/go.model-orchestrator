package mediator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"
	"github.com/openai/openai-go/shared/constant"

	"go.mcpwrapper/internal/discovery"
	"go.mcpwrapper/internal/mcp"
	"go.mcpwrapper/internal/types"
)

// ErrModelUnsupported indicates that the requested model is not handled by this mediator.
var ErrModelUnsupported = errors.New("model not supported")

// ErrStreamingUnsupported is returned when the client requests streaming responses.
var ErrStreamingUnsupported = errors.New("streaming is not supported")

// Options configure the mediator during construction.
type Options struct {
	ModelName     string
	ProviderModel string
	AllowedKinds  []string
	ToolClient    *mcp.Client
	OpenAIClient  *openai.Client
}

// ToolDescriptor exposes a discovered tool in an OpenAI-style format for diagnostics.
type ToolDescriptor struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Server      ToolServerRef  `json:"server"`
	Original    string         `json:"original_tool"`
}

// ToolServerRef provides contextual information about the MCP server hosting a tool.
type ToolServerRef struct {
	Instance string            `json:"instance"`
	Address  string            `json:"address"`
	Kind     string            `json:"kind"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type toolMeta struct {
	Server       *discovery.ServerInfo
	ToolName     string
	Description  string
	OriginalName string
}

// Mediator routes chat requests, consults discovery, and orchestrates MCP tool usage.
type Mediator struct {
	discovery     *discovery.Discovery
	openaiClient  *openai.Client
	providerModel string
	modelName     string
	allowedKinds  map[string]struct{}
	toolClient    *mcp.Client
}

// New returns a configured mediator instance.
func New(discovery *discovery.Discovery, opts Options) *Mediator {
	if opts.ModelName == "" {
		opts.ModelName = "go-agent-1"
	}
	kindSet := make(map[string]struct{}, len(opts.AllowedKinds))
	for _, k := range opts.AllowedKinds {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" {
			kindSet[k] = struct{}{}
		}
	}
	if len(kindSet) == 0 {
		kindSet = nil
	}
	client := opts.ToolClient
	if client == nil {
		client = mcp.NewClient(mcp.Options{})
	}
	return &Mediator{
		discovery:     discovery,
		openaiClient:  opts.OpenAIClient,
		providerModel: opts.ProviderModel,
		modelName:     opts.ModelName,
		allowedKinds:  kindSet,
		toolClient:    client,
	}
}

// SupportedModels exposes the list of models understood by this mediator.
func (m *Mediator) SupportedModels() []string {
	return []string{m.modelName}
}

// HandleChat is the main entry point used by the API layer.
func (m *Mediator) HandleChat(ctx context.Context, req types.ChatCompletionRequest) (types.ChatCompletionResponse, error) {
	if err := req.Validate(); err != nil {
		return types.ChatCompletionResponse{}, err
	}
	if req.Stream {
		return types.ChatCompletionResponse{}, ErrStreamingUnsupported
	}
	if req.Model != "" && !m.supportsModel(req.Model) {
		return types.ChatCompletionResponse{}, fmt.Errorf("%w: %s", ErrModelUnsupported, req.Model)
	}
	if m.openaiClient == nil {
		return types.ChatCompletionResponse{}, errors.New("openai client not configured")
	}

	messages := convertMessages(req.Messages)
	toolParams, meta, _, err := m.collectTools(ctx)
	if err != nil {
		// proceed with whatever we have; log via returned error context appended.
		messages = append(messages, openai.SystemMessage(fmt.Sprintf("Warning: tool discovery error: %v", err)))
	}

	conversation := append([]openai.ChatCompletionMessageParamUnion{}, messages...)

	for {
		params := openai.ChatCompletionNewParams{
			Model:    m.providerModelOrDefault(),
			Messages: conversation,
		}
		if len(toolParams) > 0 {
			params.Tools = toolParams
		}

		resp, err := m.openaiClient.Chat.Completions.New(ctx, params)
		if err != nil {
			return types.ChatCompletionResponse{}, err
		}
		if resp == nil || len(resp.Choices) == 0 {
			return types.ChatCompletionResponse{}, errors.New("empty completion response")
		}

		choice := resp.Choices[0]
		conversation = append(conversation, choice.Message.ToParam())

		if len(choice.Message.ToolCalls) == 0 {
			return buildOpenAIResponse(m.modelName, resp), nil
		}

		for _, call := range choice.Message.ToolCalls {
			metaEntry, ok := meta[call.Function.Name]
			if !ok {
				return types.ChatCompletionResponse{}, fmt.Errorf("unknown tool '%s'", call.Function.Name)
			}
			var args map[string]any
			if call.Function.Arguments != "" {
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					return types.ChatCompletionResponse{}, fmt.Errorf("invalid tool arguments for %s: %w", call.Function.Name, err)
				}
			}
			result, err := m.toolClient.CallTool(ctx, metaEntry.Server, metaEntry.ToolName, args)
			if err != nil {
				return types.ChatCompletionResponse{}, fmt.Errorf("tool %s failed: %w", call.Function.Name, err)
			}
			payload := map[string]any{
				"tool":        metaEntry.ToolName,
				"server":      metaEntry.Server.Instance,
				"description": metaEntry.Description,
				"result":      result.Result,
			}
			data, _ := json.Marshal(payload)
			conversation = append(conversation, openai.ToolMessage(string(data), call.ID))
		}
	}
}

// ListTools aggregates all tools exposed by discovered MCP servers and returns an OpenAI-style roster.
func (m *Mediator) ListTools(ctx context.Context) ([]ToolDescriptor, error) {
	_, _, descriptors, err := m.collectTools(ctx)
	return descriptors, err
}

func (m *Mediator) collectTools(ctx context.Context) ([]openai.ChatCompletionToolParam, map[string]toolMeta, []ToolDescriptor, error) {
	servers := m.discovery.ServersSnapshot()
	if len(servers) == 0 {
		return nil, map[string]toolMeta{}, nil, nil
	}
	if m.toolClient == nil {
		return nil, nil, nil, errors.New("tool client not configured")
	}

	var (
		toolParams  []openai.ChatCompletionToolParam
		descriptors []ToolDescriptor
	)
	meta := make(map[string]toolMeta)
	var lastErr error

	for _, srv := range servers {
		if len(m.allowedKinds) > 0 {
			if _, ok := m.allowedKinds[strings.ToLower(strings.TrimSpace(srv.Kind))]; !ok {
				continue
			}
		}
		if !isToolHost(srv) {
			continue
		}
		ctxList, cancel := context.WithTimeout(ctx, 10*time.Second)
		tools, err := m.toolClient.ListTools(ctxList, srv)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		for _, tool := range tools {
			functionName := buildFunctionName(srv.Instance, tool.Name, meta)
			description := buildToolDescription(tool.Description, srv)
			fn := shared.FunctionDefinitionParam{
				Name:        functionName,
				Description: openai.String(description),
				Parameters:  tool.Parameters,
			}
			toolParams = append(toolParams, openai.ChatCompletionToolParam{
				Type:     constant.Function("function"),
				Function: fn,
			})
			meta[functionName] = toolMeta{
				Server:       srv,
				ToolName:     tool.Name,
				Description:  description,
				OriginalName: tool.Name,
			}
			descriptors = append(descriptors, ToolDescriptor{
				Name:        functionName,
				Original:    tool.Name,
				Description: description,
				Parameters:  tool.Parameters,
				Server: ToolServerRef{
					Instance: srv.Instance,
					Address:  srv.Address,
					Kind:     srv.Kind,
					Metadata: cloneMetadata(srv.Text),
				},
			})
		}
	}

	sort.Slice(toolParams, func(i, j int) bool {
		return toolParams[i].Function.Name < toolParams[j].Function.Name
	})
	sort.Slice(descriptors, func(i, j int) bool {
		return descriptors[i].Name < descriptors[j].Name
	})

	if len(toolParams) == 0 && lastErr != nil {
		return nil, meta, descriptors, lastErr
	}
	return toolParams, meta, descriptors, lastErr
}

func (m *Mediator) supportsModel(model string) bool {
	return model == m.modelName
}

func (m *Mediator) providerModelOrDefault() string {
	if strings.TrimSpace(m.providerModel) != "" {
		return m.providerModel
	}
	return m.modelName
}

func convertMessages(msgs []types.ChatMessage) []openai.ChatCompletionMessageParamUnion {
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
			// Fallback to user role for unsupported entries.
			res = append(res, openai.UserMessage(msg.Content))
		}
	}
	return res
}

func buildFunctionName(instance, toolName string, existing map[string]toolMeta) string {
	base := fmt.Sprintf("%s__%s", slugify(instance), slugify(toolName))
	name := base
	i := 2
	for {
		if _, exists := existing[name]; !exists {
			return name
		}
		name = fmt.Sprintf("%s__%d", base, i)
		i++
	}
}

func slugify(input string) string {
	s := strings.ToLower(strings.TrimSpace(input))
	if s == "" {
		return "tool"
	}
	replacer := strings.NewReplacer(" ", "_", "-", "_", ".", "_", "/", "_", ":", "_")
	s = replacer.Replace(s)
	return s
}

func buildToolDescription(toolDescription string, srv *discovery.ServerInfo) string {
	parts := []string{}
	if trimmed := strings.TrimSpace(toolDescription); trimmed != "" {
		parts = append(parts, trimmed)
	}
	if srv != nil {
		serverInfo := fmt.Sprintf("Provided by %s (%s)", srv.Instance, srv.Kind)
		if desc, ok := srv.Text["description"]; ok && strings.TrimSpace(desc) != "" {
			serverInfo = fmt.Sprintf("%s - %s", serverInfo, desc)
		}
		parts = append(parts, serverInfo)
	}
	if len(parts) == 0 {
		return "No description provided."
	}
	return strings.Join(parts, " | ")
}

func isToolHost(info *discovery.ServerInfo) bool {
	if info == nil {
		return false
	}
	kind := strings.ToLower(strings.TrimSpace(info.Kind))
	return kind == strings.ToLower(discovery.ServerKindTool) || kind == strings.ToLower(discovery.ServerKindAgentWrapper)
}

func cloneMetadata(meta map[string]string) map[string]string {
	if meta == nil {
		return nil
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		out[k] = v
	}
	return out
}

func buildOpenAIResponse(model string, resp *openai.ChatCompletion) types.ChatCompletionResponse {
	choice := resp.Choices[0]
	content := choice.Message.Content
	usage := resp.Usage

	return types.ChatCompletionResponse{
		ID:      resp.ID,
		Object:  string(resp.Object),
		Created: resp.Created,
		Model:   model,
		Choices: []types.Choice{
			{
				Index:        int(choice.Index),
				FinishReason: choice.FinishReason,
				Message: types.AssistantMessage{
					Role:    "assistant",
					Content: content,
				},
			},
		},
		Usage: types.Usage{
			PromptTokens:     int(usage.PromptTokens),
			CompletionTokens: int(usage.CompletionTokens),
			TotalTokens:      int(usage.TotalTokens),
		},
	}
}
