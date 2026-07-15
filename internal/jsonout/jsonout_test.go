package jsonout

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/store"
)

// newTestStore opens an isolated sqlite store (file-backed in a t.TempDir so
// concurrent connections within a single test see the same data) with the
// schema migrated.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "ardvark.db")
	st, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

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

func TestGroupCount(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.UpsertDomain("a.com", store.DiscoverySourceSeed); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if _, err := st.UpsertDomain("b.com", store.DiscoverySourceSeed); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	dc, err := st.UpsertDomain("c.com", store.DiscoverySourceCTLog)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if err := st.RecordProbe(&store.Probe{DomainID: dc.ID, Method: store.ProbeMethodWellKnown, Outcome: store.ProbeOutcomeHit}); err != nil {
		t.Fatalf("RecordProbe: %v", err)
	}
	if err := st.UpdateDomainARDStatus(dc.ID, store.ARDStatusFoundValid); err != nil {
		t.Fatalf("UpdateDomainARDStatus: %v", err)
	}

	groups, err := GroupCount(st, "domains", "ard_status")
	if err != nil {
		t.Fatalf("GroupCount() error = %v", err)
	}

	counts := make(map[string]int64, len(groups))
	for _, g := range groups {
		counts[g.Key] = g.Count
	}

	if counts[store.ARDStatusUnprobed] != 2 {
		t.Errorf("unprobed count = %d, want 2", counts[store.ARDStatusUnprobed])
	}
	if counts[store.ARDStatusFoundValid] != 1 {
		t.Errorf("found_valid count = %d, want 1", counts[store.ARDStatusFoundValid])
	}
}

func TestStats(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.UpsertDomain("a.com", store.DiscoverySourceSeed); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	d, err := st.UpsertDomain("b.com", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if err := st.SaveCatalog(&store.Catalog{
		DomainID: d.ID, SourceURL: "https://b.com/.well-known/ai-catalog.json",
		FetchedAt: time.Now(), VerificationStatus: store.VerificationStatusValid,
		Entries: []store.CatalogEntry{{URN: "urn:air:b.com:tools:x", MediaType: "application/ai-skill+json"}},
	}, nil, nil); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}

	rep, err := Stats(st)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if rep.Domains.Total != 2 {
		t.Errorf("Domains.Total = %d, want 2", rep.Domains.Total)
	}
	if rep.Catalogs.Total != 1 {
		t.Errorf("Catalogs.Total = %d, want 1", rep.Catalogs.Total)
	}
	if rep.Entries.Total != 1 {
		t.Errorf("Entries.Total = %d, want 1", rep.Entries.Total)
	}
	if len(rep.Catalogs.ByVerdict) != 1 || rep.Catalogs.ByVerdict[0].Key != store.VerificationStatusValid {
		t.Errorf("Catalogs.ByVerdict = %+v, want single %q bucket", rep.Catalogs.ByVerdict, store.VerificationStatusValid)
	}
}

