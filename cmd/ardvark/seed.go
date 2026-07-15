package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/seed"
)

var (
	seedCTCount int
	seedCTLog   string

	seedCrtshCount int
	seedCrtshMatch string

	seedTrancoTop int
	seedTrancoURL string
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
		"(default: Let's Encrypt Oak, per config seed.ct.logUrl), extracts SAN domain names, " +
		"sanitizes and dedupes them, upserts them as domains (discovery_source=ct_log), " +
		"and enqueues a host_probe frontier item for each.",
	RunE: runSeedCT,
}

var seedCrtshCmd = &cobra.Command{
	Use:   "crtsh",
	Short: "Seed the frontier from crt.sh certificate search",
	Long: "seed crtsh queries crt.sh's JSON API for recent certificates (optionally narrowed " +
		"to identities mentioning --match), extracts domain names, sanitizes and dedupes them, " +
		"upserts them as domains (discovery_source=crtsh), and enqueues a host_probe frontier " +
		"item for each.",
	RunE: runSeedCrtsh,
}

var seedTrancoCmd = &cobra.Command{
	Use:   "tranco",
	Short: "Seed the frontier from the Tranco top-domains list",
	Long: "seed tranco downloads the Tranco top-domains list (per config seed.tranco.listUrl), " +
		"sanitizes and dedupes the top N domains, upserts them as domains " +
		"(discovery_source=tranco), and enqueues a host_probe frontier item for each. " +
		"Complements seed ct: it covers the established web that CT-log seeding (which only " +
		"sees freshly issued certs) misses.",
	RunE: runSeedTranco,
}

func init() {
	seedCTCmd.Flags().IntVar(&seedCTCount, "count", 0, "number of CT log entries to fetch (default: config seed.ct.entryCount)")
	seedCTCmd.Flags().StringVar(&seedCTLog, "log", "", "CT log base URL (default: config seed.ct.logUrl)")

	seedCrtshCmd.Flags().IntVar(&seedCrtshCount, "count", 0, "number of domains to enqueue (default: config seed.ct.entryCount)")
	seedCrtshCmd.Flags().StringVar(&seedCrtshMatch, "match", "", "narrow crt.sh results to identities mentioning this keyword")

	seedTrancoCmd.Flags().IntVar(&seedTrancoTop, "top", 0, "number of top domains to enqueue (default: config seed.ct.entryCount)")
	seedTrancoCmd.Flags().StringVar(&seedTrancoURL, "url", "", "Tranco list URL (default: config seed.tranco.listUrl)")

	seedCmd.AddCommand(seedCTCmd)
	seedCmd.AddCommand(seedCrtshCmd)
	seedCmd.AddCommand(seedTrancoCmd)
	rootCmd.AddCommand(seedCmd)
}

func runSeedCT(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	count := cfg.Seed.CT.EntryCount
	if seedCTCount > 0 {
		count = seedCTCount
	}
	logURL := cfg.Seed.CT.LogURL
	if seedCTLog != "" {
		logURL = seedCTLog
	}

	return runSeeder(cmd, cfg, seed.NewCTSeeder(logURL), count,
		fmt.Sprintf("%d domains from %s", count, logURL))
}

func runSeedCrtsh(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	count := cfg.Seed.CT.EntryCount
	if seedCrtshCount > 0 {
		count = seedCrtshCount
	}

	crtshSeeder := &seed.CrtshSeeder{
		Endpoint: cfg.Seed.Crtsh.Endpoint,
		Match:    seedCrtshMatch,
	}

	label := fmt.Sprintf("%d domains from %s", count, crtshSeeder.Endpoint)
	if seedCrtshMatch != "" {
		label = fmt.Sprintf("%s (match=%q)", label, seedCrtshMatch)
	}

	return runSeeder(cmd, cfg, crtshSeeder, count, label)
}

func runSeedTranco(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	top := cfg.Seed.CT.EntryCount
	if seedTrancoTop > 0 {
		top = seedTrancoTop
	}
	listURL := cfg.Seed.Tranco.ListURL
	if seedTrancoURL != "" {
		listURL = seedTrancoURL
	}

	return runSeeder(cmd, cfg, seed.NewTrancoSeeder(listURL), top,
		fmt.Sprintf("top %d domains from %s", top, listURL))
}

// runSeeder drives a seed.Seeder to completion: fetch up to n candidate
// domains, then upsert each as a domains row and enqueue a host_probe
// frontier item, tagged with the seeder's discovery_source. Shared by every
// `seed <source>` subcommand so each one only has to build its Seeder and
// resolve its flags/config.
func runSeeder(cmd *cobra.Command, cfg config.Config, s seed.Seeder, n int, sourceLabel string) error {
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	logger, err := newLogger(cfg)
	if err != nil {
		return err
	}

	names, err := s.Domains(cmd.Context(), n)
	if err != nil {
		return fmt.Errorf("seed %s: %w", s.Source(), err)
	}

	fr := frontier.New(st.DB)
	fc := newFetchClient(cfg)
	eng := crawler.New(cfg, st, fr, fc, logger, crawler.Options{})

	var added, skipped int
	for _, host := range names {
		ok, err := eng.EnqueueSeedHost(host, s.Source())
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
	p.Summary(fmt.Sprintf("seed %s complete: ", s.Source()),
		sourceLabel,
		fmt.Sprintf("%d added", added),
		fmt.Sprintf("%d skipped (already queued)", skipped),
	)
	return nil
}
