package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/eventlog"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/store"
	"github.com/helgesverre/ardvark/internal/ui"
)

// configPath is bound to the root --config persistent flag.
var configPath string

// rootCmd is the ardvark CLI entry point. Subcommands are added to it in
// their own files' init() functions.
var rootCmd = &cobra.Command{
	Use:           "ardvark",
	Short:         "Crawl the web for ARD ai-catalog.json documents",
	Long:          "ardvark discovers Agentic Resource Discovery (ARD) ai-catalog.json documents on the web, verifies them against the spec, and records every discovered agentic resource to a database and a JSONL event log.",
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "./ardvark.json", "path to ardvark.json config file")
}

// Execute runs the root command, printing any error to stderr.
func Execute() error {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return err
	}
	return nil
}

// loadConfig reads and validates ardvark.json from the --config path. A
// missing file is not an error: config.Load returns pure defaults.
func loadConfig() (config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

// openStore opens the configured database and runs AutoMigrate.
func openStore(cfg config.Config) (*store.Store, error) {
	st, err := store.Open(cfg.Storage.Driver, cfg.Storage.DSN)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// newLogger builds the crawl-event logger from cfg.Log: JSONL to
// cfg.Log.File, human-readable text to stderr.
func newLogger(cfg config.Config) (*slog.Logger, error) {
	level := parseLevel(cfg.Log.Level)
	return eventlog.New(cfg.Log.File, level)
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

// newFetchClient builds a polite fetch.Client from the crawler config.
func newFetchClient(cfg config.Config) *fetch.Client {
	return fetch.New(cfg.Crawler)
}

// printer returns a ui.Printer writing to cmd's configured stdout, so
// output composes correctly with cobra's own output redirection in tests.
func printer(cmd *cobra.Command) *ui.Printer {
	return ui.New(cmd.OutOrStdout())
}

// errPrinter returns a ui.Printer writing to cmd's configured stderr.
func errPrinter(cmd *cobra.Command) *ui.Printer {
	return ui.New(cmd.ErrOrStderr())
}
