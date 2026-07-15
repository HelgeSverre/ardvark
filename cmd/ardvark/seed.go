package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/ctseed"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/store"
)

var (
	seedCTCount int
	seedCTLog   string
)

// seedCmd is the parent command for pluggable seed sources.
var seedCmd = &cobra.Command{
	Use:   "seed",
	Short: "Bootstrap the frontier from an external domain source",
}

var seedCTCmd = &cobra.Command{
	Use:   "ct",
	Short: "Seed the frontier from Certificate Transparency log entries",
	Long: "seed ct fetches the most recent N entries from a Certificate Transparency log " +
		"(default: Let's Encrypt Oak, per config ctSeed.logUrl), extracts SAN domain names, " +
		"sanitizes and dedupes them, upserts them as domains (discovery_source=ct_log), " +
		"and enqueues a host_probe frontier item for each.",
	RunE: runSeedCT,
}

func init() {
	seedCTCmd.Flags().IntVar(&seedCTCount, "count", 0, "number of CT log entries to fetch (default: config ctSeed.entryCount)")
	seedCTCmd.Flags().StringVar(&seedCTLog, "log", "", "CT log base URL (default: config ctSeed.logUrl)")
	seedCmd.AddCommand(seedCTCmd)
	rootCmd.AddCommand(seedCmd)
}

func runSeedCT(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	count := cfg.CTSeed.EntryCount
	if seedCTCount > 0 {
		count = seedCTCount
	}
	logURL := cfg.CTSeed.LogURL
	if seedCTLog != "" {
		logURL = seedCTLog
	}

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	logger, err := newLogger(cfg)
	if err != nil {
		return err
	}

	client := ctseed.NewClient(logURL)
	names, err := client.FetchLatest(cmd.Context(), count)
	if err != nil {
		return fmt.Errorf("seed ct: %w", err)
	}

	fr := frontier.New(st.DB)
	fc := newFetchClient(cfg)
	eng := crawler.New(cfg, st, fr, fc, logger, crawler.Options{})

	var added, skipped int
	for _, host := range names {
		ok, err := eng.EnqueueSeedHost(host, store.DiscoverySourceCTLog)
		if err != nil {
			skipped++
			continue
		}
		if ok {
			added++
		} else {
			skipped++
		}
	}

	p := printer(cmd)
	p.Summary("seed ct complete: ",
		fmt.Sprintf("%d domains from %s", len(names), logURL),
		fmt.Sprintf("%d added", added),
		fmt.Sprintf("%d skipped (already queued)", skipped),
	)
	return nil
}
