// Package eventlog provides the crawl-event slog setup: every significant
// crawl event (probe result, catalog verified, item failed, registry
// harvested, ...) is written as a JSON line to the log file. Human-readable
// progress is the command's own concern (the CLI prints live result rows),
// so this log is the machine-readable record.
package eventlog

import (
	"fmt"
	"log/slog"
	"os"
)

// New returns a *slog.Logger that writes structured JSON lines to the file at
// filePath (created/appended) at the given level. slog.Logger has no Close, so
// the file is closed when the process exits.
func New(filePath string, level slog.Level) (*slog.Logger, error) {
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("eventlog: opening %s: %w", filePath, err)
	}

	handler := slog.NewJSONHandler(f, &slog.HandlerOptions{Level: level})
	return slog.New(handler), nil
}
