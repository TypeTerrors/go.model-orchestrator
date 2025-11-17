package logging

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	log "github.com/charmbracelet/log"
)

// Config describes how runtime logging should behave.
type Config struct {
	Output    io.Writer
	Level     log.Level
	Prefix    string
	UseColors bool
}

// New builds a log.Logger instance with consistent styling across binaries.
func New(cfg Config) *log.Logger {
	writer := cfg.Output
	if writer == nil {
		writer = os.Stdout
	}
	logger := log.NewWithOptions(writer, log.Options{
		Level:           cfg.Level,
		Prefix:          renderPrefix(cfg.Prefix),
		ReportTimestamp: true,
		TimeFormat:      "15:04:05",
	})
	if !cfg.UseColors {
		applyNoColorStyles(logger)
	}
	return logger
}

// FromEnv derives logging preferences from environment variables.
func FromEnv(prefix string) *log.Logger {
	level := parseLevel(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	useColors := true
	if value := strings.TrimSpace(os.Getenv("LOG_NO_COLOR")); value != "" {
		useColors = !strings.EqualFold(value, "true")
	}
	return New(Config{
		Output:    os.Stdout,
		Level:     level,
		Prefix:    prefix,
		UseColors: useColors,
	})
}

func parseLevel(value string) log.Level {
	switch strings.ToLower(value) {
	case "debug":
		return log.DebugLevel
	case "warn", "warning":
		return log.WarnLevel
	case "error":
		return log.ErrorLevel
	case "fatal":
		return log.FatalLevel
	default:
		return log.InfoLevel
	}
}

func renderPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return ""
	}
	return prefix + " "
}

func applyNoColorStyles(logger *log.Logger) {
	styles := log.DefaultStyles()
	styles.Timestamp = baseStyle()
	styles.Caller = baseStyle()
	styles.Prefix = baseStyle()
	styles.Message = baseStyle()
	styles.Key = baseStyle()
	styles.Value = baseStyle()
	styles.Separator = baseStyle()

	for level := range styles.Levels {
		styles.Levels[level] = lipgloss.NewStyle().SetString(strings.ToUpper(level.String()))
	}
	styles.Keys = map[string]lipgloss.Style{}
	styles.Values = map[string]lipgloss.Style{}

	logger.SetStyles(styles)
}

func baseStyle() lipgloss.Style {
	return lipgloss.NewStyle()
}
