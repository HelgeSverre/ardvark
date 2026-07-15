// Package crawler is the ardvark crawl engine: a bounded worker pool that
// drains the persistent internal/frontier queue, dispatching each item to a
// handler by kind (page_fetch, host_probe, catalog_fetch, artifact_fetch,
// registry_harvest) per the design doc's "Work item types" section, writing
// results to internal/store and every significant event to an
// internal/eventlog logger.
//
// Two recursion sources are bounded by config and dedup keys: nested
// catalogs (ard.maxCatalogDepth, reusing FrontierItem.Depth with
// catalog_fetch-specific meaning) and registry referrals
// (registry.maxReferralDepth, same mechanism). Provenance a handler needs
// to attribute its result (parent catalog, declaring entry, probe method,
// ...) is persisted directly on the frontier_items row by the enqueuing
// side and read back by the dequeuing side's handler — see the
// "Provenance columns" doc comment on store.FrontierItem — so it survives
// process restarts and is visible to whichever worker process ends up
// dequeuing the item, not just the one that enqueued it.
package crawler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/store"
)

// defaultMaxAttempts is the retry cap for transient failures when
// Options.MaxAttempts is unset, per the design doc's error-handling
// section.
const defaultMaxAttempts = 3

// defaultConcurrency is the worker pool size when crawler.concurrency is
// unset.
const defaultConcurrency = 8

// defaultRegistryTimeout is the registry HTTP client timeout when
// crawler.requestTimeoutSeconds is unset.
const defaultRegistryTimeout = 15 * time.Second

// idlePollInterval is how often Run's dispatcher re-checks the frontier
// while the queue is empty but items are still in flight (an in-flight item
// may enqueue new work, so the crawl cannot terminate yet).
const idlePollInterval = 25 * time.Millisecond

// globalCountsCheckInterval throttles how often Run queries
// Frontier.Counts() while locally idle. Counts is a real query (two, on
// most drivers), so it is checked at a coarser cadence than
// idlePollInterval rather than on every poll tick.
const globalCountsCheckInterval = time.Second

// expiredLeaseReclaimInterval is how often Run's dispatcher sweeps for
// expired in_flight leases on mysql/postgres, where multiple worker
// processes may share the frontier and a blanket startup reclaim (safe only
// under sqlite's single-process assumption) would be wrong: a peer's
// legitimately in-flight item must not be reclaimed out from under it.
const expiredLeaseReclaimInterval = 30 * time.Second

// ARD entry media types the engine treats specially (see
// processCatalog).
const (
	mediaTypeAICatalog  = "application/ai-catalog+json"
	mediaTypeAIRegistry = "application/ai-registry+json"
)

// Options configures an Engine run.
type Options struct {
	// RunID associates enqueued frontier items and crawl_runs bookkeeping
	// with a specific store.CrawlRun. Zero is a valid "no run tracking"
	// value.
	RunID uint
	// Force bypasses the host_probe freshness check
	// (domains.last_probed_at within crawler.refreshAfterHours).
	Force bool
	// MaxAttempts overrides the retry cap for transient failures. Defaults
	// to defaultMaxAttempts (3) when zero.
	MaxAttempts int
	// BackoffBase is the base duration for exponential backoff between
	// retries of transient failures (doubled per attempt, capped at 30s).
	// Zero disables the sleep entirely, which is useful in tests.
	BackoffBase time.Duration
	// OnProbe, if non-nil, is invoked with a ProbeEvent for each per-host
	// crawl result as it happens: a miss or error from a host probe, and a
	// fetched catalog once it has been verified. It is called from worker
	// goroutines, so the callback must be goroutine-safe. Nil disables
	// progress reporting; the event log is unaffected either way.
	OnProbe func(ProbeEvent)
}

// Engine drains the frontier with a bounded worker pool, dispatching each
// item to its kind-specific handler.
type Engine struct {
	cfg      config.Config
	store    *store.Store
	frontier *frontier.Frontier
	fetch    *fetch.Client
	logger   *slog.Logger
	opts     Options

	httpClientOnce sync.Once
	httpClient     *http.Client
}

