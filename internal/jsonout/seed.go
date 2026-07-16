package jsonout

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/seed"
	"github.com/helgesverre/ardvark/internal/store"
)

// SeedResult is the outcome of one `ardvark seed <source>` run.
type SeedResult struct {
	Source  string `json:"source"`
	Label   string `json:"label"`
	Added   int    `json:"added"`
	Skipped int    `json:"skipped"`
}

// RunSeeder drives a seed.Seeder to completion: fetch up to n candidate
// domains, then upsert each as a domains row and enqueue a host_probe
// frontier item, tagged with the seeder's discovery_source. Shared by every
// `seed <source>` subcommand and the MCP ardvark_seed tool. The caller owns
// st and its Close.
func RunSeeder(ctx context.Context, cfg config.Config, st *store.Store, s seed.Seeder, n int, sourceLabel string) (SeedResult, error) {
	logger, err := NewLogger(cfg)
	if err != nil {
		return SeedResult{}, err
	}

	names, err := s.Domains(ctx, n)
	if err != nil {
		return SeedResult{}, fmt.Errorf("seed %s: %w", s.Source(), err)
	}

	fr := frontier.New(st.DB,
		frontier.WithLeaseSeconds(cfg.Crawler.LeaseSeconds),
		frontier.WithWorkerShard(cfg.Crawler.Worker.Index, cfg.Crawler.Worker.Count),
	)
	fc := fetch.New(cfg.Crawler)
	eng := crawler.New(cfg, st, fr, fc, logger, crawler.Options{})

	res := SeedResult{Source: s.Source(), Label: sourceLabel}
	for _, host := range names {
		ok, err := eng.EnqueueSeedHost(host, s.Source())
		if err != nil {
			res.Skipped++
			continue
		}
		if ok {
			res.Added++
		} else {
			res.Skipped++
		}
	}

	return res, nil
}

// SeedSources are the accepted `source` values for BuildSeeder, matching the
// CLI's `seed <source>` subcommands.
var SeedSources = []string{"ct", "crtsh", "tranco", "github", "mcp", "curated", "commoncrawl"}

// BuildSeeder constructs the seed.Seeder for a named source using the
// config's defaults, with count (and, for commoncrawl, offset) overriding
// the configured default when non-zero. It returns the seeder, the
// effective count, and a human-readable label describing the run — the
// programmatic counterpart of the CLI's per-source flag handling, used by
// the MCP ardvark_seed tool.
func BuildSeeder(ctx context.Context, cfg config.Config, source string, count, offset int) (s seed.Seeder, n int, label string, err error) {
	switch source {
	case "ct":
		n = orDefault(count, cfg.Seed.CT.EntryCount)
		s, err = seed.NewCTSeederFromLogList(ctx, nil, cfg.Seed.CT.LogListURL, cfg.Seed.CT.Logs, time.Now())
		if err != nil {
			return nil, 0, "", fmt.Errorf("seed ct: %w", err)
		}
		label = fmt.Sprintf("%d entries from %s log(s)", n, strings.Join(cfg.Seed.CT.Logs, ","))
	case "crtsh":
		n = orDefault(count, cfg.Seed.Crtsh.Count)
		s = &seed.CrtshSeeder{Endpoint: cfg.Seed.Crtsh.Endpoint, Matches: seed.DefaultCrtshMatches}
		label = fmt.Sprintf("%d domains from %s (default keywords=%v)", n, cfg.Seed.Crtsh.Endpoint, seed.DefaultCrtshMatches)
	case "tranco":
		n = orDefault(count, cfg.Seed.Tranco.Top)
		s = seed.NewTrancoSeeder(cfg.Seed.Tranco.ListURL)
		label = fmt.Sprintf("top %d domains from %s", n, cfg.Seed.Tranco.ListURL)
	case "github":
		n = orDefault(count, cfg.Seed.GitHub.Count)
		s = seed.NewGitHubSeeder(cfg.Seed.GitHub.Query)
		label = fmt.Sprintf("%d domains matching %q on GitHub", n, cfg.Seed.GitHub.Query)
	case "mcp":
		n = orDefault(count, cfg.Seed.MCPRegistry.Count)
		s = seed.NewMCPRegistrySeeder(cfg.Seed.MCPRegistry.RegistryURL)
		label = fmt.Sprintf("%d domains from MCP registry %s", n, cfg.Seed.MCPRegistry.RegistryURL)
	case "curated":
		n = orDefault(count, cfg.Seed.Curated.Count)
		s = seed.NewCuratedSeeder(cfg.Seed.Curated.URLs)
		label = fmt.Sprintf("%d domains from %d curated list(s)", n, len(cfg.Seed.Curated.URLs))
	case "commoncrawl":
		n = orDefault(count, cfg.Seed.CommonCrawl.Top)
		off := orDefault(offset, cfg.Seed.CommonCrawl.Offset)
		s = seed.NewCommonCrawlSeeder(cfg.Seed.CommonCrawl.GraphInfoURL, cfg.Seed.CommonCrawl.Graph, off)
		graphLabel := cfg.Seed.CommonCrawl.Graph
		if graphLabel == "" {
			graphLabel = "latest"
		}
		label = fmt.Sprintf("top %d domains from Common Crawl graph %s", n, graphLabel)
		if off > 0 {
			label = fmt.Sprintf("%s (offset %d)", label, off)
		}
	default:
		return nil, 0, "", fmt.Errorf("seed: unknown source %q (want one of %s)", source, strings.Join(SeedSources, ", "))
	}
	return s, n, label, nil
}
