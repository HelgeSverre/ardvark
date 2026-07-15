package main

import (
	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/jsonout"
)

var infoCmd = &cobra.Command{
	Use:   "info",
	Short: "Show installation metadata: version, config path, database and log locations",
	Long: "info reports the running ardvark installation's metadata: binary version, resolved config " +
		"file path (and whether it exists), the configured storage backend with the absolute sqlite " +
		"database location, and the event log file. It never opens the database, so it works even " +
		"when storage is misconfigured.",
	Args: cobra.NoArgs,
	RunE: runInfo,
}

func init() {
	addJSONFlag(infoCmd)
	rootCmd.AddCommand(infoCmd)
}

func runInfo(cmd *cobra.Command, args []string) error {
	cfgPath := resolvedConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	report := jsonout.Info(cfg, cfgPath, version)

	if jsonOut {
		return printJSON(cmd, report)
	}

	p := printer(cmd)
	p.Header("ardvark")
	p.KV("version", report.Version)

	p.Header("config")
	p.KV("path", report.Config.Path)
	p.KV("exists", report.Config.Exists)

	p.Header("storage")
	p.KV("driver", report.Storage.Driver)
	p.KV("dsn", report.Storage.DSN)
	if report.Storage.Path != "" {
		p.KV("path", report.Storage.Path)
		p.KV("exists", report.Storage.Exists)
		if report.Storage.Exists {
			p.KV("size", report.Storage.SizeBytes)
		}
	}

	p.Header("log")
	p.KV("file", report.Log.File)
	p.KV("level", report.Log.Level)
	return nil
}