// New builds an Engine. logger must not be nil; pass eventlog.New's result
// (or slog.Default() in tests).
func New(cfg config.Config, st *store.Store, fr *frontier.Frontier, fetchClient *fetch.Client, logger *slog.Logger, opts Options) *Engine {
	return &Engine{
		cfg:      cfg,
		store:    st,
		frontier: fr,
		fetch:    fetchClient,
		logger:   logger,
		opts:     opts,
	}
}

// EnqueueSeedURL enqueues a page_fetch item at depth 0 for rawURL, the
// entry point for domain harvesting from a single seed URL.
func (e *Engine) EnqueueSeedURL(rawURL string) (bool, error) {
	host, err := hostOf(rawURL)
	if err != nil {
		return false, fmt.Errorf("crawler: enqueue seed url: %w", err)
	}
	return e.enqueue(store.KindPageFetch, rawURL, host, 0, provenance{})
}

// EnqueueSeedHost enqueues a host_probe item at depth 0 for host, the entry
// point for a bare-domain seed (or CT log seeding).
func (e *Engine) EnqueueSeedHost(host, discoverySource string) (bool, error) {
	if _, err := e.store.UpsertDomain(host, discoverySource); err != nil {
		return false, fmt.Errorf("crawler: enqueue seed host: %w", err)
	}
	return e.enqueue(store.KindHostProbe, "", host, 0, provenance{})
}

// provenance carries the frontier_items provenance columns (see
// store.FrontierItem's "Provenance columns" doc comment) that a handler
// will need once the item it is attached to is dequeued. Callers fill in
// only the fields relevant to the item's kind; the rest are left zero.
type provenance struct {
	ParentCatalogID   *uint
	ArtifactEntryID   *uint
	RegistryEntryID   *uint
	RegistryCatalogID *uint
	RegistryRowID     *uint
	ProbeMethod       string
}

// enqueue builds and enqueues a frontier item for kind, deriving the dedup
// key from the item's natural key (host for host_probe, URL otherwise) in
// one place so keys cannot drift across call sites, and stamping prov onto
// the row so a handler can read it back after this item is dequeued
// (possibly by a different worker process). An enqueue failure is
// warn-logged here — the crawl always continues without the item — and
// also returned for the callers that must react to it (seed enqueues,
// page-budget release).
func (e *Engine) enqueue(kind, url, host string, depth int, prov provenance) (bool, error) {
	natural := url
	if kind == store.KindHostProbe {
		natural = host
	}
	added, err := e.frontier.Enqueue(&store.FrontierItem{
		RunID:             e.opts.RunID,
		Kind:              kind,
		URL:               url,
		Host:              host,
		Depth:             depth,
		DedupKey:          dedupKey(kind, natural),
		ParentCatalogID:   prov.ParentCatalogID,
		ArtifactEntryID:   prov.ArtifactEntryID,
		RegistryEntryID:   prov.RegistryEntryID,
		RegistryCatalogID: prov.RegistryCatalogID,
		RegistryRowID:     prov.RegistryRowID,
		ProbeMethod:       prov.ProbeMethod,
	})
	if err != nil {
		e.logger.Warn("crawler: failed to enqueue item", "kind", kind, "url", url, "host", host, "error", err)
		return false, err
	}
	return added, nil
}

