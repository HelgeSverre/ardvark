package frontier

import (
	"errors"
	"fmt"
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

// TestEnqueueHostShardUsesURLHostNotAttributionHost verifies that HostShard
// is derived from item.URL's hostname (the host that will actually be
// fetched) rather than item.Host (which entry follow-ups set to the parent
// catalog's host for attribution/budget purposes and may legitimately differ,
// e.g. an artifact_fetch item for an artifact served from a CDN domain).
func TestEnqueueHostShardUsesURLHostNotAttributionHost(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{
		Kind:     store.KindArtifactFetch,
		Host:     "parent.example",               // attribution: parent catalog's host
		URL:      "https://cdn.other.com/a.json", // fetch target: a different host
		DedupKey: "artifact:https://cdn.other.com/a.json",
	}
	ok, err := f.Enqueue(item)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !ok {
		t.Fatal("expected enqueue to succeed")
	}

	if item.Host != "parent.example" {
		t.Fatalf("Enqueue must not mutate item.Host, got %q", item.Host)
	}
	if want := hostShard("cdn.other.com"); item.HostShard != want {
		t.Fatalf("HostShard = %d, want %d (fnv32a of URL host cdn.other.com, not Host parent.example)", item.HostShard, want)
	}
}

// TestEnqueueHostShardFallsBackToHostWithoutURL verifies host_probe items
// (which have no URL to fetch — the probe itself discovers the URLs) still
// shard on Host, since there is no separate fetch target to prefer.
func TestEnqueueHostShardFallsBackToHostWithoutURL(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{
		Kind:     store.KindHostProbe,
		Host:     "example.com",
		DedupKey: "host_probe:example.com",
	}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if want := hostShard("example.com"); item.HostShard != want {
		t.Fatalf("HostShard = %d, want %d (fnv32a of Host, no URL present)", item.HostShard, want)
	}
}

// TestEnqueueHostShardFallsBackToHostOnMalformedURL verifies that an
// unparseable URL degrades gracefully to sharding on Host rather than
// erroring or panicking.
func TestEnqueueHostShardFallsBackToHostOnMalformedURL(t *testing.T) {
	f := newTestFrontier(t)

	item := &store.FrontierItem{
		Kind:     store.KindArtifactFetch,
		Host:     "example.com",
		URL:      "://not a valid url", // malformed: url.Parse returns an error
		DedupKey: "artifact:malformed",
	}
	if _, err := f.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if want := hostShard("example.com"); item.HostShard != want {
		t.Fatalf("HostShard = %d, want %d (fallback to Host on malformed URL)", item.HostShard, want)
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

	// Attempts 1 and 2: re-queued as pending. Fail only acts on an item this
	// worker holds in_flight, so re-dequeue before each attempt exactly as
	// the crawl loop does (Dequeue → handler error → Fail).
	for i := 1; i <= maxAttempts-1; i++ {
		if _, err := f.Dequeue(1); err != nil {
			t.Fatalf("Dequeue (attempt %d): %v", i, err)
		}
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
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue (final): %v", err)
	}
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
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
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
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
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

// -- Lost-lease ownership guards ----------------------------------------

// loseLease reproduces the distributed-contention scenario the ownership
// guards defend against: frontier A dequeues an item (taking the lease), the
// lease is forced expired, then frontier B (a different worker, distinct
// worker_id) reclaims and re-dequeues it. Afterwards the row is legitimately
// owned by B, so any mutator A calls on it must be a no-op returning
// ErrLeaseLost — never a clobber of B's fresh claim.
func loseLease(t *testing.T) (a *Frontier, id uint) {
	t.Helper()
	s, err := store.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	a = New(s.DB, WithWorkerID("worker-a"))
	b := New(s.DB, WithWorkerID("worker-b"))

	item := &store.FrontierItem{Kind: store.KindPageFetch, URL: "https://lost.example/", DedupKey: "pf:lost"}
	if _, err := a.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, err := a.Dequeue(1)
	if err != nil || len(got) != 1 {
		t.Fatalf("A Dequeue: err=%v got=%d", err, len(got))
	}
	id = got[0].ID

	// Force A's lease into the past so B's ReclaimExpired is entitled to it.
	past := time.Now().Add(-time.Hour)
	if err := a.db.Model(&store.FrontierItem{}).Where("id = ?", id).
		Update("leased_until", &past).Error; err != nil {
		t.Fatalf("forcing expired lease: %v", err)
	}
	if n, err := b.ReclaimExpired(); err != nil || n != 1 {
		t.Fatalf("B ReclaimExpired: n=%d err=%v", n, err)
	}
	got, err = b.Dequeue(1)
	if err != nil || len(got) != 1 {
		t.Fatalf("B Dequeue: err=%v got=%d", err, len(got))
	}
	if got[0].ID != id {
		t.Fatalf("B dequeued %d, want the reclaimed item %d", got[0].ID, id)
	}
	if got[0].WorkerID != "worker-b" {
		t.Fatalf("reclaimed item worker_id = %q, want worker-b", got[0].WorkerID)
	}
	return a, id
}

// assertOwnedBy asserts the row is in the given status under the given
// worker_id — i.e. a lost-lease mutator left the new owner's claim intact.
func assertOwnedBy(t *testing.T, f *Frontier, id uint, status, worker string) {
	t.Helper()
	var it store.FrontierItem
	if err := f.db.First(&it, id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if it.Status != status {
		t.Fatalf("status = %q, want %q", it.Status, status)
	}
	if it.WorkerID != worker {
		t.Fatalf("worker_id = %q, want %q", it.WorkerID, worker)
	}
}

// A Complete from a worker whose lease was lost must not mark the item done
// (that would clobber the new owner's in-flight claim and double-run its
// SaveEntries); it returns ErrLeaseLost and changes nothing.
func TestCompleteLostLeaseIsNoOp(t *testing.T) {
	a, id := loseLease(t)
	if err := a.Complete(id); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Complete after lease loss = %v, want ErrLeaseLost", err)
	}
	if errors.Is(a.Complete(id), store.ErrNotFound) {
		t.Fatal("lost lease must be distinguishable from ErrNotFound")
	}
	assertOwnedBy(t, a, id, store.FrontierStatusInFlight, "worker-b")
}

// A Requeue from a worker whose lease was lost must not flip the item back to
// pending under the new owner; it returns ErrLeaseLost and changes nothing.
func TestRequeueLostLeaseIsNoOp(t *testing.T) {
	a, id := loseLease(t)
	if err := a.Requeue(id); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Requeue after lease loss = %v, want ErrLeaseLost", err)
	}
	assertOwnedBy(t, a, id, store.FrontierStatusInFlight, "worker-b")
}

// A Fail from a worker whose lease was lost must neither record the failure
// nor burn an attempt against the new owner's fresh claim; it returns
// ErrLeaseLost with permanent=false and leaves attempts at 0.
func TestFailLostLeaseDoesNotIncrementAttempts(t *testing.T) {
	a, id := loseLease(t)
	permanent, err := a.Fail(id, errors.New("boom"), 3)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Fail after lease loss = %v, want ErrLeaseLost", err)
	}
	if permanent {
		t.Fatal("expected permanent=false on a lost lease")
	}
	var it store.FrontierItem
	if err := a.db.First(&it, id).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if it.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 (a lost lease must not count an attempt)", it.Attempts)
	}
	assertOwnedBy(t, a, id, store.FrontierStatusInFlight, "worker-b")
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
	if _, err := f.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
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

// -- Host-affinity sharding ---------------------------------------------

// TestDequeueShardFilterPartitionsByHost verifies that WithWorkerShard
// restricts Dequeue to items whose host_shard matches this worker's
// partition, that two workers covering the full shard space (index 0 of 2,
// index 1 of 2) between them drain every item with no overlap, and that a
// single worker (count=1, sharding disabled) drains everything by itself.
func TestDequeueShardFilterPartitionsByHost(t *testing.T) {
	s, err := store.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Enqueue with a plain (unsharded) frontier; HostShard is stamped by
	// Enqueue regardless of which Frontier value's options are used to
	// dequeue later.
	seed := New(s.DB)
	var hosts []string
	for i := 0; i < 64; i++ {
		host := fmt.Sprintf("shard%d.example", i)
		hosts = append(hosts, host)
		item := &store.FrontierItem{
			Kind:     store.KindHostProbe,
			Host:     host,
			DedupKey: "hp:" + host,
		}
		if _, err := seed.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", host, err)
		}
	}

	worker0 := New(s.DB, WithWorkerShard(0, 2))
	worker1 := New(s.DB, WithWorkerShard(1, 2))

	batch0, err := worker0.Dequeue(len(hosts))
	if err != nil {
		t.Fatalf("worker0 Dequeue: %v", err)
	}
	batch1, err := worker1.Dequeue(len(hosts))
	if err != nil {
		t.Fatalf("worker1 Dequeue: %v", err)
	}

	if len(batch0)+len(batch1) != len(hosts) {
		t.Fatalf("expected the two shards to partition all %d items between them, got %d + %d = %d",
			len(hosts), len(batch0), len(batch1), len(batch0)+len(batch1))
	}

	seen := make(map[string]int) // host -> which worker (0 or 1) claimed it
	for _, it := range batch0 {
		if it.HostShard%2 != 0 {
			t.Fatalf("worker0 (index 0 of 2) dequeued item with host_shard=%d, want even", it.HostShard)
		}
		seen[it.Host]++
	}
	for _, it := range batch1 {
		if it.HostShard%2 != 1 {
			t.Fatalf("worker1 (index 1 of 2) dequeued item with host_shard=%d, want odd", it.HostShard)
		}
		seen[it.Host]++
	}
	for _, host := range hosts {
		if seen[host] != 1 {
			t.Fatalf("host %s claimed %d times across workers, want exactly 1 (no overlap, no gap)", host, seen[host])
		}
	}

	// Requeue everything and verify a single unsharded worker (count=1)
	// dequeues all of it by itself.
	for _, it := range append(batch0, batch1...) {
		if err := seed.Requeue(it.ID); err != nil {
			t.Fatalf("Requeue(%d): %v", it.ID, err)
		}
	}
	solo := New(s.DB, WithWorkerShard(0, 1))
	batchAll, err := solo.Dequeue(len(hosts))
	if err != nil {
		t.Fatalf("solo Dequeue: %v", err)
	}
	if len(batchAll) != len(hosts) {
		t.Fatalf("count=1 worker dequeued %d items, want all %d", len(batchAll), len(hosts))
	}
}

