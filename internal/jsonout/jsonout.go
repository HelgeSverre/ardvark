// Package jsonout holds the typed, JSON-serializable result structures each
// ardvark command computes, plus the shared cores that produce them. The CLI
// (cmd/ardvark) renders these results either through ui.Printer (default
// human output) or as pretty-printed JSON (--json); the embedded MCP server
// (internal/mcpserver) returns the same structures as tool results. Cores
// never print: live progress is surfaced through optional callbacks so the
// CLI's default output stays byte-identical to the pre-refactor commands.
package jsonout

import (
	"log/slog"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/eventlog"
)

// KeyCount is one GROUP BY bucket in a stats breakdown, ordered by key.
type KeyCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// NewLogger builds the crawl-event logger from cfg.Log: JSONL to
// cfg.Log.File, human-readable text to stderr (never stdout, so the MCP
// stdio protocol and --json output stay clean). Callers must invoke the
// returned close func (typically via defer) once done with the logger to
// release the underlying file descriptor.
func NewLogger(cfg config.Config) (*slog.Logger, func() error, error) {
	return eventlog.New(cfg.Log.File, parseLevel(cfg.Log.Level))
}

// parseLevel maps a config log level string to a slog.Level, defaulting to
// Info for an empty or unrecognized value.
func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// orDefault returns flag when set (non-zero value), else def — the
// flag-over-config precedence applied to counts and offsets.
func orDefault[T comparable](flag, def T) T {
	var zero T
	if flag != zero {
		return flag
	}
	return def
}
