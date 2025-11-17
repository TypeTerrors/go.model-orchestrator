package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Role definitions used when advertising over mDNS.
const (
	RoleOrchestrator = "orchestrator"
	RoleAgentWrapper = "agent-wrapper"
)

// Config captures runtime configuration derived from flags/env.
type Config struct {
	Port         int
	BackendModel string
	APIModel     string
	BaseURL      string
	APIKey       string
	Advertise    bool
	Instance     string
	Role         string
	Description  string
}

const (
	defaultPort     = 8080
	defaultAPIModel = "go-agent-1"
	defaultBaseURL  = "http://ollama:11434/v1"
	defaultAPIKey   = "ollama"
)

// LoadOrchestrator returns configuration tuned for the parent orchestrator.
func LoadOrchestrator() (Config, error) {
	return load(loadDefaults{
		role:      RoleOrchestrator,
		advertise: false,
	})
}

// LoadWrapper returns configuration tuned for agent-wrapper children.
func LoadWrapper() (Config, error) {
	return load(loadDefaults{
		role:      RoleAgentWrapper,
		advertise: true,
	})
}

type loadDefaults struct {
	role      string
	advertise bool
}

func load(defaults loadDefaults) (Config, error) {
	var cfg Config

	agentModelDefault := strings.TrimSpace(os.Getenv("AGENT_MODEL"))

	defaultAPIModelValue := defaultAPIModel
	if env := strings.TrimSpace(os.Getenv("API_MODEL")); env != "" {
		defaultAPIModelValue = env
	}

	defaultBaseURLValue := defaultBaseURL
	if env := strings.TrimSpace(os.Getenv("BASE_URL")); env != "" {
		defaultBaseURLValue = env
	} else if env := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); env != "" {
		// backwards compatibility
		defaultBaseURLValue = env
	}
	defaultAPIKeyValue := strings.TrimSpace(os.Getenv("API_KEY"))
	if defaultAPIKeyValue == "" {
		defaultAPIKeyValue = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if defaultAPIKeyValue == "" {
		defaultAPIKeyValue = defaultAPIKey
	}

	defaultRole := defaults.role
	if env := strings.TrimSpace(os.Getenv("ROLE")); env != "" {
		defaultRole = env
	}
	if defaultRole == "" {
		defaultRole = RoleOrchestrator
	}

	defaultAdvertise := defaults.advertise
	if env := strings.TrimSpace(os.Getenv("ADVERTISE")); env != "" {
		if val, err := strconv.ParseBool(env); err == nil {
			defaultAdvertise = val
		}
	}

	defaultInstance := deriveHostname()
	if env := strings.TrimSpace(os.Getenv("INSTANCE_NAME")); env != "" {
		defaultInstance = env
	}

	defaultDescription := strings.TrimSpace(os.Getenv("DESCRIPTION"))

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	modelFlag := fs.String("model", agentModelDefault, "ID of the base model exposed by this agent (required)")
	apiModelFlag := fs.String("api-model", defaultAPIModelValue, "Model name exposed to API clients")
	portFlag := fs.Int("port", 0, "HTTP port (overrides PORT env)")
	baseURLFlag := fs.String("base-url", defaultBaseURLValue, "Base URL for the upstream OpenAI-compatible endpoint (e.g. http://host:port/v1)")
	advertiseFlag := fs.Bool("advertise", defaultAdvertise, "Publish this agent over mDNS")
	instanceFlag := fs.String("instance", defaultInstance, "Instance name advertised over mDNS")
	roleFlag := fs.String("role", defaultRole, "Role advertised over mDNS (orchestrator, agent-wrapper, ...)")
	descriptionFlag := fs.String("description", defaultDescription, "Human readable description for this agent/tool")
	apiKeyFlag := fs.String("api-key", defaultAPIKeyValue, "API key for the upstream endpoint")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return cfg, err
	}

	cfg.BackendModel = firstNonEmpty(*modelFlag, agentModelDefault)
	if cfg.BackendModel == "" {
		return cfg, errors.New("base model is required (pass --model or set AGENT_MODEL)")
	}

	cfg.APIModel = strings.TrimSpace(*apiModelFlag)
	if cfg.APIModel == "" {
		cfg.APIModel = defaultAPIModel
	}

	cfg.Port = resolvePort(*portFlag, os.Getenv("PORT"), defaultPort)

	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(*baseURLFlag), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}

	cfg.Advertise = *advertiseFlag
	cfg.Instance = strings.TrimSpace(*instanceFlag)
	if cfg.Instance == "" {
		cfg.Instance = deriveHostname()
	}

	cfg.Role = strings.TrimSpace(*roleFlag)
	if cfg.Role == "" {
		cfg.Role = defaultRole
	}

	cfg.Description = strings.TrimSpace(*descriptionFlag)
	cfg.APIKey = strings.TrimSpace(*apiKeyFlag)
	if cfg.APIKey == "" {
		cfg.APIKey = defaultAPIKey
	}

	return cfg, nil
}

func resolvePort(flagValue int, envValue string, fallback int) int {
	if flagValue > 0 {
		return flagValue
	}
	if envValue != "" {
		if port, err := strconv.Atoi(envValue); err == nil && port > 0 {
			return port
		}
		fmt.Fprintf(os.Stderr, "invalid PORT value %q, falling back to default\n", envValue)
	}
	if fallback > 0 {
		return fallback
	}
	return defaultPort
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func deriveHostname() string {
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		return hostname
	}
	return "mcp-agent"
}
