package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_MissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	want := Defaults()
	if cfg != want {
		t.Fatalf("Load() = %+v, want defaults %+v", cfg, want)
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ardvark.json")
	writeFile(t, path, `{
		"storage": {"driver": "postgres", "dsn": "postgres://localhost/ardvark"},
		"crawler": {"concurrency": 16}
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Storage.Driver != "postgres" {
		t.Errorf("Storage.Driver = %q, want postgres", cfg.Storage.Driver)
	}
	if cfg.Storage.DSN != "postgres://localhost/ardvark" {
		t.Errorf("Storage.DSN = %q, want postgres dsn", cfg.Storage.DSN)
	}
	if cfg.Crawler.Concurrency != 16 {
		t.Errorf("Crawler.Concurrency = %d, want 16", cfg.Crawler.Concurrency)
	}
	// Unspecified keys keep defaults.
	if cfg.Crawler.MaxDepth != Defaults().Crawler.MaxDepth {
		t.Errorf("Crawler.MaxDepth = %d, want default %d", cfg.Crawler.MaxDepth, Defaults().Crawler.MaxDepth)
	}
	if cfg.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want default info", cfg.Log.Level)
	}
}

func TestLoadBytes_InvalidCases(t *testing.T) {
	tests := []struct {
		name       string
		json       string
		wantSubstr string
	}{
		{
			name:       "bad storage driver",
			json:       `{"storage": {"driver": "oracle"}}`,
			wantSubstr: `config.storage.driver`,
		},
		{
			name:       "bad log level",
			json:       `{"log": {"level": "verbose"}}`,
			wantSubstr: `config.log.level`,
		},
		{
			name:       "zero concurrency",
			json:       `{"crawler": {"concurrency": 0}}`,
			wantSubstr: `config.crawler.concurrency`,
		},
		{
			name:       "negative max depth",
			json:       `{"crawler": {"maxDepth": -1}}`,
			wantSubstr: `config.crawler.maxDepth`,
		},
		{
			name:       "unknown top-level key",
			json:       `{"bogus": true}`,
			wantSubstr: `config`,
		},
		{
			name:       "malformed json",
			json:       `{not json`,
			wantSubstr: `invalid JSON`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tt.json))
			if err == nil {
				t.Fatalf("LoadBytes(%q) expected error, got nil", tt.json)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("LoadBytes(%q) error = %q, want substring %q", tt.json, err.Error(), tt.wantSubstr)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test fixture %s: %v", path, err)
	}
}
