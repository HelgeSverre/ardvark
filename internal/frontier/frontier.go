// Package frontier implements a persistent work queue over the
// frontier_items table: enqueue with dedup, transactional dequeue-and-lease
// safe for concurrent workers, completion, and retry-with-backoff-to-failed
// semantics.
package frontier

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/helgesverre/ardvark/internal/store"
)

// defaultLeaseSeconds is the in_flight lease duration used when neither
// New's caller nor config.Config.Crawler.LeaseSeconds specifies one. See
// store.FrontierItem.LeasedUntil's doc comment for what the lease is for.
const defaultLeaseSeconds = 300

// Frontier is a persistent, dedup'd work queue backed by the
// frontier_items table.
//
// Mutators addressed by item id (Complete, Requeue, Fail) return an error
// wrapping store.ErrNotFound when the id matches no row: an in-flight item
// vanishing mid-crawl indicates a logic bug, never a normal outcome.
// ReclaimInFlight and ReclaimExpired are bulk sweeps for which zero matched
// rows is normal (a clean start, or one with no expired leases, has nothing
// to reclaim).
type Frontier struct {
	db            *gorm.DB
	leaseDuration time.Duration
	workerID      string

	// shardCount and shardIndex implement host-affinity sharding (see
	// store.FrontierItem.HostShard's doc comment): when shardCount > 1,
	// Dequeue restricts itself to rows whose host_shard falls in this
	// worker's partition, so N cooperating worker processes each own a
	// disjoint slice of hosts. shardCount <= 1 (the default) disables the
	// filter entirely — a single worker naturally owns the whole frontier,
	// and skipping the extra WHERE clause keeps the common (single-process)
	// case's query plan unchanged.
	shardCount int
	shardIndex int
}

// Option configures a Frontier at construction time. See WithLeaseSeconds
// and WithWorkerID.
type Option func(*Frontier)

// WithLeaseSeconds sets how long a dequeued item's in_flight lease lasts
// (see store.FrontierItem.LeasedUntil). seconds <= 0 is a no-op, leaving
// the default (or a previously-applied option) in place — config keys use
// 0/absent to mean "use the default", so callers can pass
// cfg.Crawler.LeaseSeconds straight through without a branch.
func WithLeaseSeconds(seconds int) Option {
	return func(f *Frontier) {
		if seconds > 0 {
			f.leaseDuration = time.Duration(seconds) * time.Second
		}
	}
}

// WithWorkerID sets the identifier recorded in frontier_items.worker_id for
// items this Frontier dequeues. Purely informational (reclaiming is
// decided by LeasedUntil, not worker identity); useful for operational
// visibility into which worker process is holding which item.
func WithWorkerID(id string) Option {
	return func(f *Frontier) {
		f.workerID = id
	}
}

// WithWorkerShard configures this Frontier to only dequeue items whose
// store.FrontierItem.HostShard falls in this worker's partition of
// count cooperating worker processes: "host_shard % count = index". index
// must be in [0, count) — callers should validate this at config load
// (see config.Config.Crawler.Worker) rather than relying on Dequeue to
// catch a misconfiguration, since an out-of-range index would silently
// dequeue nothing forever.
//
// count <= 1 is a no-op, leaving sharding disabled (the default: a single
// worker owns the whole frontier) — config keys use 1/absent to mean "no
// distributed sharding", so callers can pass cfg.Crawler.Worker.Count
// straight through without a branch.
func WithWorkerShard(index, count int) Option {
	return func(f *Frontier) {
		if count > 1 {
			f.shardCount = count
			f.shardIndex = index
		}
	}
}

// hostShard computes the store.FrontierItem.HostShard value for host: a
// stable, portable partition key (fnv32a % store.HostShardCount) that every
// item for the same host always maps to, regardless of which worker process
// enqueues it. See store.FrontierItem.HostShard's doc comment.
func hostShard(host string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(host)) // fnv32a.Write never errors
	return int(h.Sum32() % store.HostShardCount)
}

