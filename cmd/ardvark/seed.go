package main

import (
	"fmt"
	"strings"
	"time"

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
	Short: "Seed from Certificate Transparency logs",
	Long: "Harvest domains from the newest Certificate Transparency log entries and queue them " +
		"for probing. Logs are resolved live from the CT log list (Let's Encrypt Oak by default), " +
		"so shard URLs never need updating as they rotate.",
	RunE: runSeedCT,
}

var seedCrtshCmd = &cobra.Command{
	Use:   "crtsh",
	Short: "Seed from crt.sh certificate search",
	Long: "Harvest domains from crt.sh's certificate search and queue them for probing. " +
		"Use --match to narrow to certificates whose identity mentions a keyword (e.g. \"agent\", \"mcp\").",
	RunE: runSeedCrtsh,
}

var seedTrancoCmd = &cobra.Command{
	Use:   "tranco",
	Short: "Seed from the Tranco top-domains list",
	Long: "Queue the top N domains from the Tranco list for probing. Covers the established web " +
		"that CT-log seeding, which only sees freshly issued certificates, misses.",
	RunE: runSeedTranco,
}

func init() {
	seedCTCmd.Flags().IntVar(&seedCTCount, "count", 0, "entries to harvest (default: config seed.ct.entryCount)")
	seedCTCmd.Flags().StringVar(&seedCTLog, "log", "", "operator token (oak, argon, all) or explicit log URL (default: config seed.ct.logs)")

	seedCrtshCmd.Flags().IntVar(&seedCrtshCount, "count", 0, "domains to enqueue (default: config seed.ct.entryCount)")
	seedCrtshCmd.Flags().StringVar(&seedCrtshMatch, "match", "", "narrow to certificate identities mentioning this keyword")

	seedTrancoCmd.Flags().IntVar(&seedTrancoTop, "top", 0, "top domains to enqueue (default: config seed.ct.entryCount)")
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

	// --log accepts either an explicit log URL or an operator token; config
	// seed.ct.logs supplies the default operator filter.
	operators := cfg.Seed.CT.Logs
	var ctSeeder *seed.CTSeeder
	var label string
	if strings.Contains(seedCTLog, "://") {
		ctSeeder = seed.NewCTSeeder(seedCTLog)
		label = fmt.Sprintf("%d entries from %s", count, seedCTLog)
	} else {
		if seedCTLog != "" {
			operators = strings.Split(seedCTLog, ",")
		}
		ctSeeder, err = seed.NewCTSeederFromLogList(cmd.Context(), nil, cfg.Seed.CT.LogListURL, operators, time.Now())
		if err != nil {
			return fmt.Errorf("seed ct: %w", err)
		}
		label = fmt.Sprintf("%d entries from %s log(s)", count, strings.Join(operators, ","))
	}

	return runSeeder(cmd, cfg, ctSeeder, count, label)
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
