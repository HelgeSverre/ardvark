package jsonout

import (
	"os"
	"path/filepath"

	"github.com/helgesverre/ardvark/internal/config"
)

// InfoReport describes the running ardvark installation: version, config
// file, storage backend, and event log locations. Produced by `ardvark info`
// and the ardvark_info MCP tool.
type InfoReport struct {
	Version string      `json:"version"`
	Config  InfoConfig  `json:"config"`
	Storage InfoStorage `json:"storage"`
	Log     InfoLog     `json:"log"`
}

// InfoConfig locates the ardvark.json config file. Exists is false when the
// path does not exist (the CLI then runs on pure defaults).
type InfoConfig struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// InfoStorage describes the configured database backend. Path, Exists, and
// SizeBytes are resolved for the sqlite driver only; server backends (mysql,
// postgres) report just Driver and DSN.
type InfoStorage struct {
	Driver    string `json:"driver"`
	DSN       string `json:"dsn"`
	Path      string `json:"path,omitempty"`
	Exists    bool   `json:"exists"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// InfoLog locates the JSONL event log.
type InfoLog struct {
	File  string `json:"file"`
	Level string `json:"level"`
}

// Info resolves installation metadata for a loaded config. Relative paths
// are made absolute so the report is unambiguous regardless of the caller's
// working directory. Info only stats files — it never opens the database or
// touches the network, so it works even when the database is unusable.
func Info(cfg config.Config, configPath, version string) InfoReport {
	rep := InfoReport{Version: version}

	rep.Config.Path = absPath(configPath)
	rep.Config.Exists = fileExists(configPath)

	rep.Storage.Driver = cfg.Storage.Driver
	rep.Storage.DSN = cfg.Storage.DSN
	if cfg.Storage.Driver == "sqlite" {
		rep.Storage.Path = absPath(cfg.Storage.DSN)
		if fi, err := os.Stat(cfg.Storage.DSN); err == nil && !fi.IsDir() {
			rep.Storage.Exists = true
			rep.Storage.SizeBytes = fi.Size()
		}
	}

	rep.Log.File = absPath(cfg.Log.File)
	rep.Log.Level = cfg.Log.Level

	return rep
}

// absPath returns p made absolute, or p unchanged if resolution fails.
func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// fileExists reports whether p exists and is a regular file.
func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
