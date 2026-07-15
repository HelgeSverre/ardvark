package frontier

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/store"
)

func newTestFrontier(t *testing.T) *Frontier {
	t.Helper()
	s, err := store.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s.DB)
}

func TestEnqueueDedup(t *testing.T) {
	f := newTestFrontier(t)

	item1 := &store.FrontierItem{Kind: store.KindHostProbe, Host: "example.com", DedupKey: "host_probe:example.com"}
	ok, err := f.Enqueue(item1)
	if err != nil {
		t.Fatalf("Enqueue (first): %v", err)
	}
	if !ok {
		t.Fatal("expected first enqueue to succeed")
	}
	if item1.ID == 0 {
		t.Fatal("expected non-zero ID after enqueue")
	}

	item2 := &store.FrontierItem{Kind: store.KindHostProbe, Host: "example.com", DedupKey: "host_probe:example.com"}
	ok, err = f.Enqueue(item2)
	if err != nil {
		t.Fatalf("Enqueue (duplicate): %v", err)
	}
	if ok {
		t.Fatal("expected duplicate enqueue to be a silent no-op")
	}

	count, err := f.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 pending item, got %d", count)
	}
}

func TestEnqueueRequiresDedupKey(t *testing.T) {
	f := newTestFrontier(t)
	_, err := f.Enqueue(&store.FrontierItem{Kind: store.KindHostProbe, Host: "example.com"})
	if err == nil {
		t.Fatal("expected error for missing DedupKey")
	}
}

func TestDequeueMarksInFlight(t *testing.T) {
	f := newTestFrontier(t)

	for i, host := range []string{"a.com", "b.com", "c.com"} {
		_, err := f.Enqueue(&store.FrontierItem{
			Kind:     store.KindHostProbe,
			Host:     host,
			DedupKey: "host_probe:" + host,
			Priority: i,
		})
		if err != nil {
			t.Fatalf("Enqueue %s: %v", host, err)
		}
	}

	items, err := f.Dequeue(2)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	for _, it := range items {
		if it.Status != store.FrontierStatusInFlight {
			t.Fatalf("expected in_flight status, got %q", it.Status)
		}
	}

	count, err := f.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 remaining pending item, got %d", count)
	}
}