// SeedOne with a URL must enqueue both a page_fetch (to crawl the page) and a
// host_probe of the origin (so a URL whose page 404s still gets its
// well-known catalog checked). A bare domain enqueues only a host_probe.
func TestSeedOne(t *testing.T) {
	t.Run("url seeds page_fetch and host_probe", func(t *testing.T) {
		st := newTestStore(t)
		eng := newTestEngine(t, st)

		if _, err := SeedOne(eng, "https://example.com/some/page"); err != nil {
			t.Fatalf("SeedOne: %v", err)
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

		if _, err := SeedOne(eng, "example.com"); err != nil {
			t.Fatalf("SeedOne: %v", err)
		}
		if got := countFrontier(t, st, string(store.KindPageFetch)); got != 0 {
			t.Errorf("page_fetch items = %d, want 0", got)
		}
		if got := countFrontier(t, st, string(store.KindHostProbe)); got != 1 {
			t.Errorf("host_probe items = %d, want 1", got)
		}
	})
}

func TestSummarizeRun(t *testing.T) {
	st := newTestStore(t)

	run, err := st.CreateRun("{}")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := st.DB.Create(&store.FrontierItem{
		RunID: run.ID, Kind: store.KindPageFetch, URL: "https://a.com/", Host: "a.com",
		Status: store.FrontierStatusDone, DedupKey: "page_fetch:https://a.com/",
	}).Error; err != nil {
		t.Fatalf("seeding frontier item: %v", err)
	}
	if err := st.DB.Create(&store.FrontierItem{
		RunID: run.ID, Kind: store.KindHostProbe, Host: "a.com",
		Status: store.FrontierStatusDone, DedupKey: "host_probe:a.com",
	}).Error; err != nil {
		t.Fatalf("seeding frontier item: %v", err)
	}
	if err := st.DB.Create(&store.FrontierItem{
		RunID: run.ID, Kind: store.KindArtifactFetch, URL: "https://a.com/x", Host: "a.com",
		Status: store.FrontierStatusFailed, DedupKey: "artifact_fetch:https://a.com/x",
	}).Error; err != nil {
		t.Fatalf("seeding frontier item: %v", err)
	}

	domain, err := st.UpsertDomain("a.com", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if err := st.SaveCatalog(&store.Catalog{
		DomainID: domain.ID, SourceURL: "https://a.com/.well-known/ai-catalog.json",
		FetchedAt: time.Now(), VerificationStatus: store.VerificationStatusValid,
	}, nil, nil); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}

	pagesFetched, hostsProbed, catalogsFound, catalogsValid, errCount, err := SummarizeRun(st, run.StartedAt.Add(-time.Minute))
	if err != nil {
		t.Fatalf("SummarizeRun() error = %v", err)
	}
	if pagesFetched != 1 {
		t.Errorf("pagesFetched = %d, want 1", pagesFetched)
	}
	if hostsProbed != 1 {
		t.Errorf("hostsProbed = %d, want 1", hostsProbed)
	}
	if catalogsFound != 1 {
		t.Errorf("catalogsFound = %d, want 1", catalogsFound)
	}
	if catalogsValid != 1 {
		t.Errorf("catalogsValid = %d, want 1", catalogsValid)
	}
	if errCount != 1 {
		t.Errorf("errCount = %d, want 1", errCount)
	}
}

func TestWriteJSONLAndCSV(t *testing.T) {
	rows := []ExportRow{
		{Host: "example.com", URN: "urn:air:example.com:skills:x", DisplayName: "X", MediaType: "application/ai-skill+json"},
		{Host: "other.net", URN: "urn:air:other.net:skills:y", DisplayName: "Y, with comma", MediaType: "application/ai-skill+json"},
	}

	var jsonlBuf bytes.Buffer
	if err := WriteJSONL(&jsonlBuf, rows); err != nil {
		t.Fatalf("WriteJSONL() error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(jsonlBuf.String(), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("WriteJSONL() produced %d lines, want %d", len(lines), len(rows))
	}
	if !strings.Contains(lines[0], "example.com") {
		t.Fatalf("WriteJSONL() line 0 = %q, want to contain host", lines[0])
	}

	var csvBuf bytes.Buffer
	if err := WriteCSV(&csvBuf, rows); err != nil {
		t.Fatalf("WriteCSV() error = %v", err)
	}
	csvOut := csvBuf.String()
	if !strings.HasPrefix(csvOut, "host,catalog_source_url") {
		t.Fatalf("WriteCSV() missing header, got %q", csvOut)
	}
	if !strings.Contains(csvOut, `"Y, with comma"`) {
		t.Fatalf("WriteCSV() did not quote field containing a comma, got %q", csvOut)
	}
}

func TestBuildSeederUnknownSource(t *testing.T) {
	if _, _, _, err := BuildSeeder(t.Context(), config.Defaults(), "nope", 0, 0); err == nil {
		t.Fatal("BuildSeeder(unknown source): want error, got nil")
	}
}
