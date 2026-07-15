package jsonout

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/helgesverre/ardvark/internal/config"
)

// Info must resolve absolute paths, report existence and size for sqlite
// databases, and carry the version through verbatim.
func TestInfo(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ardvark.json")
	dbPath := filepath.Join(dir, "test.db")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if err := os.WriteFile(dbPath, []byte("sqlite bytes"), 0o644); err != nil {
		t.Fatalf("writing db: %v", err)
	}

	cfg := config.Defaults()
	cfg.Storage.DSN = dbPath
	cfg.Log.File = filepath.Join(dir, "test.jsonl")

	rep := Info(cfg, cfgPath, "1.2.3")

	if rep.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", rep.Version)
	}
	if rep.Config.Path != cfgPath {
		t.Errorf("Config.Path = %q, want %q", rep.Config.Path, cfgPath)
	}
	if !rep.Config.Exists {
		t.Error("Config.Exists = false, want true")
	}
	if rep.Storage.Driver != "sqlite" {
		t.Errorf("Storage.Driver = %q, want sqlite", rep.Storage.Driver)
	}
	if rep.Storage.Path != dbPath {
		t.Errorf("Storage.Path = %q, want %q", rep.Storage.Path, dbPath)
	}
	if !rep.Storage.Exists {
		t.Error("Storage.Exists = false, want true")
	}
	if want := int64(len("sqlite bytes")); rep.Storage.SizeBytes != want {
		t.Errorf("Storage.SizeBytes = %d, want %d", rep.Storage.SizeBytes, want)
	}
	if rep.Log.Level != "info" {
		t.Errorf("Log.Level = %q, want info", rep.Log.Level)
	}
	if !filepath.IsAbs(rep.Log.File) {
		t.Errorf("Log.File = %q, want absolute path", rep.Log.File)
	}
}

// A missing config file and database must be reported as absent, not
// created; relative paths must still resolve to absolute ones.
func TestInfoMissingFiles(t *testing.T) {
	cfg := config.Defaults() // dsn "ardvark.db" relative to cwd

	rep := Info(cfg, filepath.Join(t.TempDir(), "nope.json"), "dev")

	if rep.Config.Exists {
		t.Error("Config.Exists = true for missing file, want false")
	}
	if !filepath.IsAbs(rep.Storage.Path) {
		t.Errorf("Storage.Path = %q, want absolute path", rep.Storage.Path)
	}
	if !filepath.IsAbs(rep.Config.Path) {
		t.Errorf("Config.Path = %q, want absolute path", rep.Config.Path)
	}
}

// Server backends have no local database file: Path, Exists, and SizeBytes
// must stay zero while Driver and DSN pass through.
func TestInfoServerBackend(t *testing.T) {
	cfg := config.Defaults()
	cfg.Storage.Driver = "postgres"
	cfg.Storage.DSN = "host=localhost user=ardvark dbname=ardvark"

	rep := Info(cfg, "ardvark.json", "dev")

	if rep.Storage.Path != "" {
		t.Errorf("Storage.Path = %q, want empty for postgres", rep.Storage.Path)
	}
	if rep.Storage.Exists {
		t.Error("Storage.Exists = true, want false for postgres")
	}
	if rep.Storage.DSN != cfg.Storage.DSN {
		t.Errorf("Storage.DSN = %q, want %q", rep.Storage.DSN, cfg.Storage.DSN)
	}
}
