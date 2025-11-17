package types

import (
	"errors"
	"fmt"
)

// ChatCompletionRequest models the subset of the OpenAI Chat Completions API
// required for bridging AnythingLLM with the local mediator.
type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Tools       []Tool        `json:"tools,omitempty"`
	User        string        `json:"user,omitempty"`
}

// ChatMessage mirrors the OpenAI shape; content is treated as text only for now.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// Tool matches the OpenAI tools array shape to preserve compatibility.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function exposed via the OpenAI-like API.
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ChatCompletionResponse is returned to the API layer for JSON serialisation.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is a single assistant response choice.
type Choice struct {
	Index        int              `json:"index"`
	FinishReason string           `json:"finish_reason"`
	Message      AssistantMessage `json:"message"`
}

// AssistantMessage represents the assistant payload in the response.
type AssistantMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Usage mimics OpenAI token accounting so AnythingLLM can render analytics.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Validate performs lightweight sanity checks on incoming requests.
func (r *ChatCompletionRequest) Validate() error {
	if r.Model == "" {
		return errors.New("model is required")
	}
	if len(r.Messages) == 0 {
		return errors.New("at least one message is required")
	}
	for i, msg := range r.Messages {
		if msg.Role == "" {
			return fmt.Errorf("message %d missing role", i)
		}
	}
	return nil
}
