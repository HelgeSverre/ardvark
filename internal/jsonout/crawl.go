package jsonout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/store"
)

// CrawlResult is the final summary of one crawl run.
type CrawlResult struct {
	SeedsRequested int `json:"seeds_requested"`
	Seeded         int `json:"seeded"`
	PagesFetched   int `json:"pages_fetched"`
	HostsProbed    int `json:"hosts_probed"`
	CatalogsFound  int `json:"catalogs_found"`
	CatalogsValid  int `json:"catalogs_valid"`
	Errors         int `json:"errors"`
}

// CrawlCallbacks surfaces live crawl progress to the CLI. Any field may be
// nil.
type CrawlCallbacks struct {
	// OnProbe receives live per-host crawl results from the engine's worker
	// goroutines; it must be goroutine-safe.
	OnProbe func(crawler.ProbeEvent)
	// SeedError is invoked when a seed fails to enqueue (the crawl
	// continues with the remaining seeds).
	SeedError func(seed string, err error)
	// Seeded is invoked once, after seeding and before the crawl runs,
	// with how many of the requested seeds were newly enqueued.
	Seeded func(seeded, requested int)
}

// Crawl seeds the persistent frontier from the given URLs and/or bare
// domains, then runs the crawler until the frontier is empty (pending work
// from prior runs is resumed automatically) and returns the run summary.
func Crawl(ctx context.Context, cfg config.Config, st *store.Store, seeds []string, force bool, cb CrawlCallbacks) (CrawlResult, error) {
	logger, err := NewLogger(cfg)
	if err != nil {
		return CrawlResult{}, err
	}

	configSnapshot, err := json.Marshal(cfg)
	if err != nil {
		return CrawlResult{}, fmt.Errorf("crawl: marshalling config snapshot: %w", err)
	}

	run, err := st.CreateRun(string(configSnapshot))
	if err != nil {
		return CrawlResult{}, err
	}

	fr := frontier.New(st.DB, frontier.WithLeaseSeconds(cfg.Crawler.LeaseSeconds))
	fc := fetch.New(cfg.Crawler)
	eng := crawler.New(cfg, st, fr, fc, logger, crawler.Options{
		RunID:       run.ID,
		Force:       force,
		BackoffBase: time.Second,
		OnProbe:     cb.OnProbe,
	})

	seeded := 0
	for _, s := range seeds {
		added, err := SeedOne(eng, s)
		if err != nil {
			if cb.SeedError != nil {
				cb.SeedError(s, err)
			}
			continue
		}
		if added {
			seeded++
		}
	}
	if cb.Seeded != nil {
		cb.Seeded(seeded, len(seeds))
	}

	if err := eng.Run(ctx); err != nil {
		return CrawlResult{}, err
	}

	pagesFetched, hostsProbed, catalogsFound, catalogsValid, errCount, err := SummarizeRun(st, run.StartedAt)
	if err != nil {
		return CrawlResult{}, err
	}
	if err := st.FinishRun(run.ID, pagesFetched, hostsProbed, catalogsFound, catalogsValid, errCount); err != nil {
		return CrawlResult{}, err
	}

	return CrawlResult{
		SeedsRequested: len(seeds),
		Seeded:         seeded,
		PagesFetched:   pagesFetched,
		HostsProbed:    hostsProbed,
		CatalogsFound:  catalogsFound,
		CatalogsValid:  catalogsValid,
		Errors:         errCount,
	}, nil
}

// SeedOne enqueues a single seed. A bare domain is seeded as a host_probe. A
// URL is seeded as a page_fetch and, additionally, as a host_probe of its
// origin host — so a seed URL whose page 404s or has no anchors (an SPA, an
// API root) still gets its well-known catalog checked. Deduping makes the
// extra host_probe harmless when the page crawl reaches the same host.
func SeedOne(eng *crawler.Engine, seed string) (bool, error) {
	if !strings.Contains(seed, "://") {
		return eng.EnqueueSeedHost(seed, store.DiscoverySourceSeed)
	}

	added, err := eng.EnqueueSeedURL(seed)
	if err != nil {
		return added, err
	}
	if u, perr := url.Parse(seed); perr == nil && u.Hostname() != "" {
		if _, herr := eng.EnqueueSeedHost(u.Hostname(), store.DiscoverySourceSeed); herr != nil {
			return added, herr
		}
	}
	return added, nil
}

// SummarizeRun computes crawl_run summary counters for FinishRun. Pages
// fetched and hosts probed are counted from completed frontier items
// belonging to the run; catalogs found/valid are counted from catalog rows
// fetched since the run started (catalogs have no run_id column, so a time
// window is used — accurate for a single crawl run, an approximation if
// runs overlap). Errors are frontier items that exhausted their retry
// budget.
func SummarizeRun(st *store.Store, startedAt time.Time) (pagesFetched, hostsProbed, catalogsFound, catalogsValid, errCount int, err error) {
	var n int64

	// Count frontier work by when it completed, not by run_id: items seeded
	// by a separate `seed` command (or a prior run) carry that run's id but
	// are drained by this crawl, so time-window attribution is what reflects
	// what this run actually did.
	if err = st.DB.Model(&store.FrontierItem{}).
		Where("kind = ? AND status = ? AND updated_at >= ?", store.KindPageFetch, store.FrontierStatusDone, startedAt).
		Count(&n).Error; err != nil {
		return
	}
	pagesFetched = int(n)

	if err = st.DB.Model(&store.FrontierItem{}).
		Where("kind = ? AND status = ? AND updated_at >= ?", store.KindHostProbe, store.FrontierStatusDone, startedAt).
		Count(&n).Error; err != nil {
		return
	}
	hostsProbed = int(n)

	if err = st.DB.Model(&store.Catalog{}).
		Where("fetched_at >= ?", startedAt).
		Count(&n).Error; err != nil {
		return
	}
	catalogsFound = int(n)

	if err = st.DB.Model(&store.Catalog{}).
		Where("fetched_at >= ? AND verification_status IN ?", startedAt, []string{
			store.VerificationStatusValid, store.VerificationStatusValidWithWarnings,
		}).
		Count(&n).Error; err != nil {
		return
	}
	catalogsValid = int(n)

	if err = st.DB.Model(&store.FrontierItem{}).
		Where("status = ? AND updated_at >= ?", store.FrontierStatusFailed, startedAt).
		Count(&n).Error; err != nil {
		return
	}
	errCount = int(n)

	return
}
