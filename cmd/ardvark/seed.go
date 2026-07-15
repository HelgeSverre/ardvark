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
	"github.com/helgesverre/ardvark/internal/store"
)

// orDefault returns flag when set (non-zero value), else def — the
// flag-over-config precedence every seed subcommand applies.
func orDefault[T comparable](flag, def T) T {
	var zero T
	if flag != zero {
		return flag
	}
	return def
}

var (
	seedCTCount int
	seedCTLog   string

	seedCrtshCount int
	seedCrtshMatch string

	seedTrancoTop int
	seedTrancoURL string

	seedGitHubCount int
	seedGitHubQuery string

	seedMCPCount       int
	seedMCPRegistryURL string
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
		"Use --match to narrow to certificates whose identity mentions a keyword (e.g. \"agent\", \"mcp\"); " +
		"without --match, a curated agent/mcp/ai keyword set is queried instead of an unfiltered wildcard, " +
		"which crt.sh cannot serve.",
	RunE: runSeedCrtsh,
}

var seedTrancoCmd = &cobra.Command{
	Use:   "tranco",
	Short: "Seed from the Tranco top-domains list",
	Long: "Queue the top N domains from the Tranco list for probing. Covers the established web " +
		"that CT-log seeding, which only sees freshly issued certificates, misses.",
	RunE: runSeedTranco,
}

var seedGitHubCmd = &cobra.Command{
	Use:   "github",
	Short: "Seed from GitHub code search",
	Long: "Search GitHub's code search API for well-known ARD catalog files (default query: " +
		"filename:ai-catalog.json path:.well-known) and queue the owning repositories' domains for probing. " +
		"The highest-precision seed source available, since a hit is a real deployed catalog, not a keyword " +
		"coincidence. Requires a GITHUB_TOKEN environment variable.",
	RunE: runSeedGitHub,
}

var seedMCPCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Seed from the MCP server registry",
	Long: "Harvest domains from the official MCP (Model Context Protocol) server registry: each listed " +
		"server's remote endpoint host, plus a domain decoded from its reverse-DNS-style name. Highest-" +
		"propensity ARD adopters, since MCP server operators are exactly the audience ARD targets.",
	RunE: runSeedMCP,
}

func init() {
	seedCTCmd.Flags().IntVar(&seedCTCount, "count", 0, "entries to harvest (default: config seed.ct.entryCount)")
	seedCTCmd.Flags().StringVar(&seedCTLog, "log", "", "operator token (oak, argon, all) or explicit log URL (default: config seed.ct.logs)")

	seedCrtshCmd.Flags().IntVar(&seedCrtshCount, "count", 0, "domains to enqueue (default: config seed.crtsh.count)")
	seedCrtshCmd.Flags().StringVar(&seedCrtshMatch, "match", "", "narrow to certificate identities mentioning this keyword (default: a curated agent/mcp/ai keyword set)")

	seedTrancoCmd.Flags().IntVar(&seedTrancoTop, "top", 0, "top domains to enqueue (default: config seed.tranco.top)")
	seedTrancoCmd.Flags().StringVar(&seedTrancoURL, "url", "", "Tranco list URL (default: config seed.tranco.listUrl)")

	seedGitHubCmd.Flags().IntVar(&seedGitHubCount, "count", 0, "domains to enqueue (default: config seed.github.count)")
	seedGitHubCmd.Flags().StringVar(&seedGitHubQuery, "query", "", "GitHub code-search query (default: config seed.github.query)")

	seedMCPCmd.Flags().IntVar(&seedMCPCount, "count", 0, "domains to enqueue (default: config seed.mcp.count)")
	seedMCPCmd.Flags().StringVar(&seedMCPRegistryURL, "registry", "", "MCP registry base URL (default: config seed.mcp.registryUrl)")

	seedCmd.AddCommand(seedCTCmd)
	seedCmd.AddCommand(seedCrtshCmd)
	seedCmd.AddCommand(seedTrancoCmd)
	seedCmd.AddCommand(seedGitHubCmd)
	seedCmd.AddCommand(seedMCPCmd)
	rootCmd.AddCommand(seedCmd)
}

func runSeedCT(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	count := orDefault(seedCTCount, cfg.Seed.CT.EntryCount)

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

	return runSeeder(cmd, cfg, st, ctSeeder, count, label)
}

func runSeedCrtsh(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	count := orDefault(seedCrtshCount, cfg.Seed.Crtsh.Count)

	crtshSeeder := &seed.CrtshSeeder{Endpoint: cfg.Seed.Crtsh.Endpoint}

	label := fmt.Sprintf("%d domains from %s", count, crtshSeeder.Endpoint)
	if seedCrtshMatch != "" {
		crtshSeeder.Match = seedCrtshMatch
		label = fmt.Sprintf("%s (match=%q)", label, seedCrtshMatch)
	} else {
		// A bare "q=%" wildcard is not something crt.sh can serve; fall
		// back to a curated agent/mcp/ai keyword set instead of forcing
		// every caller to pick a keyword themselves.
		crtshSeeder.Matches = seed.DefaultCrtshMatches
		label = fmt.Sprintf("%s (default keywords=%v)", label, seed.DefaultCrtshMatches)
	}

	return runSeeder(cmd, cfg, st, crtshSeeder, count, label)
}

func runSeedTranco(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	top := orDefault(seedTrancoTop, cfg.Seed.Tranco.Top)
	listURL := orDefault(seedTrancoURL, cfg.Seed.Tranco.ListURL)

	return runSeeder(cmd, cfg, st, seed.NewTrancoSeeder(listURL), top,
		fmt.Sprintf("top %d domains from %s", top, listURL))
}

func runSeedGitHub(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	count := orDefault(seedGitHubCount, cfg.Seed.GitHub.Count)
	query := orDefault(seedGitHubQuery, cfg.Seed.GitHub.Query)

	return runSeeder(cmd, cfg, st, seed.NewGitHubSeeder(query), count,
		fmt.Sprintf("%d domains matching %q on GitHub", count, query))
}

func runSeedMCP(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	count := orDefault(seedMCPCount, cfg.Seed.MCPRegistry.Count)
	registryURL := orDefault(seedMCPRegistryURL, cfg.Seed.MCPRegistry.RegistryURL)

	return runSeeder(cmd, cfg, st, seed.NewMCPRegistrySeeder(registryURL), count,
		fmt.Sprintf("%d domains from MCP registry %s", count, registryURL))
}

// runSeeder drives a seed.Seeder to completion: fetch up to n candidate
// domains, then upsert each as a domains row and enqueue a host_probe
// frontier item, tagged with the seeder's discovery_source. Shared by every
// `seed <source>` subcommand so each one only has to build its Seeder and
// resolve its flags/config. The caller owns st and its Close.
func runSeeder(cmd *cobra.Command, cfg config.Config, st *store.Store, s seed.Seeder, n int, sourceLabel string) error {
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
	p.Summary(fmt.Sprintf("seed %s complete", s.Source()),
		sourceLabel,
		fmt.Sprintf("%d added", added),
		fmt.Sprintf("%d skipped (already queued)", skipped),
	)
	return nil
}
