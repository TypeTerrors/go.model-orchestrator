package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.mcpwrapper/internal/discovery"
)

// ToolDefinition mirrors the MCP tool metadata returned by servers.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// CallResult is the generic return payload from /tools/call responses.
type CallResult struct {
	Tool   string         `json:"tool"`
	Result map[string]any `json:"result"`
}

// Client provides a minimal MCP HTTP client.
type Client struct {
	httpClient *http.Client
}

// Options control client behaviour.
type Options struct {
	Timeout time.Duration
}

// NewClient constructs a client with sane defaults.
func NewClient(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// ListTools queries the MCP server for available tools.
func (c *Client) ListTools(ctx context.Context, server *discovery.ServerInfo) ([]ToolDefinition, error) {
	if server == nil {
		return nil, fmt.Errorf("nil server")
	}
	endpoint, err := buildURL(server, "/tools/list")
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("list tools failed: %s (%s)", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Tools []ToolDefinition `json:"tools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode tools: %w", err)
	}
	return payload.Tools, nil
}

// CallTool invokes a specific tool with arguments.
func (c *Client) CallTool(ctx context.Context, server *discovery.ServerInfo, tool string, arguments map[string]any) (CallResult, error) {
	var result CallResult
	if server == nil {
		return result, fmt.Errorf("nil server")
	}
	if strings.TrimSpace(tool) == "" {
		return result, fmt.Errorf("tool name is required")
	}
	endpoint, err := buildURL(server, "/tools/call")
	if err != nil {
		return result, err
	}

	reqPayload := map[string]any{
		"name":      tool,
		"arguments": arguments,
	}
	buf, err := json.Marshal(reqPayload)
	if err != nil {
		return result, fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return result, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return result, fmt.Errorf("call tool: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return result, fmt.Errorf("call tool failed: %s (%s)", resp.Status, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return result, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

func buildURL(server *discovery.ServerInfo, path string) (string, error) {
	if server == nil {
		return "", fmt.Errorf("nil server")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := server.Text["url"]
	if strings.TrimSpace(target) != "" {
		base, err := url.Parse(target)
		if err != nil {
			return "", fmt.Errorf("invalid server url %q: %w", target, err)
		}
		base.Path = path
		return base.String(), nil
	}

	host := server.Host
	if host == "" {
		host = server.Instance
	}
	address := server.Address
	if address == "" && host != "" {
		address = net.JoinHostPort(host, fmt.Sprint(server.Port))
	}
	if strings.TrimSpace(address) == "" {
		return "", fmt.Errorf("server %s missing address", server.Instance)
	}
	scheme := strings.TrimSpace(server.Text["scheme"])
	if scheme == "" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s%s", scheme, address, path), nil
}