// Run drains the frontier with a continuous worker pool: concurrency()
// long-lived workers consume items from a channel fed by a dispatcher loop
// that repeatedly dequeues pending items (frontier.Dequeue marks them
// in_flight atomically, so the single dispatcher is the only frontier
// reader and workers never race for items). A slow item therefore occupies
// one worker, not the whole pool. Run returns nil once the frontier is
// globally empty — this worker's dequeue comes back empty, this worker has
// no items in flight, AND Frontier.Counts() reports zero pending and zero
// in_flight — or when ctx is cancelled (graceful shutdown: items already
// dequeued are still dispatched, and process requeues them once it sees
// ctx.Err).
//
// The global check matters for distributed crawling (mysql/postgres,
// multiple worker processes sharing one frontier): this process's own
// queue being empty says nothing about whether a peer process is still
// working on (or holding pending) items that could enqueue more work, so
// "locally idle" alone is never sufficient to terminate.
func (e *Engine) Run(ctx context.Context) error {
	sqliteBackend := isSQLiteDriver(e.cfg.Storage.Driver)

	if sqliteBackend {
		// sqlite's storage backend supports exactly one crawl process at a
		// time (see store.Open), so any in_flight row at startup is always
		// the residue of a previous process killed mid-batch: reclaim all
		// of them unconditionally.
		if n, err := e.frontier.ReclaimInFlight(); err != nil {
			return fmt.Errorf("crawler: reclaim in-flight: %w", err)
		} else if n > 0 {
			e.logger.Info("crawler: reclaimed stale in-flight items", "count", n)
		}
	} else {
		// mysql/postgres may have other worker processes concurrently
		// holding legitimate in_flight items, so only leases that have
		// actually expired are reclaimed (see ReclaimExpired). Run once at
		// startup in addition to the periodic sweep in the dispatcher loop
		// below, so a crash-and-restart doesn't wait a full
		// expiredLeaseReclaimInterval before resuming stranded work.
		if n, err := e.frontier.ReclaimExpired(); err != nil {
			return fmt.Errorf("crawler: reclaim expired: %w", err)
		} else if n > 0 {
			e.logger.Info("crawler: reclaimed expired in-flight items", "count", n)
		}
	}

	workers := e.concurrency()
	items := make(chan store.FrontierItem)
	var inFlight atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range items {
				e.process(ctx, item)
				inFlight.Add(-1)
			}
		}()
	}

	var runErr error
	lastReclaim := time.Now()
	var lastCountsCheck time.Time
	for ctx.Err() == nil {
		if !sqliteBackend && time.Since(lastReclaim) >= expiredLeaseReclaimInterval {
			if n, err := e.frontier.ReclaimExpired(); err != nil {
				e.logger.Error("crawler: periodic reclaim expired failed", "error", err)
			} else if n > 0 {
				e.logger.Info("crawler: reclaimed expired in-flight items", "count", n)
			}
			lastReclaim = time.Now()
		}

		// Snapshot idleness before dequeuing: if nothing was in flight then
		// and the dequeue still comes back empty, no worker goroutine of
		// *this* process can have enqueued new work in between (workers
		// commit all enqueues before their in-flight count drops).
		idle := inFlight.Load() == 0
		batch, err := e.frontier.Dequeue(workers)
		if err != nil {
			runErr = fmt.Errorf("crawler: dequeue: %w", err)
			break
		}
		if len(batch) == 0 {
			if idle {
				// Locally idle, but other worker processes may still hold
				// pending or in_flight work that could enqueue more.
				// Counts() is a real query, so it is only checked at
				// globalCountsCheckInterval granularity rather than on
				// every idlePollInterval tick.
				if time.Since(lastCountsCheck) >= globalCountsCheckInterval {
					pending, globalInFlight, cerr := e.frontier.Counts()
					lastCountsCheck = time.Now()
					if cerr != nil {
						runErr = fmt.Errorf("crawler: counts: %w", cerr)
						break
					}
					if pending == 0 && globalInFlight == 0 {
						break
					}
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(idlePollInterval):
			}
			continue
		}
		inFlight.Add(int64(len(batch)))
		for _, item := range batch {
			items <- item
		}
	}
	close(items)
	wg.Wait()
	return runErr
}

// isSQLiteDriver reports whether driver names ardvark's sqlite storage
// backend ("sqlite" or "sqlite3"), including the config.Defaults() default
// of an empty driver string being treated as sqlite by store.Open.
func isSQLiteDriver(driver string) bool {
	return driver == "" || driver == "sqlite" || driver == "sqlite3"
}

// concurrency returns the configured worker pool size.
func (e *Engine) concurrency() int {
	if e.cfg.Crawler.Concurrency > 0 {
		return e.cfg.Crawler.Concurrency
	}
	return defaultConcurrency
}

