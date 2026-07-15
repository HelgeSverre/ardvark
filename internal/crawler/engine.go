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
	"container/list"
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

	parentCatalogByURL *lockedMap[string, uint]
	artifactEntryByURL *lockedMap[string, uint]
	registryCtxByURL   *lockedMap[string, registryContext]
	// catalogMethodByURL remembers which probe method (well_known,
	// robots_agentmap, link_tag) discovered each enqueued catalog URL, so
	// the verified-catalog ProbeEvent can report it. Same in-memory
	// provenance caveat as the maps above.
	catalogMethodByURL *lockedMap[string, string]

	// pagesMu guards pageCounts/pageOrder/pageElems, an in-memory
	// approximation of "pages fetched so far per domain" used to enforce
	// maxPagesPerDomain without an extra store query per link. Unlike the
	// provenance maps above, a host's entry cannot simply be dropped once
	// "consumed" — the budget must hold for the domain's entire life in
	// the crawl, and page_fetch items for a host can keep arriving as long
	// as its budget isn't exhausted. So instead of per-entry release this
	// is bounded with an LRU cap (maxTrackedDomains): once that many
	// distinct hosts are tracked, the least-recently-touched host is
	// evicted to make room. This is a deliberate, documented
	// approximation (see reservePage) — an evicted host that reappears
	// much later starts a fresh count, so it could exceed
	// maxPagesPerDomain by a bounded amount. Given the cap is large
	// relative to any single crawl's realistic per-run host cardinality,
	// eviction is rare in practice; it exists purely to bound memory for
	// very long-running crawls over huge seed sets (e.g. CT-log seeding).
	pagesMu    sync.Mutex
	pageCounts map[string]int
	pageOrder  *list.List
	pageElems  map[string]*list.Element
}

// maxTrackedDomains bounds the number of distinct hosts pageCounts (and its
// LRU bookkeeping) will track at once. See the pagesMu doc comment.
const maxTrackedDomains = 20000

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
		parentCatalogByURL: newLockedMap[string, uint](),
		artifactEntryByURL: newLockedMap[string, uint](),
		registryCtxByURL:   newLockedMap[string, registryContext](),
		catalogMethodByURL: newLockedMap[string, string](),
		pageCounts:         make(map[string]int),
		pageOrder:          list.New(),
		pageElems:          make(map[string]*list.Element),
	}
}

// EnqueueSeedURL enqueues a page_fetch item at depth 0 for rawURL, the
// entry point for domain harvesting from a single seed URL.
func (e *Engine) EnqueueSeedURL(rawURL string) (bool, error) {
	host, err := hostOf(rawURL)
	if err != nil {
		return false, fmt.Errorf("crawler: enqueue seed url: %w", err)
	}
	return e.enqueue(store.KindPageFetch, rawURL, host, 0)
}

// EnqueueSeedHost enqueues a host_probe item at depth 0 for host, the entry
// point for a bare-domain seed (or CT log seeding).
func (e *Engine) EnqueueSeedHost(host, discoverySource string) (bool, error) {
	if _, err := e.store.UpsertDomain(host, discoverySource); err != nil {
		return false, fmt.Errorf("crawler: enqueue seed host: %w", err)
	}
	return e.enqueue(store.KindHostProbe, "", host, 0)
}

