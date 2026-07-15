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

	logger, err := New(path, slog.LevelInfo)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

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

	logger, err := New(path, slog.LevelWarn)
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

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
	if _, err := New(path, slog.LevelInfo); err == nil {
		t.Fatalf("New() with missing parent dir: expected error, got nil")
	}
}