// maxAttempts returns the retry cap for transient failures.
func (e *Engine) maxAttempts() int {
	if e.opts.MaxAttempts > 0 {
		return e.opts.MaxAttempts
	}
	return defaultMaxAttempts
}

// ProcessItem processes a single already-dequeued frontier item exactly as
// Run's worker pool would (dispatch, then Complete/Fail bookkeeping and
// event logging). Exported for tests that need fine-grained control over
// frontier draining, e.g. simulating a crawl that stops partway through to
// exercise resumability.
func (e *Engine) ProcessItem(ctx context.Context, item store.FrontierItem) {
	e.process(ctx, item)
}

// process handles one dequeued frontier item end to end: dispatch, then
// Complete or Fail the item and log the outcome. It never returns an error
// itself — all failure handling is done via the frontier and event log, so
// one bad item can never crash the worker pool.
func (e *Engine) process(ctx context.Context, item store.FrontierItem) {
	err := e.handleItem(ctx, item)
	if err == nil {
		if cerr := e.frontier.Complete(item.ID); cerr != nil {
			e.logger.Error("crawler: failed to mark item complete", "item_id", item.ID, "kind", item.Kind, "error", cerr)
			return
		}
		e.logger.Info("crawler: item complete", "item_id", item.ID, "kind", item.Kind, "url", item.URL, "host", item.Host)
		return
	}

	// Interrupted by shutdown, not a real failure: return the item to pending
	// so a resumed run retries it, rather than burning an attempt or marking
	// it permanently failed and silently losing the work.
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		if rerr := e.frontier.Requeue(item.ID); rerr != nil {
			e.logger.Error("crawler: failed to requeue interrupted item", "item_id", item.ID, "error", rerr)
		}
		return
	}

	transient := fetch.Transient(err)
	max := 1
	if transient {
		max = e.maxAttempts()
		if e.opts.BackoffBase > 0 {
			time.Sleep(backoffDuration(item.Attempts+1, e.opts.BackoffBase))
		}
	}

	if _, ferr := e.frontier.Fail(item.ID, err, max); ferr != nil {
		e.logger.Error("crawler: failed to record item failure", "item_id", item.ID, "kind", item.Kind, "error", ferr)
		return
	}
	e.logger.Warn("crawler: item failed", "item_id", item.ID, "kind", item.Kind, "url", item.URL, "host", item.Host, "transient", transient, "error", err.Error())
}

// backoffDuration computes exponential backoff for the given 1-based
// attempt number, doubling per attempt and capping at 30s.
func backoffDuration(attempt int, base time.Duration) time.Duration {
	if base <= 0 || attempt < 1 {
		return 0
	}
	const capDuration = 30 * time.Second
	d := base
	for i := 1; i < attempt && d < capDuration; i++ {
		d *= 2
	}
	if d > capDuration {
		d = capDuration
	}
	return d
}

// handleItem dispatches item to its kind-specific handler.
func (e *Engine) handleItem(ctx context.Context, item store.FrontierItem) error {
	switch item.Kind {
	case store.KindPageFetch:
		return e.handlePageFetch(ctx, item)
	case store.KindHostProbe:
		return e.handleHostProbe(ctx, item)
	case store.KindCatalogFetch:
		return e.handleCatalogFetch(ctx, item)
	case store.KindArtifactFetch:
		return e.handleArtifactFetch(ctx, item)
	case store.KindRegistryHarvest:
		return e.handleRegistryHarvest(ctx, item)
	default:
		return fmt.Errorf("crawler: unknown frontier item kind %q", item.Kind)
	}
}

