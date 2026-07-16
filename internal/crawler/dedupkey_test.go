package crawler

import (
	"strings"
	"testing"

	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/store"
)

// TestDedupKey_FixedWidthForLongURL guards the invariant that motivates
// hashing: the dedup key is always exactly 64 lowercase hex chars regardless of
// how long the source URL is, so it can never overflow the size:64 uniqueIndex
// column (which would drop or silently collide items on mysql/postgres — see
// dedupKey's doc comment).
func TestDedupKey_FixedWidthForLongURL(t *testing.T) {
	longURL := "https://example.com/" + strings.Repeat("a", 4000)
	key := dedupKey(store.KindPageFetch, longURL)
	if len(key) != 64 {
		t.Fatalf("expected 64-char key, got %d (%q)", len(key), key)
	}
	if strings.ToLower(key) != key {
		t.Fatalf("expected lowercase hex key, got %q", key)
	}
	if strings.ContainsAny(key, "ghijklmnopqrstuvwxyz") {
		t.Fatalf("expected hex-only key, got %q", key)
	}
}

// TestDedupKey_DistinctForSharedPrefix is the collision case that the old
// raw-string key regressed on: two DISTINCT URLs sharing a >512-char common
// prefix must produce DISTINCT keys. Under the old scheme both keys truncated
// to the same 512 chars on MySQL non-strict, silently losing one URL.
func TestDedupKey_DistinctForSharedPrefix(t *testing.T) {
	prefix := "https://example.com/" + strings.Repeat("a", 600) + "/"
	a := dedupKey(store.KindPageFetch, prefix+"one")
	b := dedupKey(store.KindPageFetch, prefix+"two")
	if a == b {
		t.Fatalf("expected distinct keys for distinct long URLs, both were %q", a)
	}
}

// TestEnqueue_LongURLDedupsAgainstItself exercises the whole enqueue path with
// an over-512-char URL: the first enqueue succeeds (no overflow), and a second
// enqueue of the SAME URL is a silent no-op via the in-flight dedup path
// (added=false, err=nil), proving dedup still works for long URLs.
func TestEnqueue_LongURLDedupsAgainstItself(t *testing.T) {
	eng, st := newTestEngine(t, testCrawlerConfig())
	fr := frontier.New(st.DB)

	longURL := "https://example.com/" + strings.Repeat("a", 3000)
	host := "example.com"

	added, err := fr.Enqueue(eng.buildItem(store.KindPageFetch, longURL, host, 0, provenance{}))
	if err != nil {
		t.Fatalf("first enqueue of long URL: %v", err)
	}
	if !added {
		t.Fatal("expected first enqueue of long URL to insert a row")
	}

	added, err = fr.Enqueue(eng.buildItem(store.KindPageFetch, longURL, host, 0, provenance{}))
	if err != nil {
		t.Fatalf("second enqueue of long URL: %v", err)
	}
	if added {
		t.Fatal("expected second enqueue of identical long URL to dedup (no-op)")
	}

	var count int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindPageFetch).Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 frontier row for the long URL, got %d", count)
	}
}

// TestEnqueue_DistinctLongURLsBothInsert is the end-to-end collision case: two
// distinct long URLs sharing a >512-char prefix must BOTH enqueue as separate
// rows. Before hashing, the truncated keys collided and only one row survived.
func TestEnqueue_DistinctLongURLsBothInsert(t *testing.T) {
	eng, st := newTestEngine(t, testCrawlerConfig())
	fr := frontier.New(st.DB)

	prefix := "https://example.com/" + strings.Repeat("a", 700) + "/"
	host := "example.com"

	for _, suffix := range []string{"one", "two"} {
		added, err := fr.Enqueue(eng.buildItem(store.KindPageFetch, prefix+suffix, host, 0, provenance{}))
		if err != nil {
			t.Fatalf("enqueue %q: %v", suffix, err)
		}
		if !added {
			t.Fatalf("expected distinct long URL %q to insert its own row", suffix)
		}
	}

	var count int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindPageFetch).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 distinct frontier rows, got %d", count)
	}
}
