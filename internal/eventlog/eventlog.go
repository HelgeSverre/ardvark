// Package eventlog provides the crawl-event slog setup: every significant
// crawl event (probe result, catalog verified, item failed, registry
// harvested, ...) is written once as a JSON line to a log file and once as
// human-readable text to stderr.
package eventlog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

// New returns a *slog.Logger that writes structured JSON lines to the file
// at filePath (created/appended) and human-readable text to stderr, both at
// the given level. The returned io.Closer-like cleanup is the caller's
// responsibility only insofar as the process exiting closes the file; New
// does not expose the underlying *os.File since slog.Logger has no Close.
func New(filePath string, level slog.Level) (*slog.Logger, error) {
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("eventlog: opening %s: %w", filePath, err)
	}

	opts := &slog.HandlerOptions{Level: level}
	jsonHandler := slog.NewJSONHandler(f, opts)
	textHandler := slog.NewTextHandler(os.Stderr, opts)

	logger := slog.New(newMultiHandler(jsonHandler, textHandler))
	return logger, nil
}

// multiHandler fans out slog records to multiple handlers, satisfying
// slog.Handler.
type multiHandler struct {
	handlers []slog.Handler
}

func newMultiHandler(handlers ...slog.Handler) *multiHandler {
	return &multiHandler{handlers: handlers}
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return newMultiHandler(next...)
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return newMultiHandler(next...)
}