// New wraps a *gorm.DB (or Store.DB) in a Frontier. Without options the
// lease duration is defaultLeaseSeconds and the worker id is a
// best-effort hostname:pid string (see defaultWorkerID).
func New(db *gorm.DB, opts ...Option) *Frontier {
	f := &Frontier{
		db:            db,
		leaseDuration: defaultLeaseSeconds * time.Second,
		workerID:      defaultWorkerID(),
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// defaultWorkerID returns a best-effort "hostname:pid" identifier for the
// current process, falling back to just the pid if the hostname is
// unavailable. Never fails: worker_id is informational only.
func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return fmt.Sprintf("pid-%d", os.Getpid())
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// Item is a unit of frontier work.
type Item = store.FrontierItem

// Enqueue inserts item with status "pending" if item.DedupKey does not
// already exist in the frontier.
//
// If the dedup key already exists but the prior item has finished (status
// "done" or "failed"), Enqueue resets that row back to "pending" (clearing
// attempts/last_error and adopting the new item's RunID) and returns (true,
// nil): completed work is otherwise permanently un-re-enqueueable, which
// would make freshness-window re-probing (crawler.refreshAfterHours) and
// --force both dead code across runs, since the caller's own downstream
// checks (e.g. RecentlyProbed, content-hash comparisons) are what actually
// decide whether the re-dispatched item does new work.
//
// If the prior item is still "pending" or "in_flight", Enqueue is a silent
// no-op and returns (false, nil) — true in-flight dedup, to avoid
// duplicate concurrent work.
func (f *Frontier) Enqueue(item *store.FrontierItem) (bool, error) {
	if item.DedupKey == "" {
		return false, fmt.Errorf("frontier: enqueue requires a non-empty DedupKey")
	}
	if item.Status == "" {
		item.Status = store.FrontierStatusPending
	}
	// HostShard is derived, never caller-supplied: computing it here (the
	// one place every item passes through before being written) guarantees
	// every row's shard reflects its own Host, regardless of which
	// enqueuing call site (seed, page-fetch fan-out, ...) built the item.
	item.HostShard = hostShard(item.Host)

	err := f.db.Create(item).Error
	if err == nil {
		return true, nil
	}
	if !isUniqueConstraintErr(err) {
		return false, fmt.Errorf("frontier: enqueue: %w", err)
	}

	var existing store.FrontierItem
	if lookupErr := f.db.Where("dedup_key = ?", item.DedupKey).First(&existing).Error; lookupErr != nil {
		return false, nil
	}
	if existing.Status != store.FrontierStatusDone && existing.Status != store.FrontierStatusFailed {
		return false, nil
	}

	// The status check above and this UPDATE are not atomic: another
	// worker could dequeue/complete/fail the same row in between (e.g.
	// re-enqueue a "done" reference-cycle target concurrently with a
	// fresh dequeue of it after some other path reset it to pending). The
	// WHERE clause's own status guard makes the UPDATE itself the atomic
	// decision point — if it matches zero rows, someone else already
	// changed the row's status out from under us, so this call lost the
	// race and must not report success.
	res := f.db.Model(&store.FrontierItem{}).
		Where("id = ? AND status IN ?", existing.ID, []string{store.FrontierStatusDone, store.FrontierStatusFailed}).
		Updates(map[string]any{
			"status":       store.FrontierStatusPending,
			"attempts":     0,
			"last_error":   "",
			"run_id":       item.RunID,
			"leased_until": nil,
			"worker_id":    "",
			// Adopt the re-enqueue's depth. Without this a reference cycle
			// (e.g. two registries referring to each other) keeps re-activating
			// a done item at its original shallow depth, so the depth guard
			// never trips and the crawl loops forever.
			"depth": item.Depth,
			// Adopt the re-enqueue's provenance too: the prior (done/failed)
			// row's columns describe who referenced it *last time*, which may
			// no longer be accurate (e.g. a different parent catalog now
			// references the same URL), and a stale value here would silently
			// misattribute the re-activated item's result.
			"parent_catalog_id":   item.ParentCatalogID,
			"artifact_entry_id":   item.ArtifactEntryID,
			"registry_entry_id":   item.RegistryEntryID,
			"registry_catalog_id": item.RegistryCatalogID,
			"registry_row_id":     item.RegistryRowID,
			"probe_method":        item.ProbeMethod,
		})
	if res.Error != nil {
		return false, fmt.Errorf("frontier: re-enqueue: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		// Someone else won the race (already reset or re-dequeued this
		// row); this enqueue attempt did nothing.
		return false, nil
	}
	return true, nil
}

// isUniqueConstraintErr reports whether err looks like a unique-constraint
// violation, across sqlite/mysql/postgres error message shapes.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	needles := []string{
		"UNIQUE constraint failed", // sqlite
		"Duplicate entry",          // mysql
		"duplicate key value",      // postgres
		"SQLSTATE 23505",           // postgres
	}
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// Dequeue atomically marks up to n pending items as in_flight and returns
// them, ordered by priority descending then creation order. Safe for
// concurrent callers: uses a transaction with row locking appropriate to
// the underlying driver (SELECT ... FOR UPDATE SKIP LOCKED where the driver
// supports it; sqlite's single-writer transaction serialization otherwise).
func (f *Frontier) Dequeue(n int) ([]store.FrontierItem, error) {
	if n <= 0 {
		return nil, nil
	}

	var items []store.FrontierItem
	err := f.db.Transaction(func(tx *gorm.DB) error {
		q := tx.Where("status = ?", store.FrontierStatusPending).
			Order("priority desc, id asc").
			Limit(n)

		if f.shardCount > 1 {
			// Portable across sqlite/mysql/postgres: '%' is a standard SQL
			// operator on all three, unlike MOD() (no sqlite support).
			q = q.Where("host_shard % ? = ?", f.shardCount, f.shardIndex)
		}

		if lockingSupported(tx) {
			q = q.Clauses(clause.Locking{
				Strength: "UPDATE",
				Options:  "SKIP LOCKED",
			})
		}

		if err := q.Find(&items).Error; err != nil {
			return fmt.Errorf("selecting pending items: %w", err)
		}
		if len(items) == 0 {
			return nil
		}

		ids := make([]uint, len(items))
		for i, it := range items {
			ids[i] = it.ID
		}
		leasedUntil := time.Now().Add(f.leaseDuration)
		if err := tx.Model(&store.FrontierItem{}).
			Where("id IN ?", ids).
			Updates(map[string]any{
				"status":       store.FrontierStatusInFlight,
				"leased_until": leasedUntil,
				"worker_id":    f.workerID,
			}).Error; err != nil {
			return fmt.Errorf("marking items in_flight: %w", err)
		}
		for i := range items {
			items[i].Status = store.FrontierStatusInFlight
			items[i].LeasedUntil = &leasedUntil
			items[i].WorkerID = f.workerID
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("frontier: dequeue: %w", err)
	}
	return items, nil
}

// lockingSupported reports whether the underlying dialector supports
// SELECT ... FOR UPDATE row locking (sqlite does not; mysql/postgres do).
func lockingSupported(tx *gorm.DB) bool {
	name := tx.Dialector.Name()
	return name == "mysql" || name == "postgres"
}

// Requeue returns an item to pending without touching its attempt counter.
// Used when work is interrupted (context cancelled) rather than failed, so a
// resumed run picks it up again cleanly.
func (f *Frontier) Requeue(id uint) error {
	res := f.db.Model(&store.FrontierItem{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":       store.FrontierStatusPending,
			"leased_until": nil,
			"worker_id":    "",
		})
	if res.Error != nil {
		return fmt.Errorf("frontier: requeue %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("frontier: requeue %d: %w", id, store.ErrNotFound)
	}
	return nil
}

// ReclaimInFlight returns every item currently in_flight to pending
// (clearing its lease), regardless of whether its lease has expired, and
// reports how many were reclaimed. This blanket sweep is only safe under
// the single-process assumption: ardvark's sqlite storage backend supports
// exactly one crawl process at a time (see store.Open's connection-pool
// comment), so an in_flight row at startup is always the residue of a
// previous process that was killed mid-batch, never a peer still working
// on it. On mysql/postgres, where multiple worker processes may share the
// frontier concurrently, use ReclaimExpired instead.
func (f *Frontier) ReclaimInFlight() (int64, error) {
	res := f.db.Model(&store.FrontierItem{}).
		Where("status = ?", store.FrontierStatusInFlight).
		Updates(map[string]any{
			"status":       store.FrontierStatusPending,
			"leased_until": nil,
			"worker_id":    "",
		})
	if res.Error != nil {
		return 0, fmt.Errorf("frontier: reclaim in-flight: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// ReclaimExpired returns to pending only those items that are in_flight
// with a lease that has already passed, clearing their lease fields, and
// reports how many were reclaimed. Unlike ReclaimInFlight's blanket sweep,
// this is safe to run at any time against a frontier shared by multiple
// worker processes (mysql/postgres): an in_flight row whose lease has not
// yet expired may still be legitimately owned by a live peer, so only
// expired leases are ever touched.
func (f *Frontier) ReclaimExpired() (int64, error) {
	res := f.db.Model(&store.FrontierItem{}).
		Where("status = ? AND leased_until < ?", store.FrontierStatusInFlight, time.Now()).
		Updates(map[string]any{
			"status":       store.FrontierStatusPending,
			"leased_until": nil,
			"worker_id":    "",
		})
	if res.Error != nil {
		return 0, fmt.Errorf("frontier: reclaim expired: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// Complete marks a frontier item as done, releasing its lease.
func (f *Frontier) Complete(id uint) error {
	res := f.db.Model(&store.FrontierItem{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":       store.FrontierStatusDone,
			"leased_until": nil,
			"worker_id":    "",
		})
	if res.Error != nil {
		return fmt.Errorf("frontier: complete %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("frontier: complete %d: %w", id, store.ErrNotFound)
	}
	return nil
}

// Fail records a failure for item id: increments its attempt counter and
// last_error, then either re-queues it as pending (status "pending",
// available for immediate re-dequeue — the crawler's own backoff/retry
// scheduling layer is responsible for delaying re-dispatch) or, once
// attempts reaches maxAttempts, marks it permanently "failed". The returned
// permanent flag reports which of the two happened, so callers do not have
// to mirror the attempts arithmetic to know whether the item will ever be
// dispatched again.
func (f *Frontier) Fail(id uint, cause error, maxAttempts int) (permanent bool, err error) {
	var item store.FrontierItem
	if err := f.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, fmt.Errorf("frontier: fail %d: %w", id, store.ErrNotFound)
		}
		return false, fmt.Errorf("frontier: fail %d: %w", id, err)
	}

	attempts := item.Attempts + 1
	status := store.FrontierStatusPending
	if attempts >= maxAttempts {
		status = store.FrontierStatusFailed
	}

	errMsg := ""
	if cause != nil {
		errMsg = cause.Error()
	}

	res := f.db.Model(&store.FrontierItem{}).Where("id = ?", id).Updates(map[string]any{
		"attempts":     attempts,
		"last_error":   errMsg,
		"status":       status,
		"leased_until": nil,
		"worker_id":    "",
	})
	if res.Error != nil {
		return false, fmt.Errorf("frontier: fail %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return false, fmt.Errorf("frontier: fail %d: %w", id, store.ErrNotFound)
	}
	return status == store.FrontierStatusFailed, nil
}

// PendingCount returns the number of items currently in "pending" status.
func (f *Frontier) PendingCount() (int64, error) {
	var count int64
	if err := f.db.Model(&store.FrontierItem{}).
		Where("status = ?", store.FrontierStatusPending).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("frontier: pending count: %w", err)
	}
	return count, nil
}

// CountByHostKind returns how many frontier_items rows exist for host with
// the given kind, across every status (pending, in_flight, done, failed).
// The crawl engine uses this at enqueue time to enforce
// crawler.maxPagesPerDomain against kind page_fetch: since Enqueue's dedup
// key guarantees at most one row per distinct URL, this count is an exact
// per-process page budget check (see the Engine doc comment on
// maxPagesPerDomain for the cross-worker race this does not cover).
func (f *Frontier) CountByHostKind(host, kind string) (int64, error) {
	var count int64
	if err := f.db.Model(&store.FrontierItem{}).
		Where("host = ? AND kind = ?", host, kind).
		Count(&count).Error; err != nil {
		return 0, fmt.Errorf("frontier: count by host/kind: %w", err)
	}
	return count, nil
}

// Counts returns the number of items currently pending and in_flight. The
// crawl engine uses this as its global termination check: a dequeue coming
// back empty only means this worker's view of the frontier is exhausted,
// not that the frontier is — other worker processes sharing the same
// mysql/postgres database may still be holding in_flight items (which
// could enqueue further work) or have pending items yet to be claimed.
func (f *Frontier) Counts() (pending, inFlight int64, err error) {
	if err = f.db.Model(&store.FrontierItem{}).
		Where("status = ?", store.FrontierStatusPending).
		Count(&pending).Error; err != nil {
		return 0, 0, fmt.Errorf("frontier: counts (pending): %w", err)
	}
	if err = f.db.Model(&store.FrontierItem{}).
		Where("status = ?", store.FrontierStatusInFlight).
		Count(&inFlight).Error; err != nil {
		return 0, 0, fmt.Errorf("frontier: counts (in_flight): %w", err)
	}
	return pending, inFlight, nil
}