func TestDequeueEmpty(t *testing.T) {
	f := newTestFrontier(t)
	items, err := f.Dequeue(5)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestCompleteMarksDone(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{Kind: store.KindHostProbe, Host: "done.com", DedupKey: "host_probe:done.com"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	if err := f.Complete(item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var reloaded store.FrontierItem
	if err := f.db.First(&reloaded, item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != store.FrontierStatusDone {
		t.Fatalf("expected done status, got %q", reloaded.Status)
	}
}

func TestCompleteNotFound(t *testing.T) {
	f := newTestFrontier(t)
	if err := f.Complete(9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected store.ErrNotFound for missing item, got %v", err)
	}
}

func TestFailRetriesThenFails(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{Kind: store.KindPageFetch, URL: "https://retry.com/", DedupKey: "page_fetch:https://retry.com/"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	const maxAttempts = 3
	cause := errors.New("timeout")

	// Attempts 1 and 2: re-queued as pending.
	for i := 1; i <= maxAttempts-1; i++ {
		permanent, err := f.Fail(item.ID, cause, maxAttempts)
		if err != nil {
			t.Fatalf("Fail (attempt %d): %v", i, err)
		}
		if permanent {
			t.Fatalf("attempt %d: expected Fail to report a retryable failure, got permanent", i)
		}
		var reloaded store.FrontierItem
		if err := f.db.First(&reloaded, item.ID).Error; err != nil {
			t.Fatalf("reload: %v", err)
		}
		if reloaded.Status != store.FrontierStatusPending {
			t.Fatalf("attempt %d: expected pending status, got %q", i, reloaded.Status)
		}
		if reloaded.Attempts != i {
			t.Fatalf("attempt %d: expected Attempts=%d, got %d", i, i, reloaded.Attempts)
		}
		if reloaded.LastError != "timeout" {
			t.Fatalf("attempt %d: expected last_error to be recorded, got %q", i, reloaded.LastError)
		}
	}

	// Final attempt reaches maxAttempts: permanently failed.
	permanent, err := f.Fail(item.ID, cause, maxAttempts)
	if err != nil {
		t.Fatalf("Fail (final): %v", err)
	}
	if !permanent {
		t.Fatal("expected Fail to report the final attempt as permanent")
	}
	var final store.FrontierItem
	if err := f.db.First(&final, item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if final.Status != store.FrontierStatusFailed {
		t.Fatalf("expected failed status, got %q", final.Status)
	}
	if final.Attempts != maxAttempts {
		t.Fatalf("expected Attempts=%d, got %d", maxAttempts, final.Attempts)
	}
}

func TestFailNotFound(t *testing.T) {
	f := newTestFrontier(t)
	if _, err := f.Fail(9999, errors.New("boom"), 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected store.ErrNotFound for missing item, got %v", err)
	}
}

func TestRequeueNotFound(t *testing.T) {
	f := newTestFrontier(t)
	if err := f.Requeue(9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected store.ErrNotFound for missing item, got %v", err)
	}
}

// TestConcurrentDequeueNoDoubleDelivery enqueues N items and has several
// goroutines race to dequeue them one at a time; the union of all dequeued
// items must contain each item exactly once.
func TestConcurrentDequeueNoDoubleDelivery(t *testing.T) {
	f := newTestFrontier(t)

	const total = 40
	for i := 0; i < total; i++ {
		host := "host" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + ".com"
		_, err := f.Enqueue(&store.FrontierItem{
			Kind:     store.KindHostProbe,
			Host:     host,
			DedupKey: "host_probe:" + host,
		})
		if err != nil {
			t.Fatalf("Enqueue %s: %v", host, err)
		}
	}

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[uint]int)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				items, err := f.Dequeue(1)
				if err != nil {
					t.Errorf("Dequeue: %v", err)
					return
				}
				if len(items) == 0 {
					return
				}
				mu.Lock()
				for _, it := range items {
					seen[it.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("expected %d unique items dequeued, got %d", total, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("item %d was dequeued %d times, expected exactly 1", id, n)
		}
	}
}

// A done item re-enqueued at a deeper depth must adopt that depth, or a
// reference cycle (two registries referring to each other) would re-activate
// it forever at its original shallow depth and never trip the depth guard.
func TestEnqueueResetAdoptsDeeperDepth(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{Kind: store.KindRegistryHarvest, URL: "https://r.example/api", Depth: 0, DedupKey: "reg:r"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := f.Complete(item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Re-enqueue the same dedup key at depth 2 (as a referral would).
	reenq := &store.FrontierItem{Kind: store.KindRegistryHarvest, URL: "https://r.example/api", Depth: 2, DedupKey: "reg:r"}
	ok, err := f.Enqueue(reenq)
	if err != nil {
		t.Fatalf("re-Enqueue: %v", err)
	}
	if !ok {
		t.Fatal("expected re-enqueue of a done item to reset it to pending")
	}

	items, err := f.Dequeue(1)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 1 || items[0].Depth != 2 {
		t.Fatalf("expected reclaimed item at depth 2, got %+v", items)
	}
}

// TestEnqueueResetAdoptsNewProvenance ensures that when Enqueue reactivates
// a done/failed row, it also adopts the re-enqueue's provenance columns
// (parent catalog, artifact entry, registry context, probe method) rather
// than leaving the prior row's stale values in place: a re-activated item
// must be attributed to whoever is referencing it *this* time, not
// whichever caller happened to enqueue it originally.
func TestEnqueueResetAdoptsNewProvenance(t *testing.T) {
	f := newTestFrontier(t)

	oldParent := uint(1)
	item := &store.FrontierItem{
		Kind: store.KindCatalogFetch, URL: "https://c.example/nested.json", Depth: 0,
		DedupKey: "catalog_fetch:https://c.example/nested.json", ParentCatalogID: &oldParent,
		ProbeMethod: "well_known",
	}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := f.Complete(item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	newParent := uint(2)
	reenq := &store.FrontierItem{
		Kind: store.KindCatalogFetch, URL: "https://c.example/nested.json", Depth: 1,
		DedupKey: "catalog_fetch:https://c.example/nested.json", ParentCatalogID: &newParent,
		ProbeMethod: "link_tag",
	}
	ok, err := f.Enqueue(reenq)
	if err != nil {
		t.Fatalf("re-Enqueue: %v", err)
	}
	if !ok {
		t.Fatal("expected re-enqueue of a done item to reset it to pending")
	}

	items, err := f.Dequeue(1)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 dequeued item, got %d", len(items))
	}
	got := items[0]
	if got.ParentCatalogID == nil || *got.ParentCatalogID != newParent {
		t.Errorf("expected reactivated item to adopt new parent_catalog_id %d, got %v", newParent, got.ParentCatalogID)
	}
	if got.ProbeMethod != "link_tag" {
		t.Errorf("expected reactivated item to adopt new probe_method %q, got %q", "link_tag", got.ProbeMethod)
	}
}

// ReclaimInFlight returns items stranded in_flight by a killed process back to
// pending so a resumed crawl picks them up.
func TestReclaimInFlight(t *testing.T) {
	f := newTestFrontier(t)

	if _, err := f.Enqueue(&store.FrontierItem{Kind: store.KindHostProbe, Host: "a.example", DedupKey: "hp:a"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Dequeue moves it to in_flight but we never Complete/Fail it (crash).
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if got, _ := f.Dequeue(1); len(got) != 0 {
		t.Fatal("expected nothing pending while item is in_flight")
	}

	n, err := f.ReclaimInFlight()
	if err != nil {
		t.Fatalf("ReclaimInFlight: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}
	if got, _ := f.Dequeue(1); len(got) != 1 {
		t.Fatal("expected reclaimed item to be dequeuable again")
	}
}

// Requeue returns an item to pending without burning an attempt (used on
// graceful shutdown so interrupted work resumes cleanly).
func TestRequeue(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{Kind: store.KindHostProbe, Host: "b.example", DedupKey: "hp:b"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := f.Requeue(item.ID); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	got, err := f.Dequeue(1)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(got) != 1 || got[0].Attempts != 0 {
		t.Fatalf("expected requeued item with 0 attempts, got %+v", got)
	}
}

// -- Lease-based claiming ----------------------------------------------

// Dequeue must stamp a future leased_until and a non-empty worker_id on
// every item it marks in_flight, both in the returned slice and in the
// persisted row.
func TestDequeueSetsLease(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{Kind: store.KindHostProbe, Host: "lease.example", DedupKey: "hp:lease"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	before := time.Now()
	items, err := f.Dequeue(1)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	got := items[0]
	if got.LeasedUntil == nil {
		t.Fatal("expected LeasedUntil to be set on the returned item")
	}
	if !got.LeasedUntil.After(before) {
		t.Fatalf("expected LeasedUntil in the future, got %v (before=%v)", got.LeasedUntil, before)
	}
	if got.WorkerID == "" {
		t.Fatal("expected WorkerID to be set on the returned item")
	}

	var reloaded store.FrontierItem
	if err := f.db.First(&reloaded, item.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.LeasedUntil == nil {
		t.Fatal("expected LeasedUntil to be persisted")
	}
	if reloaded.WorkerID == "" {
		t.Fatal("expected WorkerID to be persisted")
	}
}

// WithLeaseSeconds must control the lease duration Dequeue stamps.
func TestWithLeaseSecondsOption(t *testing.T) {
	s, err := store.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	f := New(s.DB, WithLeaseSeconds(120), WithWorkerID("worker-a"))
	if _, err := f.Enqueue(&store.FrontierItem{Kind: store.KindHostProbe, Host: "opt.example", DedupKey: "hp:opt"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	items, err := f.Dequeue(1)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].WorkerID != "worker-a" {
		t.Fatalf("WorkerID = %q, want %q", items[0].WorkerID, "worker-a")
	}
	remaining := time.Until(*items[0].LeasedUntil)
	if remaining < 100*time.Second || remaining > 130*time.Second {
		t.Fatalf("expected ~120s lease remaining, got %v", remaining)
	}
}

// WithLeaseSeconds(0) (or a negative value) must be a no-op: config keys
// use 0/absent to mean "use the default", not "no lease".
func TestWithLeaseSecondsZeroIsNoOp(t *testing.T) {
	s, err := store.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	f := New(s.DB, WithLeaseSeconds(0))
	if f.leaseDuration != defaultLeaseSeconds*time.Second {
		t.Fatalf("leaseDuration = %v, want default %v", f.leaseDuration, defaultLeaseSeconds*time.Second)
	}
}

// Complete, Fail, and Requeue must all clear leased_until/worker_id, so a
// finished or returned-to-pending item never looks like it's still
// (validly) leased.
func TestCompleteClearsLease(t *testing.T) {
	f := newTestFrontier(t)
	item := &store.FrontierItem{Kind: store.KindHostProbe, Host: "c.example", DedupKey: "hp:c"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := f.Complete(item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	assertLeaseCleared(t, f, item.ID)
}

func TestFailClearsLease(t *testing.T) {
	f := newTestFrontier(t)
	item := &store.FrontierItem{Kind: store.KindHostProbe, Host: "f.example", DedupKey: "hp:f"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := f.Fail(item.ID, errors.New("boom"), 3); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	assertLeaseCleared(t, f, item.ID)
}

func TestRequeueClearsLease(t *testing.T) {
	f := newTestFrontier(t)
	item := &store.FrontierItem{Kind: store.KindHostProbe, Host: "r.example", DedupKey: "hp:r"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := f.Requeue(item.ID); err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	assertLeaseCleared(t, f, item.ID)
}

func assertLeaseCleared(t *testing.T, f *Frontier, id uint) {
	t.Helper()
	var reloaded store.FrontierItem
	if err := f.db.First(&reloaded, id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.LeasedUntil != nil {
		t.Fatalf("expected LeasedUntil cleared, got %v", reloaded.LeasedUntil)
	}
	if reloaded.WorkerID != "" {
		t.Fatalf("expected WorkerID cleared, got %q", reloaded.WorkerID)
	}
}

// ReclaimExpired must reset only in_flight items whose lease has passed,
// leaving still-valid leases (a peer worker's legitimate in-flight item)
// untouched.
func TestReclaimExpiredOnlyExpired(t *testing.T) {
	f := newTestFrontier(t)

	for _, host := range []string{"expired.example", "fresh.example"} {
		if _, err := f.Enqueue(&store.FrontierItem{Kind: store.KindHostProbe, Host: host, DedupKey: "hp:" + host}); err != nil {
			t.Fatalf("Enqueue %s: %v", host, err)
		}
	}

	items, err := f.Dequeue(2)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}

	// Simulate a lease that already expired for the first item only.
	past := time.Now().Add(-time.Hour)
	if err := f.db.Model(&store.FrontierItem{}).Where("id = ?", items[0].ID).
		Update("leased_until", &past).Error; err != nil {
		t.Fatalf("forcing expired lease: %v", err)
	}

	n, err := f.ReclaimExpired()
	if err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}

	var expired, fresh store.FrontierItem
	if err := f.db.First(&expired, items[0].ID).Error; err != nil {
		t.Fatalf("reload expired: %v", err)
	}
	if expired.Status != store.FrontierStatusPending {
		t.Fatalf("expired item status = %q, want pending", expired.Status)
	}
	if expired.LeasedUntil != nil {
		t.Fatalf("expected expired item's lease cleared, got %v", expired.LeasedUntil)
	}

	if err := f.db.First(&fresh, items[1].ID).Error; err != nil {
		t.Fatalf("reload fresh: %v", err)
	}
	if fresh.Status != store.FrontierStatusInFlight {
		t.Fatalf("fresh item status = %q, want in_flight (must not be reclaimed)", fresh.Status)
	}
	if fresh.LeasedUntil == nil {
		t.Fatal("expected fresh item's lease to remain set")
	}
}

// -- Counts --------------------------------------------------------------

func TestCounts(t *testing.T) {
	f := newTestFrontier(t)

	for _, host := range []string{"a.example", "b.example", "c.example"} {
		if _, err := f.Enqueue(&store.FrontierItem{Kind: store.KindHostProbe, Host: host, DedupKey: "hp:" + host}); err != nil {
			t.Fatalf("Enqueue %s: %v", host, err)
		}
	}
	if _, err := f.Dequeue(2); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	pending, inFlight, err := f.Counts()
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending = %d, want 1", pending)
	}
	if inFlight != 2 {
		t.Fatalf("inFlight = %d, want 2", inFlight)
	}
}

// -- Re-enqueue race guard -------------------------------------------------

// Enqueue's reset-a-finished-item-to-pending path is a read (lookup the
// existing row) followed by a write (guarded UPDATE); concurrent callers
// racing to re-enqueue the same finished dedup key must produce exactly one
// winner, not one winner per concurrent caller that happened to read the
// row before any of the writes landed. Because sqlite serializes writes,
// this test is deterministic: whichever UPDATE actually executes first
// flips the status away from done/failed, so every later UPDATE's
// "AND status IN (...)" guard matches zero rows and reports (false, nil).
func TestEnqueueReenqueueRaceGuard(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{Kind: store.KindHostProbe, Host: "race.example", DedupKey: "hp:race"}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := f.Complete(item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	const racers = 10
	var wg sync.WaitGroup
	var wins atomic.Int64
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reenq := &store.FrontierItem{Kind: store.KindHostProbe, Host: "race.example", DedupKey: "hp:race"}
			ok, err := f.Enqueue(reenq)
			if err != nil {
				t.Errorf("Enqueue: %v", err)
				return
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := wins.Load(); got != 1 {
		t.Fatalf("expected exactly 1 winning re-enqueue, got %d", got)
	}

	// The row itself must end up pending exactly once — not corrupted by
	// losing racers' partial updates.
	count, err := f.PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if count != 1 {
		t.Fatalf("PendingCount = %d, want 1", count)
	}
}
