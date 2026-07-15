package main

import (
	"log/slog"
	"testing"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/store"
)

func newTestEngine(t *testing.T, st *store.Store) *crawler.Engine {
	t.Helper()
	cfg := config.Defaults()
	fr := frontier.New(st.DB)
	fc := fetch.New(cfg.Crawler)
	return crawler.New(cfg, st, fr, fc, slog.New(slog.DiscardHandler), crawler.Options{})
}

func countFrontier(t *testing.T, st *store.Store, kind string) int64 {
	t.Helper()
	var n int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", kind).Count(&n)
	return n
}

// seedOne with a URL must enqueue both a page_fetch (to crawl the page) and a
// host_probe of the origin (so a URL whose page 404s still gets its
// well-known catalog checked). A bare domain enqueues only a host_probe.
func TestSeedOne(t *testing.T) {
	t.Run("url seeds page_fetch and host_probe", func(t *testing.T) {
		st := newTestStore(t)
		eng := newTestEngine(t, st)

		if _, err := seedOne(eng, "https://example.com/some/page"); err != nil {
			t.Fatalf("seedOne: %v", err)
		}
		if got := countFrontier(t, st, string(store.KindPageFetch)); got != 1 {
			t.Errorf("page_fetch items = %d, want 1", got)
		}
		if got := countFrontier(t, st, string(store.KindHostProbe)); got != 1 {
			t.Errorf("host_probe items = %d, want 1", got)
		}
	})

	t.Run("bare domain seeds only host_probe", func(t *testing.T) {
		st := newTestStore(t)
		eng := newTestEngine(t, st)

		if _, err := seedOne(eng, "example.com"); err != nil {
			t.Fatalf("seedOne: %v", err)
		}
		if got := countFrontier(t, st, string(store.KindPageFetch)); got != 0 {
			t.Errorf("page_fetch items = %d, want 0", got)
		}
		if got := countFrontier(t, st, string(store.KindHostProbe)); got != 1 {
			t.Errorf("host_probe items = %d, want 1", got)
		}
	})
}