// TestDequeueShardFilterPartitionsForeignHostByURL verifies that a foreign
// -host item — one whose Host is set to the parent catalog's host for
// attribution but whose URL points at a different host entirely, as entry
// follow-ups (catalog_fetch/artifact_fetch/registry_harvest) do — is only
// ever dequeued by the worker owning the URL's host shard, not the worker
// owning Host's shard. This is the regression test for the bug where
// HostShard was computed from Host and could route a foreign-host fetch to
// the wrong worker, breaking the one-worker-per-host guarantee.
func TestDequeueShardFilterPartitionsForeignHostByURL(t *testing.T) {
	s, err := store.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const (
		parentHost = "parent.example"
		urlHost    = "assets.cdn1.io"
	)
	parentShard := hostShard(parentHost) % 2
	urlShard := hostShard(urlHost) % 2
	if parentShard == urlShard {
		t.Fatalf("test fixture invalid: %s and %s fall in the same shard (%d); pick different hosts", parentHost, urlHost, parentShard)
	}

	seed := New(s.DB)
	item := &store.FrontierItem{
		Kind:     store.KindArtifactFetch,
		Host:     parentHost,
		URL:      "https://" + urlHost + "/a.json",
		DedupKey: "artifact:https://" + urlHost + "/a.json",
	}
	if _, err := seed.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ownerIdx := urlShard // the worker whose index matches the URL host's shard parity
	otherIdx := parentShard

	owner := New(s.DB, WithWorkerShard(ownerIdx, 2))
	other := New(s.DB, WithWorkerShard(otherIdx, 2))

	otherBatch, err := other.Dequeue(10)
	if err != nil {
		t.Fatalf("other Dequeue: %v", err)
	}
	if len(otherBatch) != 0 {
		t.Fatalf("worker owning parent host's shard dequeued %d items, want 0 (item belongs to URL host's shard)", len(otherBatch))
	}

	ownerBatch, err := owner.Dequeue(10)
	if err != nil {
		t.Fatalf("owner Dequeue: %v", err)
	}
	if len(ownerBatch) != 1 {
		t.Fatalf("worker owning URL host's shard dequeued %d items, want 1", len(ownerBatch))
	}
	if ownerBatch[0].DedupKey != item.DedupKey {
		t.Fatalf("owner dequeued unexpected item %+v", ownerBatch[0])
	}
}

