package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"go.mcpwrapper/internal/mediator"
	"go.mcpwrapper/internal/types"
)

// Server is the HTTP entry point that mimics the OpenAI chat completions API.
type Server struct {
	med *mediator.Mediator
	mux *http.ServeMux
}

// NewServer sets up the routing layer.
func NewServer(med *mediator.Mediator) *Server {
	s := &Server{
		med: med,
		mux: http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/models", s.handleModels)
	s.mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	s.mux.HandleFunc("GET /v1/tools", s.handleTools)
}

// Handler exposes the mux for integration with http.Server.
func (s *Server) Handler() http.Handler {
	return s
}

// ServeHTTP delegates to the mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.med.SupportedModels()

	resp := modelsResponse{
		Object: "list",
		Data:   make([]modelDescriptor, 0, len(models)),
	}
	for _, m := range models {
		resp.Data = append(resp.Data, modelDescriptor{
			ID:      m,
			Object:  "model",
			OwnedBy: "go-agent",
		})
	}
	writeJSON(w, resp, http.StatusOK)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req types.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	resp, err := s.med.HandleChat(r.Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, mediator.ErrModelUnsupported):
			writeError(w, http.StatusNotFound, err)
		case errors.Is(err, mediator.ErrStreamingUnsupported):
			writeError(w, http.StatusBadRequest, err)
		default:
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}

	writeJSON(w, resp, http.StatusOK)
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tools, err := s.med.ListTools(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	resp := toolsResponse{
		Object: "list",
		Data:   tools,
	}
	writeJSON(w, resp, http.StatusOK)
}

type modelsResponse struct {
	Object string            `json:"object"`
	Data   []modelDescriptor `json:"data"`
}

type modelDescriptor struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

func writeJSON(w http.ResponseWriter, payload any, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type openAIError struct {
	Error openAIErrorDetails `json:"error"`
}

type openAIErrorDetails struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type toolsResponse struct {
	Object string                         `json:"object"`
	Data   []mediator.ToolDescriptor `json:"data"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := openAIError{
		Error: openAIErrorDetails{
			Message: err.Error(),
			Type:    http.StatusText(status),
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}
