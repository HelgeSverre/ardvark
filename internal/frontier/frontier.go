// Package frontier implements a persistent work queue over the
// frontier_items table: enqueue with dedup, transactional dequeue-and-lease
// safe for concurrent workers, completion, and retry-with-backoff-to-failed
// semantics.
package frontier

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/helgesverre/ardvark/internal/store"
)

// Frontier is a persistent, dedup'd work queue backed by the
// frontier_items table.
type Frontier struct {
	db *gorm.DB
}

// New wraps a *gorm.DB (or Store.DB) in a Frontier.
func New(db *gorm.DB) *Frontier {
	return &Frontier{db: db}
}

// NewFromStore wraps a *store.Store in a Frontier.
func NewFromStore(s *store.Store) *Frontier {
	return &Frontier{db: s.DB}
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

	res := f.db.Model(&store.FrontierItem{}).Where("id = ?", existing.ID).Updates(map[string]any{
		"status":     store.FrontierStatusPending,
		"attempts":   0,
		"last_error": "",
		"run_id":     item.RunID,
		// Adopt the re-enqueue's depth. Without this a reference cycle
		// (e.g. two registries referring to each other) keeps re-activating
		// a done item at its original shallow depth, so the depth guard
		// never trips and the crawl loops forever.
		"depth": item.Depth,
	})
	if res.Error != nil {
		return false, fmt.Errorf("frontier: re-enqueue: %w", res.Error)
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
		if err := tx.Model(&store.FrontierItem{}).
			Where("id IN ?", ids).
			Updates(map[string]any{"status": store.FrontierStatusInFlight}).Error; err != nil {
			return fmt.Errorf("marking items in_flight: %w", err)
		}
		for i := range items {
			items[i].Status = store.FrontierStatusInFlight
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
		Updates(map[string]any{"status": store.FrontierStatusPending})
	if res.Error != nil {
		return fmt.Errorf("frontier: requeue %d: %w", id, res.Error)
	}
	return nil
}

// ReclaimInFlight returns any items stuck in_flight to pending and reports how
// many were reclaimed. ardvark runs one crawl process at a time, so an
// in_flight item at startup is always the residue of a previous process that
// was killed mid-batch; reclaiming them makes crash recovery resumable.
func (f *Frontier) ReclaimInFlight() (int64, error) {
	res := f.db.Model(&store.FrontierItem{}).
		Where("status = ?", store.FrontierStatusInFlight).
		Updates(map[string]any{"status": store.FrontierStatusPending})
	if res.Error != nil {
		return 0, fmt.Errorf("frontier: reclaim in-flight: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// Complete marks a frontier item as done.
func (f *Frontier) Complete(id uint) error {
	res := f.db.Model(&store.FrontierItem{}).Where("id = ?", id).
		Updates(map[string]any{"status": store.FrontierStatusDone})
	if res.Error != nil {
		return fmt.Errorf("frontier: complete %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("frontier: complete %d: item not found", id)
	}
	return nil
}

// Fail records a failure for item id: increments its attempt counter and
// last_error, then either re-queues it as pending (status "pending",
// available for immediate re-dequeue — the crawler's own backoff/retry
// scheduling layer is responsible for delaying re-dispatch) or, once
// attempts reaches maxAttempts, marks it permanently "failed".
func (f *Frontier) Fail(id uint, cause error, maxAttempts int) error {
	var item store.FrontierItem
	if err := f.db.First(&item, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("frontier: fail %d: item not found", id)
		}
		return fmt.Errorf("frontier: fail %d: %w", id, err)
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
		"attempts":   attempts,
		"last_error": errMsg,
		"status":     status,
	})
	if res.Error != nil {
		return fmt.Errorf("frontier: fail %d: %w", id, res.Error)
	}
	return nil
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
