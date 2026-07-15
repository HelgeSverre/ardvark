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
// (registry.maxReferralDepth, same mechanism). Because FrontierItem does
// not carry a parent-catalog or declaring-entry foreign key, the engine
// tracks that provenance in an in-memory map keyed by URL, populated at
// enqueue time. This is a known limitation: if the process restarts with
// items still pending in the frontier, resumed catalog_fetch /
// registry_harvest items whose provenance was only in memory will fall
// back to sensible defaults (no parent, catalog id 0) rather than losing
// data outright. A future iteration could add explicit foreign-key columns
// to the frontier_items table to make this fully resumable.
package crawler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
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

	provMu             sync.Mutex
	parentCatalogByURL map[string]uint
	artifactEntryByURL map[string]uint
	registryCtxByURL   map[string]registryContext
	// catalogMethodByURL remembers which probe method (well_known,
	// robots_agentmap, link_tag) discovered each enqueued catalog URL, so
	// the verified-catalog ProbeEvent can report it. Same in-memory
	// provenance caveat as the maps above.
	catalogMethodByURL map[string]string

	// pagesMu guards pageCounts, an in-memory approximation of "pages
	// fetched so far per domain" used to enforce maxPagesPerDomain without
	// an extra store query per link.
	pagesMu    sync.Mutex
	pageCounts map[string]int
}

// registryContext carries the provenance an in-flight registry_harvest
// item needs: which catalog entry declared the registry, which catalog its
// harvested entries should be attributed to, and the registries-table row
// id for the registry itself.
type registryContext struct {
	entryID   uint
	catalogID uint
	regRowID  uint
}

// New builds an Engine. logger must not be nil; pass eventlog.New's result
// (or slog.Default() in tests).
func New(cfg config.Config, st *store.Store, fr *frontier.Frontier, fetchClient *fetch.Client, logger *slog.Logger, opts Options) *Engine {
	return &Engine{
		cfg:                cfg,
		store:              st,
		frontier:           fr,
		fetch:              fetchClient,
		logger:             logger,
		opts:               opts,
		parentCatalogByURL: make(map[string]uint),
		artifactEntryByURL: make(map[string]uint),
		registryCtxByURL:   make(map[string]registryContext),
		catalogMethodByURL: make(map[string]string),
		pageCounts:         make(map[string]int),
	}
}

// EnqueueSeedURL enqueues a page_fetch item at depth 0 for rawURL, the
// entry point for domain harvesting from a single seed URL.
func (e *Engine) EnqueueSeedURL(rawURL string) (bool, error) {
	host, err := hostOf(rawURL)
	if err != nil {
		return false, fmt.Errorf("crawler: enqueue seed url: %w", err)
	}
	return e.frontier.Enqueue(&store.FrontierItem{
		RunID:    e.opts.RunID,
		Kind:     store.KindPageFetch,
		URL:      rawURL,
		Host:     host,
		Depth:    0,
		DedupKey: dedupKey(store.KindPageFetch, rawURL),
	})
}

// EnqueueSeedHost enqueues a host_probe item at depth 0 for host, the entry
// point for a bare-domain seed (or CT log seeding).
func (e *Engine) EnqueueSeedHost(host, discoverySource string) (bool, error) {
	if _, err := e.store.UpsertDomain(host, discoverySource); err != nil {
		return false, fmt.Errorf("crawler: enqueue seed host: %w", err)
	}
	return e.frontier.Enqueue(&store.FrontierItem{
		RunID:    e.opts.RunID,
		Kind:     store.KindHostProbe,
		Host:     host,
		Depth:    0,
		DedupKey: dedupKey(store.KindHostProbe, host),
	})
}

// Run drains the frontier: it repeatedly dequeues up to
// crawler.concurrency pending items, processes them concurrently, and
// waits for the batch to finish before dequeuing again (so items newly
// enqueued mid-batch, e.g. page links discovered by a page_fetch, are
// picked up on the next iteration). Run returns nil once the frontier is
// empty or ctx is cancelled (graceful shutdown: no new batch is started,
// but the current in-flight batch is allowed to finish).
func (e *Engine) Run(ctx context.Context) error {
	// Reclaim items left in_flight by a previously killed process (ardvark
	// runs one crawl at a time, so any in_flight row at startup is stale).
	if n, err := e.frontier.ReclaimInFlight(); err != nil {
		return fmt.Errorf("crawler: reclaim in-flight: %w", err)
	} else if n > 0 {
		e.logger.Info("crawler: reclaimed stale in-flight items", "count", n)
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		items, err := e.frontier.Dequeue(e.concurrency())
		if err != nil {
			return fmt.Errorf("crawler: dequeue: %w", err)
		}
		if len(items) == 0 {
			return nil
		}

		var wg sync.WaitGroup
		for _, item := range items {
			item := item
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.process(ctx, item)
			}()
		}
		wg.Wait()
	}
}

// concurrency returns the configured worker pool size, defaulting to 8.
func (e *Engine) concurrency() int {
	if e.cfg.Crawler.Concurrency > 0 {
		return e.cfg.Crawler.Concurrency
	}
	return 8
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

	if ferr := e.frontier.Fail(item.ID, err, max); ferr != nil {
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
			timeout = 15 * time.Second
		}
		e.httpClient = &http.Client{Timeout: timeout}
	})
	return e.httpClient
}

// -- Provenance tracking (see package doc for the resumability caveat) ----

func (e *Engine) setParentCatalog(url string, catalogID uint) {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	e.parentCatalogByURL[url] = catalogID
}

func (e *Engine) parentCatalogFor(url string) *uint {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	if id, ok := e.parentCatalogByURL[url]; ok {
		return &id
	}
	return nil
}

func (e *Engine) setArtifactEntry(url string, entryID uint) {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	e.artifactEntryByURL[url] = entryID
}

func (e *Engine) artifactEntry(url string) uint {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	return e.artifactEntryByURL[url]
}

func (e *Engine) setCatalogMethod(url, method string) {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	e.catalogMethodByURL[url] = method
}

func (e *Engine) catalogMethodFor(url string) string {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	return e.catalogMethodByURL[url]
}

func (e *Engine) setRegistryContext(url string, entryID, catalogID, regRowID uint) {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	e.registryCtxByURL[url] = registryContext{entryID: entryID, catalogID: catalogID, regRowID: regRowID}
}

func (e *Engine) registryContextFor(url string) (registryContext, bool) {
	e.provMu.Lock()
	defer e.provMu.Unlock()
	ctx, ok := e.registryCtxByURL[url]
	return ctx, ok
}

// -- Page-count tracking for maxPagesPerDomain -----------------------------

// reservePage atomically checks-and-reserves one unit of host's
// maxPagesPerDomain budget, returning false without reserving if the budget
// is already exhausted. Reservation happens at enqueue time (not fetch
// time) so that a single page fan-out cannot enqueue far more page_fetch
// items than the budget allows before any of them have actually been
// fetched and counted.
func (e *Engine) reservePage(host string, budget int) bool {
	e.pagesMu.Lock()
	defer e.pagesMu.Unlock()
	if e.pageCounts[host] >= budget {
		return false
	}
	e.pageCounts[host]++
	return true
}

// releasePage undoes a reservePage call, used when an enqueue attempt made
// after a successful reservation ultimately fails (so the budget isn't
// permanently consumed by dead reservations).
func (e *Engine) releasePage(host string) {
	e.pagesMu.Lock()
	defer e.pagesMu.Unlock()
	if e.pageCounts[host] > 0 {
		e.pageCounts[host]--
	}
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