// TestCountByHostKind verifies CountByHostKind counts only rows matching
// both host and kind, across every status.
func TestCountByHostKind(t *testing.T) {
	f := newTestFrontier(t)

	mk := func(host, kind, dedup string) *store.FrontierItem {
		return &store.FrontierItem{Kind: kind, Host: host, URL: "https://" + host + "/x", DedupKey: dedup}
	}

	if _, err := f.Enqueue(mk("budget.example", store.KindPageFetch, "pf:1")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := f.Enqueue(mk("budget.example", store.KindPageFetch, "pf:2")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Different kind, same host: must not be counted.
	if _, err := f.Enqueue(mk("budget.example", store.KindHostProbe, "hp:budget")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Same kind, different host: must not be counted.
	if _, err := f.Enqueue(mk("other.example", store.KindPageFetch, "pf:3")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	count, err := f.CountByHostKind("budget.example", store.KindPageFetch)
	if err != nil {
		t.Fatalf("CountByHostKind: %v", err)
	}
	if count != 2 {
		t.Fatalf("CountByHostKind(budget.example, page_fetch) = %d, want 2", count)
	}

	// A completed item still counts: the budget check must count every
	// status, not just pending/in_flight, since a page already fetched
	// still consumed its slot in the domain's budget.
	items, err := f.Dequeue(10)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	for _, it := range items {
		if it.Host == "budget.example" && it.Kind == store.KindPageFetch {
			if err := f.Complete(it.ID); err != nil {
				t.Fatalf("Complete: %v", err)
			}
		}
	}
	count, err = f.CountByHostKind("budget.example", store.KindPageFetch)
	if err != nil {
		t.Fatalf("CountByHostKind (after complete): %v", err)
	}
	if count != 2 {
		t.Fatalf("CountByHostKind after complete = %d, want 2 (all statuses counted)", count)
	}

	count, err = f.CountByHostKind("other.example", store.KindPageFetch)
	if err != nil {
		t.Fatalf("CountByHostKind: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountByHostKind(other.example, page_fetch) = %d, want 1", count)
	}
}
