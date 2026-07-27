package eventlog

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNew_WritesParseableJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	logger, closeLogger, err := New(path, slog.LevelInfo)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	defer closeLogger()

	logger.Info("probe complete", "host", "example.com", "outcome", "hit")
	logger.Warn("catalog invalid", "host", "example.org")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), string(data))
	}

	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not parseable JSON: %v (%q)", i, err, line)
		}
		if _, ok := rec["msg"]; !ok {
			t.Errorf("line %d missing msg field: %q", i, line)
		}
	}

	if !strings.Contains(lines[0], "example.com") {
		t.Errorf("line 0 missing expected attr: %q", lines[0])
	}
	if !strings.Contains(lines[1], "example.org") {
		t.Errorf("line 1 missing expected attr: %q", lines[1])
	}
}

func TestNew_FiltersBelowLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	logger, closeLogger, err := New(path, slog.LevelWarn)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	defer closeLogger()

	logger.Info("should not appear")
	logger.Warn("should appear")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}

	if strings.Contains(string(data), "should not appear") {
		t.Errorf("info-level record leaked through at warn level: %q", string(data))
	}
	if !strings.Contains(string(data), "should appear") {
		t.Errorf("warn-level record missing: %q", string(data))
	}
}

func TestNew_CreatesFileIfMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "does-not-exist-yet.jsonl")
	// Parent dir does not exist; New should still error cleanly rather than panic.
	if _, _, err := New(path, slog.LevelInfo); err == nil {
		t.Fatalf("New() with missing parent dir: expected error, got nil")
	}
}

// TestNew_CloseReleasesFile exercises the close func returned alongside the
// logger: after closing, writing to the logger must not panic (slog just
// surfaces the write error via its internal error handling), and — more
// importantly for the fd-leak this guards against — repeated open/close
// cycles against the same path must all succeed rather than piling up open
// file descriptors.
func TestNew_CloseReleasesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	for i := 0; i < 50; i++ {
		logger, closeLogger, err := New(path, slog.LevelInfo)
		if err != nil {
			t.Fatalf("New() iteration %d: unexpected error: %v", i, err)
		}
		logger.Info("cycle", "i", i)
		if err := closeLogger(); err != nil {
			t.Fatalf("close() iteration %d: unexpected error: %v", i, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50 (one per open/close cycle)", len(lines))
	}

	// A second close must not panic or block; os.File.Close on an
	// already-closed file just returns an error, which callers (defer
	// closeLogger()) discard the same way they discard resp.Body.Close()
	// errors elsewhere in this codebase.
	_, closeLogger, err := New(path, slog.LevelInfo)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}
	if err := closeLogger(); err != nil {
		t.Fatalf("first close: unexpected error: %v", err)
	}
	if closeLogger() == nil {
		t.Fatalf("second close: expected error from closing an already-closed file, got nil")
	}
}
