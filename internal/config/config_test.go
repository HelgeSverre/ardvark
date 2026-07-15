package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoad_MissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	want := Defaults()
	if !reflect.DeepEqual(cfg, want) {
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

func TestLoadBytes_ZeroFractionFloatsDecodeIntoIntFields(t *testing.T) {
	// JSON Schema "integer" accepts zero-fraction floats, so files written
	// as 2.0 or 1e3 pass validation and must load.
	cfg, err := LoadBytes([]byte(`{
		"crawler": {"maxDepth": 2.0},
		"seed": {"ct": {"entryCount": 1e3}}
	}`))
	if err != nil {
		t.Fatalf("LoadBytes() unexpected error: %v", err)
	}
	if cfg.Crawler.MaxDepth != 2 {
		t.Errorf("Crawler.MaxDepth = %d, want 2", cfg.Crawler.MaxDepth)
	}
	if cfg.Seed.CT.EntryCount != 1000 {
		t.Errorf("Seed.CT.EntryCount = %d, want 1000", cfg.Seed.CT.EntryCount)
	}
}

func TestLoadBytes_SeedSourceConfigParses(t *testing.T) {
	cfg, err := LoadBytes([]byte(`{
		"seed": {
			"crtsh":  {"count": 50},
			"tranco": {"top": 200},
			"github": {"query": "filename:foo.json", "count": 10},
			"mcp":    {"registryUrl": "https://example.com/registry", "count": 5}
		}
	}`))
	if err != nil {
		t.Fatalf("LoadBytes() unexpected error: %v", err)
	}
	if cfg.Seed.Crtsh.Count != 50 {
		t.Errorf("Seed.Crtsh.Count = %d, want 50", cfg.Seed.Crtsh.Count)
	}
	if cfg.Seed.Tranco.Top != 200 {
		t.Errorf("Seed.Tranco.Top = %d, want 200", cfg.Seed.Tranco.Top)
	}
	if cfg.Seed.GitHub.Query != "filename:foo.json" || cfg.Seed.GitHub.Count != 10 {
		t.Errorf("Seed.GitHub = %+v, want query=filename:foo.json count=10", cfg.Seed.GitHub)
	}
	if cfg.Seed.MCPRegistry.RegistryURL != "https://example.com/registry" || cfg.Seed.MCPRegistry.Count != 5 {
		t.Errorf("Seed.MCPRegistry = %+v, want registryUrl=https://example.com/registry count=5", cfg.Seed.MCPRegistry)
	}
	// Untouched sibling keys keep their defaults.
	if cfg.Seed.CT.EntryCount != Defaults().Seed.CT.EntryCount {
		t.Errorf("Seed.CT.EntryCount = %d, want default %d", cfg.Seed.CT.EntryCount, Defaults().Seed.CT.EntryCount)
	}
}

func TestLoadBytes_CuratedAndCommonCrawlConfigParses(t *testing.T) {
	cfg, err := LoadBytes([]byte(`{
		"seed": {
			"curated":     {"urls": ["https://example.com/list.md"], "count": 25},
			"commoncrawl": {"graphInfoUrl": "https://example.com/graphinfo.json", "graph": "cc-test-2026", "top": 10, "offset": 5}
		}
	}`))
	if err != nil {
		t.Fatalf("LoadBytes() unexpected error: %v", err)
	}
	if len(cfg.Seed.Curated.URLs) != 1 || cfg.Seed.Curated.URLs[0] != "https://example.com/list.md" {
		t.Errorf("Seed.Curated.URLs = %v, want [https://example.com/list.md]", cfg.Seed.Curated.URLs)
	}
	if cfg.Seed.Curated.Count != 25 {
		t.Errorf("Seed.Curated.Count = %d, want 25", cfg.Seed.Curated.Count)
	}
	cc := cfg.Seed.CommonCrawl
	if cc.GraphInfoURL != "https://example.com/graphinfo.json" || cc.Graph != "cc-test-2026" || cc.Top != 10 || cc.Offset != 5 {
		t.Errorf("Seed.CommonCrawl = %+v, want graphInfoUrl=https://example.com/graphinfo.json graph=cc-test-2026 top=10 offset=5", cc)
	}
	// Untouched sibling keys keep their defaults.
	if cfg.Seed.Tranco.Top != Defaults().Seed.Tranco.Top {
		t.Errorf("Seed.Tranco.Top = %d, want default %d", cfg.Seed.Tranco.Top, Defaults().Seed.Tranco.Top)
	}
}

func TestDefaults_SeedSourcesHaveOwnCounts(t *testing.T) {
	d := Defaults()
	if d.Seed.Crtsh.Count <= 0 {
		t.Errorf("Seed.Crtsh.Count = %d, want positive default", d.Seed.Crtsh.Count)
	}
	if d.Seed.Tranco.Top <= 0 {
		t.Errorf("Seed.Tranco.Top = %d, want positive default", d.Seed.Tranco.Top)
	}
	if d.Seed.GitHub.Count <= 0 || d.Seed.GitHub.Query == "" {
		t.Errorf("Seed.GitHub = %+v, want positive count and non-empty query", d.Seed.GitHub)
	}
	if d.Seed.MCPRegistry.Count <= 0 || d.Seed.MCPRegistry.RegistryURL == "" {
		t.Errorf("Seed.MCPRegistry = %+v, want positive count and non-empty registry URL", d.Seed.MCPRegistry)
	}
	if d.Seed.Curated.Count <= 0 || len(d.Seed.Curated.URLs) == 0 {
		t.Errorf("Seed.Curated = %+v, want positive count and non-empty URL set", d.Seed.Curated)
	}
	if d.Seed.CommonCrawl.Top <= 0 || d.Seed.CommonCrawl.GraphInfoURL == "" {
		t.Errorf("Seed.CommonCrawl = %+v, want positive top and non-empty graphinfo URL", d.Seed.CommonCrawl)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test fixture %s: %v", path, err)
	}
}