// enqueue builds and enqueues a frontier item for kind, deriving the dedup
// key from the item's natural key (host for host_probe, URL otherwise) in
// one place so keys cannot drift across call sites. An enqueue failure is
// warn-logged here — the crawl always continues without the item — and
// also returned for the callers that must react to it (seed enqueues,
// page-budget release).
func (e *Engine) enqueue(kind, url, host string, depth int) (bool, error) {
	natural := url
	if kind == store.KindHostProbe {
		natural = host
	}
	added, err := e.frontier.Enqueue(&store.FrontierItem{
		RunID:    e.opts.RunID,
		Kind:     kind,
		URL:      url,
		Host:     host,
		Depth:    depth,
		DedupKey: dedupKey(kind, natural),
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
		// The item will never be dispatched again (barring a future
		// re-enqueue of the same dedup key, which always calls the
		// matching setXxx before re-enqueueing — see releaseProvenance's
		// doc comment), so its in-memory provenance entry can be dropped.
		e.releaseProvenance(item)
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

	permanent, ferr := e.frontier.Fail(item.ID, err, max)
	if ferr != nil {
		e.logger.Error("crawler: failed to record item failure", "item_id", item.ID, "kind", item.Kind, "error", ferr)
		return
	}
	// A still-pending item (more retries left) still needs its provenance
	// entry for the next attempt; a permanently failed one never will.
	if permanent {
		e.releaseProvenance(item)
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

// -- Provenance tracking (see package doc for the resumability caveat) ----
//
// Lifecycle: each entry is written by a setXxx call immediately before the
// frontier item that will consume it is enqueued, and read once by the
// item's handler while it is being processed. Once process() learns the
// item will never be dispatched again — it completed successfully, or it
// failed permanently (attempts exhausted) — releaseProvenance drops the
// entry so these maps do not grow for the lifetime of a long crawl,
// retaining only entries for work still pending or in flight (including
// items with retries remaining, which still need their entry for the next
// attempt).
//
// A reference cycle (catalog A -> B -> A, or registry referrals pointing
// back at each other) re-enqueues an already-completed dedup key at a
// deeper depth (frontier.Enqueue's doc comment); the corresponding setXxx
// call always happens again before that re-enqueue, so the entry exists by
// the time the item is redispatched even though it was dropped after the
// first completion. This ordering — set-then-enqueue, dequeue-then-read,
// complete-then-release — is what makes eager release safe.

// lockedMap is a mutex-guarded map: the minimal get/set/delete shape the
// provenance tracking needs, shared across the four maps instead of four
// hand-rolled accessor pairs.
type lockedMap[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]V
}

func newLockedMap[K comparable, V any]() *lockedMap[K, V] {
	return &lockedMap[K, V]{m: make(map[K]V)}
}

func (l *lockedMap[K, V]) get(k K) (V, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.m[k]
	return v, ok
}

func (l *lockedMap[K, V]) set(k K, v V) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[k] = v
}

func (l *lockedMap[K, V]) delete(k K) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, k)
}

func (e *Engine) setParentCatalog(url string, catalogID uint) {
	e.parentCatalogByURL.set(url, catalogID)
}

func (e *Engine) parentCatalogFor(url string) *uint {
	if id, ok := e.parentCatalogByURL.get(url); ok {
		return &id
	}
	return nil
}

func (e *Engine) setArtifactEntry(url string, entryID uint) {
	e.artifactEntryByURL.set(url, entryID)
}

func (e *Engine) artifactEntry(url string) uint {
	id, _ := e.artifactEntryByURL.get(url)
	return id
}

func (e *Engine) setCatalogMethod(url, method string) {
	e.catalogMethodByURL.set(url, method)
}

func (e *Engine) catalogMethodFor(url string) string {
	method, _ := e.catalogMethodByURL.get(url)
	return method
}

func (e *Engine) setRegistryContext(url string, entryID, catalogID, regRowID uint) {
	e.registryCtxByURL.set(url, registryContext{entryID: entryID, catalogID: catalogID, regRowID: regRowID})
}

func (e *Engine) registryContextFor(url string) (registryContext, bool) {
	return e.registryCtxByURL.get(url)
}

// releaseProvenance drops item's in-memory provenance entry (parent
// catalog, artifact entry, catalog discovery method, or registry context)
// once process() knows item will never be dispatched again. See the
// "Provenance tracking" doc comment above for why this is safe.
func (e *Engine) releaseProvenance(item store.FrontierItem) {
	switch item.Kind {
	case store.KindCatalogFetch:
		e.parentCatalogByURL.delete(item.URL)
		e.catalogMethodByURL.delete(item.URL)
	case store.KindArtifactFetch:
		e.artifactEntryByURL.delete(item.URL)
	case store.KindRegistryHarvest:
		e.registryCtxByURL.delete(item.URL)
	}
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
	e.touchPageLocked(host)
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
	e.touchPageLocked(host)
	if e.pageCounts[host] > 0 {
		e.pageCounts[host]--
	}
}

// touchPageLocked records host as the most-recently-used entry in the
// pageCounts LRU, creating it (at count 0) if new. Callers must hold
// pagesMu. If host is new and the tracker is already at maxTrackedDomains,
// the least-recently-touched host is evicted first (see the pagesMu doc
// comment on Engine for the correctness tradeoff this implies).
func (e *Engine) touchPageLocked(host string) {
	if el, ok := e.pageElems[host]; ok {
		e.pageOrder.MoveToFront(el)
		return
	}
	if e.pageOrder.Len() >= maxTrackedDomains {
		if oldest := e.pageOrder.Back(); oldest != nil {
			evicted := oldest.Value.(string)
			e.pageOrder.Remove(oldest)
			delete(e.pageElems, evicted)
			delete(e.pageCounts, evicted)
		}
	}
	e.pageElems[host] = e.pageOrder.PushFront(host)
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