// get performs a client.Get, translating a robots.txt disallow into a
// "gracefully skip, don't retry" signal rather than an error, since being
// disallowed is an expected, non-exceptional outcome.
func (e *Engine) get(ctx context.Context, rawURL string) (fetched *fetch.Fetched, skip bool, err error) {
	fetched, err = e.fetch.Get(ctx, rawURL)
	if err != nil {
		if errors.Is(err, fetch.ErrRobotsDisallowed) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return fetched, false, nil
}

// refreshWindow returns the configured freshness window for host_probe
// deduplication. Defaults are applied at config-load time (see
// maxPagesPerDomain's doc comment); an explicit 0 here means "always
// re-probe" rather than falling back to the documented 168h default.
func (e *Engine) refreshWindow() time.Duration {
	return time.Duration(e.cfg.Crawler.RefreshAfterHours) * time.Hour
}

// registryHTTPClient returns a lazily-built *http.Client for
// internal/registry, sized from the crawler's request timeout. The
// registry package talks to a different kind of endpoint (a JSON search
// API) than internal/fetch's page/document GETs, so it does not go through
// the politeness client.
func (e *Engine) registryHTTPClient() *http.Client {
	e.httpClientOnce.Do(func() {
		timeout := time.Duration(e.cfg.Crawler.RequestTimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = defaultRegistryTimeout
		}
		e.httpClient = &http.Client{Timeout: timeout}
	})
	return e.httpClient
}

// -- Page-count tracking for maxPagesPerDomain -----------------------------

// pageBudgetAvailable reports whether host still has room in its
// maxPagesPerDomain budget for one more page_fetch item, by counting
// existing page_fetch rows for host directly in the frontier
// (frontier.CountByHostKind) rather than an in-memory approximation. The
// dedup key guarantees at most one frontier_items row per distinct URL, so
// within a single process this count is exact.
//
// The check happens at enqueue time (not fetch time) so that a single
// page's link fan-out cannot enqueue far more page_fetch items than the
// budget allows before any of them have actually been fetched and counted.
// It is checked immediately before each enqueue call in the fan-out loop
// (handlePageFetch), so the count reflects every item enqueued earlier in
// the same loop.
//
// Under distributed crawling (mysql/postgres, multiple worker processes),
// this check is exact per-process but not globally atomic: two workers
// racing to enqueue page_fetch items for the same host could each observe
// a budget count just under the limit and both enqueue, overshooting by a
// small, bounded amount. Host-affinity sharding (store.FrontierItem.HostShard)
// means this race is rare in practice — normally only one worker ever owns
// a given host — but it is not eliminated for hosts discovered mid-crawl
// before sharding routes their future items consistently. This is a
// deliberate, documented tradeoff rather than a bug: closing it fully would
// require a cross-process lock or a DB-side atomic counter for a budget
// whose entire purpose is a soft cap, not an exact one.
func (e *Engine) pageBudgetAvailable(host string) bool {
	count, err := e.frontier.CountByHostKind(host, store.KindPageFetch)
	if err != nil {
		// Fail open: the budget is a politeness/scope guard, not a
		// correctness requirement, so a broken count must not block the
		// crawl — but it shouldn't be silent.
		e.logger.Warn("crawler: page budget count failed", "host", host, "error", err)
		return true
	}
	return count < int64(e.maxPagesPerDomain())
}

// maxPagesPerDomain returns the configured page budget per domain.
// config.Load/config.Defaults already fill in the documented default (50)
// for any key absent from the config file, so an explicit 0 here reflects
// the operator's own choice (permitted by the config schema's minimum:0)
// and must not be silently overridden.
func (e *Engine) maxPagesPerDomain() int {
	return e.cfg.Crawler.MaxPagesPerDomain
}

// maxDepth returns the configured page-crawl depth budget. See
// maxPagesPerDomain's doc comment: defaults are applied at config-load
// time, so an explicit 0 is honored here rather than replaced.
func (e *Engine) maxDepth() int {
	return e.cfg.Crawler.MaxDepth
}

// maxCatalogDepth returns the configured nested-catalog recursion budget.
// See maxPagesPerDomain's doc comment.
func (e *Engine) maxCatalogDepth() int {
	return e.cfg.ARD.MaxCatalogDepth
}

// maxReferralDepth returns the configured registry referral recursion
// budget. See maxPagesPerDomain's doc comment: defaults are applied at
// config-load time, so an explicit 0 is honored here rather than replaced.
func (e *Engine) maxReferralDepth() int {
	return e.cfg.Registry.MaxReferralDepth
}

// dedupKey builds a frontier dedup key from a kind and a natural key (URL
// or host).
func dedupKey(kind, natural string) string {
	return kind + ":" + natural
}
