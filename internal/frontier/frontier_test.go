package frontier

import (
	"errors"
	"sync"
	"testing"

	"github.com/helgesverre/ardvark/internal/store"
)

func newTestFrontier(t *testing.T) *Frontier {
	t.Helper()
	s, err := store.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewFromStore(s)
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
	if err := f.Complete(9999); err == nil {
		t.Fatal("expected error for missing item")
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
		if err := f.Fail(item.ID, cause, maxAttempts); err != nil {
			t.Fatalf("Fail (attempt %d): %v", i, err)
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
	if err := f.Fail(item.ID, cause, maxAttempts); err != nil {
		t.Fatalf("Fail (final): %v", err)
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
	if err := f.Fail(9999, errors.New("boom"), 3); err == nil {
		t.Fatal("expected error for missing item")
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
