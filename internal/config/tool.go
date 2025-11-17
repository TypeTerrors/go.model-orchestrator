package config

import (
	"flag"
	"os"
	"strconv"
	"strings"
)

// ToolConfig captures configuration for standalone MCP tool servers.
type ToolConfig struct {
	Port      int
	Advertise bool
	Instance  string
	Role      string
}

const defaultToolRole = "tool"

// LoadToolServer parses flags/env to configure an MCP tool server.
func LoadToolServer() (ToolConfig, error) {
	var cfg ToolConfig

	defaultAdvertise := true
	if env := strings.TrimSpace(os.Getenv("ADVERTISE")); env != "" {
		if val, err := strconv.ParseBool(env); err == nil {
			defaultAdvertise = val
		}
	}

	defaultInstance := deriveHostname()
	if env := strings.TrimSpace(os.Getenv("INSTANCE_NAME")); env != "" {
		defaultInstance = env
	}

	defaultRole := defaultToolRole
	if env := strings.TrimSpace(os.Getenv("ROLE")); env != "" {
		defaultRole = env
	}

	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	portFlag := fs.Int("port", 0, "HTTP port (overrides PORT env)")
	advertiseFlag := fs.Bool("advertise", defaultAdvertise, "Publish this tool server over mDNS")
	instanceFlag := fs.String("instance", defaultInstance, "Instance name advertised over mDNS")
	roleFlag := fs.String("role", defaultRole, "Role advertised over mDNS")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return cfg, err
	}

	cfg.Port = resolvePort(*portFlag, os.Getenv("PORT"), defaultPort)
	cfg.Advertise = *advertiseFlag
	cfg.Instance = strings.TrimSpace(*instanceFlag)
	if cfg.Instance == "" {
		cfg.Instance = deriveHostname()
	}
	cfg.Role = strings.TrimSpace(*roleFlag)
	if cfg.Role == "" {
		cfg.Role = defaultToolRole
	}

	return cfg, nil
}
