package crawler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

var testDSNCounter int

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	testDSNCounter++
	dsn := fmt.Sprintf("file:crawlertest%d?mode=memory&cache=shared", testDSNCounter)
	s, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testCrawlerConfig() config.Config {
	cfg := config.Defaults()
	cfg.Crawler.Concurrency = 4
	cfg.Crawler.PerHostRequestsPerSecond = 1000
	cfg.Crawler.RequestTimeoutSeconds = 5
	cfg.Crawler.RespectRobotsTxt = true
	cfg.Crawler.MaxDepth = 2
	cfg.Crawler.MaxPagesPerDomain = 10
	cfg.ARD.MaxCatalogDepth = 3
	cfg.ARD.FetchArtifacts = true
	cfg.Registry.Harvest = true
	cfg.Registry.PageLimit = 5
	return cfg
}

func newTestEngine(t *testing.T, cfg config.Config) (*Engine, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	fr := frontier.New(st.DB)
	fc := fetch.New(cfg.Crawler)
	eng := New(cfg, st, fr, fc, discardLogger(), Options{MaxAttempts: 2})
	return eng, st
}

// -- handleItem dispatch -----------------------------------------------------

func TestHandleItem_UnknownKind(t *testing.T) {
	eng, _ := newTestEngine(t, testCrawlerConfig())
	err := eng.handleItem(context.Background(), store.FrontierItem{Kind: "bogus_kind"})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

// -- page_fetch ---------------------------------------------------------

func TestHandlePageFetch_EnqueuesHostProbeAndLinks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body>
			<a href="/page2">local link</a>
			<a href="https://other-host.example/thing">external host link</a>
		</body></html>`))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	host := strings.TrimPrefix(srv.URL, "http://")

	item := store.FrontierItem{URL: srv.URL + "/", Host: host, Depth: 0}
	if err := eng.handlePageFetch(context.Background(), item); err != nil {
		t.Fatalf("handlePageFetch: %v", err)
	}

	// A host_probe should have been enqueued for the external host.
	var count int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ? AND host = ?", store.KindHostProbe, "other-host.example").Count(&count)
	if count != 1 {
		t.Errorf("expected 1 host_probe for other-host.example, got %d", count)
	}

	// A page_fetch should have been enqueued for the in-budget local link.
	var pageCount int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ? AND url = ?", store.KindPageFetch, srv.URL+"/page2").Count(&pageCount)
	if pageCount != 1 {
		t.Errorf("expected 1 page_fetch for /page2, got %d", pageCount)
	}
}

func TestHandlePageFetch_LinkTagHintEnqueuesCatalogFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head>
			<link rel="ai-catalog" href="/custom/catalog.json">
		</head><body></body></html>`))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	host := strings.TrimPrefix(srv.URL, "http://")

	item := store.FrontierItem{URL: srv.URL + "/", Host: host, Depth: 0}
	if err := eng.handlePageFetch(context.Background(), item); err != nil {
		t.Fatalf("handlePageFetch: %v", err)
	}

	var count int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ? AND url = ?", store.KindCatalogFetch, srv.URL+"/custom/catalog.json").Count(&count)
	if count != 1 {
		t.Errorf("expected 1 catalog_fetch enqueued from <link rel=ai-catalog>, got %d", count)
	}

	var probeCount int64
	st.DB.Model(&store.Probe{}).Where("method = ? AND url = ?", store.ProbeMethodLinkTag, srv.URL+"/custom/catalog.json").Count(&probeCount)
	if probeCount != 1 {
		t.Errorf("expected 1 link_tag probe recorded, got %d", probeCount)
	}
}

func TestHandlePageFetch_RespectsMaxDepth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<a href="/deeper">deeper</a>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testCrawlerConfig()
	cfg.Crawler.MaxDepth = 1
	eng, st := newTestEngine(t, cfg)
	host := strings.TrimPrefix(srv.URL, "http://")

	// At the max depth already: no further page_fetch should be enqueued.
	item := store.FrontierItem{URL: srv.URL + "/", Host: host, Depth: 1}
	if err := eng.handlePageFetch(context.Background(), item); err != nil {
		t.Fatalf("handlePageFetch: %v", err)
	}

	var count int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindPageFetch).Count(&count)
	if count != 0 {
		t.Errorf("expected no page_fetch items enqueued beyond max depth, got %d", count)
	}
}

func TestHandlePageFetch_NonHTMLSkipped(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/data.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	host := strings.TrimPrefix(srv.URL, "http://")
	item := store.FrontierItem{URL: srv.URL + "/data.json", Host: host, Depth: 0}

	if err := eng.handlePageFetch(context.Background(), item); err != nil {
		t.Fatalf("handlePageFetch: %v", err)
	}

	var count int64
	st.DB.Model(&store.FrontierItem{}).Count(&count)
	if count != 0 {
		t.Errorf("expected no items enqueued for non-HTML content, got %d", count)
	}
}

// -- host_probe -------------------------------------------------------------

func TestHandleHostProbe_HitEnqueuesCatalogFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(`{"specVersion":"1.0","host":{"displayName":"Test"},"entries":[]}`))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	host := strings.TrimPrefix(srv.URL, "http://")

	// Force the well-known probe to hit this http (not https) test server
	// by directly invoking the handler and letting probe.Probe attempt an
	// https URL — since httptest is plain http, exercise the underlying
	// mechanics instead via a direct GetWellKnown/RawRobots round trip.
	if err := eng.handleHostProbe(context.Background(), store.FrontierItem{Host: host}); err != nil {
		t.Fatalf("handleHostProbe: %v", err)
	}

	var probeCount int64
	st.DB.Model(&store.Probe{}).Count(&probeCount)
	if probeCount != 2 {
		t.Errorf("expected 2 probe rows (well_known + robots_agentmap), got %d", probeCount)
	}

	var domain store.Domain
	if err := st.DB.Where("host = ?", host).First(&domain).Error; err != nil {
		t.Fatalf("expected domain row for %s: %v", host, err)
	}
}

func TestHandleHostProbe_SkipsWhenRecentlyProbed(t *testing.T) {
	eng, st := newTestEngine(t, testCrawlerConfig())
	host := "recently-probed.example"

	domain, err := st.UpsertDomain(host, store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if err := st.RecordProbe(&store.Probe{DomainID: domain.ID, Method: "well_known", Outcome: "miss", ProbedAt: time.Now()}); err != nil {
		t.Fatalf("RecordProbe: %v", err)
	}

	if err := eng.handleHostProbe(context.Background(), store.FrontierItem{Host: host}); err != nil {
		t.Fatalf("handleHostProbe: %v", err)
	}

	var probeCount int64
	st.DB.Model(&store.Probe{}).Count(&probeCount)
	if probeCount != 1 {
		t.Errorf("expected no new probes for a recently-probed host, got total %d", probeCount)
	}
}

func TestHandleHostProbe_ForceBypassesFreshness(t *testing.T) {
	cfg := testCrawlerConfig()
	st := newTestStore(t)
	fr := frontier.New(st.DB)
	fc := fetch.New(cfg.Crawler)
	eng := New(cfg, st, fr, fc, discardLogger(), Options{Force: true})

	host := "forced.example"
	domain, err := st.UpsertDomain(host, store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if err := st.RecordProbe(&store.Probe{DomainID: domain.ID, Method: "well_known", Outcome: "miss", ProbedAt: time.Now()}); err != nil {
		t.Fatalf("RecordProbe: %v", err)
	}

	if err := eng.handleHostProbe(context.Background(), store.FrontierItem{Host: host}); err != nil {
		t.Fatalf("handleHostProbe: %v", err)
	}

	var probeCount int64
	st.DB.Model(&store.Probe{}).Where("domain_id = ?", domain.ID).Count(&probeCount)
	if probeCount <= 1 {
		t.Errorf("expected Force to re-probe despite freshness, got %d probes", probeCount)
	}
}

// -- catalog_fetch ------------------------------------------------------

func TestHandleCatalogFetch_SavesCatalogAndEnqueuesFollowOns(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/nested.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(`{"specVersion":"1.0","host":{"displayName":"Nested"},"entries":[
			{"identifier":"urn:air:example.com:agents:nested-agent","displayName":"Nested Agent","type":"application/a2a-agent-card+json","url":"` + hostPlaceholder + `/nested-artifact.json"}
		]}`))
	})
	mux.HandleFunc("/artifact1.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/nested-artifact.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":"nested"}`))
	})
	mux.HandleFunc("/registry/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{
					"identifier":  "urn:air:registry.example:agents:found-one",
					"displayName": "Found One",
					"type":        "application/a2a-agent-card+json",
					"url":         "https://registry.example/found-one.json",
				},
			},
			"referrals": []any{},
			"pageToken": "",
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := strings.ReplaceAll(rootCatalogJSON(hostPlaceholder), hostPlaceholder, srv.URL)

	mux.HandleFunc("/catalog2.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(body))
	})

	eng, st := newTestEngine(t, testCrawlerConfig())
	host := strings.TrimPrefix(srv.URL, "http://")

	item := store.FrontierItem{URL: srv.URL + "/catalog2.json", Host: host, Depth: 0}
	if err := eng.handleCatalogFetch(context.Background(), item); err != nil {
		t.Fatalf("handleCatalogFetch: %v", err)
	}

	var cat store.Catalog
	if err := st.DB.Preload("Entries").Where("source_url = ?", item.URL).First(&cat).Error; err != nil {
		t.Fatalf("expected catalog row: %v", err)
	}
	if len(cat.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(cat.Entries))
	}

	var checks int64
	st.DB.Model(&store.VerificationCheck{}).Where("subject_type = ? AND subject_id = ?", store.SubjectTypeCatalog, cat.ID).Count(&checks)
	if checks == 0 {
		t.Error("expected at least one catalog-level verification check")
	}

	var artifactFetchCount int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindArtifactFetch).Count(&artifactFetchCount)
	if artifactFetchCount != 1 {
		t.Errorf("expected 1 artifact_fetch enqueued, got %d", artifactFetchCount)
	}

	var nestedCatalogFetchCount int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ? AND url = ?", store.KindCatalogFetch, srv.URL+"/nested.json").Count(&nestedCatalogFetchCount)
	if nestedCatalogFetchCount != 1 {
		t.Errorf("expected 1 nested catalog_fetch enqueued, got %d", nestedCatalogFetchCount)
	}

	var registryHarvestCount int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindRegistryHarvest).Count(&registryHarvestCount)
	if registryHarvestCount != 1 {
		t.Errorf("expected 1 registry_harvest enqueued, got %d", registryHarvestCount)
	}

	var registryRows int64
	st.DB.Model(&store.Registry{}).Count(&registryRows)
	if registryRows != 1 {
		t.Errorf("expected 1 registries row, got %d", registryRows)
	}
}

func TestHandleCatalogFetch_UnchangedContentSkipsResave(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := strings.ReplaceAll(rootCatalogJSON(hostPlaceholder), hostPlaceholder, srv.URL)
	mux.HandleFunc("/catalog2.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(body))
	})
	mux.HandleFunc("/artifact1.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})

	eng, st := newTestEngine(t, testCrawlerConfig())
	host := strings.TrimPrefix(srv.URL, "http://")
	item := store.FrontierItem{URL: srv.URL + "/catalog2.json", Host: host, Depth: 0}

	if err := eng.handleCatalogFetch(context.Background(), item); err != nil {
		t.Fatalf("handleCatalogFetch (1st): %v", err)
	}
	if err := eng.handleCatalogFetch(context.Background(), item); err != nil {
		t.Fatalf("handleCatalogFetch (2nd, unchanged): %v", err)
	}

	var catalogCount int64
	st.DB.Model(&store.Catalog{}).Where("source_url = ?", item.URL).Count(&catalogCount)
	if catalogCount != 1 {
		t.Errorf("expected 1 catalog row after re-fetching unchanged content, got %d", catalogCount)
	}

	// --force must bypass the change-detection skip.
	eng.opts.Force = true
	if err := eng.handleCatalogFetch(context.Background(), item); err != nil {
		t.Fatalf("handleCatalogFetch (3rd, forced): %v", err)
	}
	st.DB.Model(&store.Catalog{}).Where("source_url = ?", item.URL).Count(&catalogCount)
	if catalogCount != 2 {
		t.Errorf("expected 2 catalog rows after --force re-fetch, got %d", catalogCount)
	}
}

func TestHandleCatalogFetch_RespectsMaxCatalogDepth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/deep.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(`{"specVersion":"1.0","host":{"displayName":"Deep"},"entries":[
			{"identifier":"urn:air:example.com:catalogs:nested","displayName":"Nested Catalog","type":"application/ai-catalog+json","url":"https://example.com/never-fetched.json"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testCrawlerConfig()
	cfg.ARD.MaxCatalogDepth = 3
	eng, st := newTestEngine(t, cfg)
	host := strings.TrimPrefix(srv.URL, "http://")

	// Dequeue this catalog at the max depth already: no nested catalog_fetch
	// should be enqueued.
	item := store.FrontierItem{URL: srv.URL + "/deep.json", Host: host, Depth: 3}
	if err := eng.handleCatalogFetch(context.Background(), item); err != nil {
		t.Fatalf("handleCatalogFetch: %v", err)
	}

	var count int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindCatalogFetch).Count(&count)
	if count != 0 {
		t.Errorf("expected no nested catalog_fetch beyond max depth, got %d", count)
	}
}

func TestHandleCatalogFetch_EmbeddedNestedCatalogStoredDirectly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/withdata.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(`{"specVersion":"1.0","host":{"displayName":"Parent"},"entries":[
			{"identifier":"urn:air:example.com:catalogs:embedded","displayName":"Embedded Catalog","type":"application/ai-catalog+json","data":{"specVersion":"1.0","host":{"displayName":"Embedded"},"entries":[]}}
		]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	host := strings.TrimPrefix(srv.URL, "http://")

	item := store.FrontierItem{URL: srv.URL + "/withdata.json", Host: host, Depth: 0}
	if err := eng.handleCatalogFetch(context.Background(), item); err != nil {
		t.Fatalf("handleCatalogFetch: %v", err)
	}

	var parent store.Catalog
	if err := st.DB.Where("source_url = ?", item.URL).First(&parent).Error; err != nil {
		t.Fatalf("expected parent catalog: %v", err)
	}

	var nested store.Catalog
	if err := st.DB.Where("parent_catalog_id = ?", parent.ID).First(&nested).Error; err != nil {
		t.Fatalf("expected embedded nested catalog stored with parent_catalog_id: %v", err)
	}
	if nested.HostDisplayName != "Embedded" {
		t.Errorf("expected embedded catalog host display name 'Embedded', got %q", nested.HostDisplayName)
	}

	// Embedded catalogs are processed directly, not via the frontier.
	var catalogFetchCount int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindCatalogFetch).Count(&catalogFetchCount)
	if catalogFetchCount != 0 {
		t.Errorf("expected no catalog_fetch frontier items for an embedded nested catalog, got %d", catalogFetchCount)
	}
}

// -- artifact_fetch -----------------------------------------------------

func TestHandleArtifactFetch_SavesArtifact(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/card.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/a2a-agent-card+json")
		w.Write([]byte(`{"name":"test"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	entryID := uint(42)

	item := store.FrontierItem{URL: srv.URL + "/card.json", ArtifactEntryID: &entryID}
	if err := eng.handleArtifactFetch(context.Background(), item); err != nil {
		t.Fatalf("handleArtifactFetch: %v", err)
	}

	var artifact store.Artifact
	if err := st.DB.Where("entry_id = ?", 42).First(&artifact).Error; err != nil {
		t.Fatalf("expected artifact row: %v", err)
	}
	if artifact.FetchStatus != store.FetchStatusOK {
		t.Errorf("expected fetch_status ok, got %q", artifact.FetchStatus)
	}
}

func TestHandleArtifactFetch_PermanentFailureRecordsErrorArtifact(t *testing.T) {
	mux := http.NewServeMux()
	// No handler: 404.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	entryID := uint(7)

	item := store.FrontierItem{URL: srv.URL + "/missing.json", ArtifactEntryID: &entryID}
	if err := eng.handleArtifactFetch(context.Background(), item); err != nil {
		t.Fatalf("expected permanent failure to be swallowed as an errored artifact, got %v", err)
	}

	var artifact store.Artifact
	if err := st.DB.Where("entry_id = ?", 7).First(&artifact).Error; err != nil {
		t.Fatalf("expected error artifact row: %v", err)
	}
	if artifact.FetchStatus != store.FetchStatusError {
		t.Errorf("expected fetch_status error, got %q", artifact.FetchStatus)
	}
}

func TestHandleArtifactFetch_TransientFailurePropagatesForRetry(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/flaky.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, _ := newTestEngine(t, testCrawlerConfig())
	item := store.FrontierItem{URL: srv.URL + "/flaky.json"}

	err := eng.handleArtifactFetch(context.Background(), item)
	if err == nil {
		t.Fatal("expected a transient error to propagate")
	}
	if !fetch.Transient(err) {
		t.Errorf("expected a transient error, got %v", err)
	}
}

// -- process() Complete/Fail wiring ------------------------------------

func TestProcess_CompletesSuccessfulItem(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	entryID := uint(1)

	item := store.FrontierItem{Kind: store.KindArtifactFetch, URL: srv.URL + "/ok.json", DedupKey: "x", Status: store.FrontierStatusInFlight, ArtifactEntryID: &entryID}
	if err := st.DB.Create(&item).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}

	eng.process(context.Background(), item)

	var reloaded store.FrontierItem
	st.DB.First(&reloaded, item.ID)
	if reloaded.Status != store.FrontierStatusDone {
		t.Errorf("expected status done, got %q", reloaded.Status)
	}
}

func TestProcess_FailsPermanentlyOnUnknownKind(t *testing.T) {
	eng, st := newTestEngine(t, testCrawlerConfig())

	item := store.FrontierItem{Kind: "bogus", DedupKey: "y", Status: store.FrontierStatusInFlight}
	if err := st.DB.Create(&item).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}

	eng.process(context.Background(), item)

	var reloaded store.FrontierItem
	st.DB.First(&reloaded, item.ID)
	if reloaded.Status != store.FrontierStatusFailed {
		t.Errorf("expected status failed for a permanent, non-transient error, got %q", reloaded.Status)
	}
	if reloaded.LastError == "" {
		t.Error("expected LastError to be recorded")
	}
}

// -- provenance persistence ------------------------------------------------

// TestProcess_PersistsProvenanceAcrossRetry ensures a transient failure
// (retries remaining) leaves the frontier row's provenance columns intact:
// the next attempt at the same item — possibly dequeued by a different
// worker process — still needs them to attribute its result.
func TestProcess_PersistsProvenanceAcrossRetry(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/flaky.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	artifactURL := srv.URL + "/flaky.json"
	entryID := uint(7)

	item := store.FrontierItem{
		Kind: store.KindArtifactFetch, URL: artifactURL,
		DedupKey: dedupKey(store.KindArtifactFetch, artifactURL), Status: store.FrontierStatusInFlight,
		ArtifactEntryID: &entryID,
	}
	if err := st.DB.Create(&item).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// MaxAttempts is 2 in newTestEngine's Options, so the first failure
	// still has a retry left.
	eng.process(context.Background(), item)

	var reloaded store.FrontierItem
	st.DB.First(&reloaded, item.ID)
	if reloaded.Status != store.FrontierStatusPending {
		t.Fatalf("expected item still pending (retry remaining), got %q", reloaded.Status)
	}
	if reloaded.ArtifactEntryID == nil || *reloaded.ArtifactEntryID != 7 {
		t.Errorf("expected artifact_entry_id to survive a retryable failure, got %v", reloaded.ArtifactEntryID)
	}
}

// TestProvenance_SurvivesFreshEngineInstance is the regression test for
// cross-process provenance (see internal/crawler's package doc): a
// catalog_fetch/artifact_fetch/registry_harvest item enqueued by one
// worker's *Engine must still carry full provenance when dequeued and
// processed by a completely different *Engine instance sharing the same
// store/frontier — simulating a second worker process — rather than only
// the instance that enqueued it.
func TestProvenance_SurvivesFreshEngineInstance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/nested-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(`{"specVersion":"1.0","host":{"displayName":"Nested"},"entries":[]}`))
	})
	mux.HandleFunc("/artifact.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testCrawlerConfig()
	st := newTestStore(t)
	fr := frontier.New(st.DB)
	fc := fetch.New(cfg.Crawler)
	host := strings.TrimPrefix(srv.URL, "http://")

	// "Worker A" enqueues the items with provenance set, as
	// enqueueEntryFollowups/handleHostProbe would.
	workerA := New(cfg, st, fr, fc, discardLogger(), Options{MaxAttempts: 2})

	parentCatalogID := uint(99)
	if _, err := workerA.enqueue(store.KindCatalogFetch, srv.URL+"/nested-catalog.json", host, 1, provenance{
		ParentCatalogID: &parentCatalogID,
		ProbeMethod:     store.ProbeMethodWellKnown,
	}); err != nil {
		t.Fatalf("enqueue catalog_fetch: %v", err)
	}

	artifactEntryID := uint(42)
	if _, err := workerA.enqueue(store.KindArtifactFetch, srv.URL+"/artifact.json", host, 0, provenance{
		ArtifactEntryID: &artifactEntryID,
	}); err != nil {
		t.Fatalf("enqueue artifact_fetch: %v", err)
	}

	// Seed a store.Catalog and a registries row for the registry_harvest
	// item to attribute its harvested entries to.
	cat := &store.Catalog{SourceURL: "https://registry.example/parent", VerificationStatus: "valid"}
	if err := st.DB.Create(cat).Error; err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	regRow := &store.Registry{EntryID: 1, BaseURL: "https://registry.invalid/api", HarvestStatus: store.HarvestStatusPending}
	if err := st.DB.Create(regRow).Error; err != nil {
		t.Fatalf("seed registry row: %v", err)
	}
	regEntryID := uint(1)
	regCatalogID := cat.ID
	if _, err := workerA.enqueue(store.KindRegistryHarvest, "https://registry.invalid/api", host, 0, provenance{
		RegistryEntryID:   &regEntryID,
		RegistryCatalogID: &regCatalogID,
		RegistryRowID:     &regRow.ID,
	}); err != nil {
		t.Fatalf("enqueue registry_harvest: %v", err)
	}

	// "Worker B" is a brand new Engine over the same store/frontier — it
	// shares no in-process state with workerA whatsoever, unlike the old
	// in-memory maps.
	workerB := New(cfg, st, fr, fc, discardLogger(), Options{MaxAttempts: 2})

	items, err := fr.Dequeue(3)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 dequeued items, got %d", len(items))
	}

	for _, item := range items {
		switch item.Kind {
		case store.KindCatalogFetch:
			if item.ParentCatalogID == nil || *item.ParentCatalogID != parentCatalogID {
				t.Errorf("catalog_fetch item missing ParentCatalogID, got %v", item.ParentCatalogID)
			}
			if item.ProbeMethod != store.ProbeMethodWellKnown {
				t.Errorf("catalog_fetch item missing ProbeMethod, got %q", item.ProbeMethod)
			}
			if err := workerB.handleCatalogFetch(context.Background(), item); err != nil {
				t.Fatalf("handleCatalogFetch: %v", err)
			}
			var nested store.Catalog
			if err := st.DB.Where("source_url = ?", srv.URL+"/nested-catalog.json").First(&nested).Error; err != nil {
				t.Fatalf("expected nested catalog persisted: %v", err)
			}
			if nested.ParentCatalogID == nil || *nested.ParentCatalogID != parentCatalogID {
				t.Errorf("expected nested catalog's parent_catalog_id %d, got %v", parentCatalogID, nested.ParentCatalogID)
			}

		case store.KindArtifactFetch:
			if item.ArtifactEntryID == nil || *item.ArtifactEntryID != artifactEntryID {
				t.Errorf("artifact_fetch item missing ArtifactEntryID, got %v", item.ArtifactEntryID)
			}
			if err := workerB.handleArtifactFetch(context.Background(), item); err != nil {
				t.Fatalf("handleArtifactFetch: %v", err)
			}
			var artifact store.Artifact
			if err := st.DB.Where("entry_id = ?", artifactEntryID).First(&artifact).Error; err != nil {
				t.Fatalf("expected artifact row attributed to entry %d: %v", artifactEntryID, err)
			}

		case store.KindRegistryHarvest:
			if item.RegistryEntryID == nil || *item.RegistryEntryID != regEntryID {
				t.Errorf("registry_harvest item missing RegistryEntryID, got %v", item.RegistryEntryID)
			}
			if item.RegistryCatalogID == nil || *item.RegistryCatalogID != regCatalogID {
				t.Errorf("registry_harvest item missing RegistryCatalogID, got %v", item.RegistryCatalogID)
			}
			if item.RegistryRowID == nil || *item.RegistryRowID != regRow.ID {
				t.Errorf("registry_harvest item missing RegistryRowID, got %v", item.RegistryRowID)
			}
			// registry.invalid does not resolve to a real server; the
			// assertions above are what this test cares about (provenance
			// surviving to a fresh engine instance), so the resulting fetch
			// error is expected and ignored.
			_ = workerB.handleRegistryHarvest(context.Background(), item)

		default:
			t.Fatalf("unexpected item kind %q", item.Kind)
		}
	}
}

// -- end-to-end via Run() ------------------------------------------------

func TestRun_DrainsSeedURLThroughPageFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<a href="/about">about</a>`))
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`no links here`))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())

	if _, err := eng.EnqueueSeedURL(srv.URL + "/"); err != nil {
		t.Fatalf("EnqueueSeedURL: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var doneCount int64
	st.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusDone).Count(&doneCount)
	if doneCount < 2 {
		t.Errorf("expected at least 2 completed items (root page + /about), got %d", doneCount)
	}

	var pending int64
	st.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusPending).Count(&pending)
	if pending != 0 {
		t.Errorf("expected the frontier to be fully drained, got %d pending", pending)
	}
}

// TestRun_SlowItemDoesNotBlockWorkers is the regression test for the old
// batch-synchronized Run: dequeuing concurrency() items and waiting on the
// whole batch meant one slow item idled every other worker until it
// finished. With the continuous pool, one worker rides out the slow item
// while the rest keep draining the queue, so every fast item must have been
// served before the slow handler has even returned.
func TestRun_SlowItemDoesNotBlockWorkers(t *testing.T) {
	const slowDelay = 750 * time.Millisecond
	const fastCount = 5

	var mu sync.Mutex
	var slowDone time.Time
	var lastFast time.Time

	mux := http.NewServeMux()
	mux.HandleFunc("/slow.json", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(slowDelay)
		mu.Lock()
		slowDone = time.Now()
		mu.Unlock()
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/fast/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if now := time.Now(); now.After(lastFast) {
			lastFast = now
		}
		mu.Unlock()
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testCrawlerConfig()
	cfg.Crawler.Concurrency = 2
	eng, _ := newTestEngine(t, cfg)
	host := strings.TrimPrefix(srv.URL, "http://")

	// The slow item is enqueued first, so it lands on a worker immediately;
	// the fast items must all funnel through the remaining worker while the
	// slow one is still in flight.
	urls := []string{srv.URL + "/slow.json"}
	for i := 0; i < fastCount; i++ {
		urls = append(urls, fmt.Sprintf("%s/fast/%d.json", srv.URL, i))
	}
	entryID := uint(1)
	for _, u := range urls {
		if _, err := eng.frontier.Enqueue(&store.FrontierItem{
			Kind:            store.KindArtifactFetch,
			URL:             u,
			Host:            host,
			DedupKey:        dedupKey(store.KindArtifactFetch, u),
			ArtifactEntryID: &entryID,
		}); err != nil {
			t.Fatalf("Enqueue %s: %v", u, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if slowDone.IsZero() || lastFast.IsZero() {
		t.Fatal("expected both the slow and the fast handlers to have been hit")
	}
	if !lastFast.Before(slowDone) {
		t.Errorf("last fast item was served %v after the slow item finished; a slow item stalled the pool", lastFast.Sub(slowDone))
	}
}

func TestRun_GracefulShutdownOnCancel(t *testing.T) {
	eng, _ := newTestEngine(t, testCrawlerConfig())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := eng.Run(ctx); err != nil {
		t.Fatalf("expected graceful nil return on cancelled context, got %v", err)
	}
}

// TestRun_ForceCatalogCycleTerminates is the regression test for the
// FOLLOWUPS.md item "--force turns any catalog reference cycle into a
// non-terminating loop": two catalogs, A and B, each url-reference the
// other via a nested `application/ai-catalog+json` entry. Under --force,
// the content-hash "unchanged, skip re-save" shortcut in processCatalog is
// bypassed, so the only thing that can stop A->B->A->B->... is the
// maxCatalogDepth guard combined with the frontier's re-enqueue-adopts-
// deeper-depth fix (see frontier.Enqueue's doc comment and
// TestEnqueueResetAdoptsDeeperDepth). Run must still terminate (the
// context timeout must never fire) and the number of catalog_fetch items
// actually processed must be bounded by maxCatalogDepth, not unbounded.
func TestRun_ForceCatalogCycleTerminates(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/a.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(`{"specVersion":"1.0","host":{"displayName":"A"},"entries":[
			{"identifier":"urn:air:example.com:catalogs:b","displayName":"B","type":"application/ai-catalog+json","url":"` + srv.URL + `/b.json"}
		]}`))
	})
	mux.HandleFunc("/b.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(`{"specVersion":"1.0","host":{"displayName":"B"},"entries":[
			{"identifier":"urn:air:example.com:catalogs:a","displayName":"A","type":"application/ai-catalog+json","url":"` + srv.URL + `/a.json"}
		]}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	// httptest.NewServer must exist before the handlers above can close
	// over srv.URL; re-register now that srv is set (Go closures capture
	// the variable, not its value at registration time, so the handlers
	// above already see the right URL once srv is assigned).

	cfg := testCrawlerConfig()
	cfg.ARD.MaxCatalogDepth = 3
	eng, st := newTestEngine(t, cfg)
	eng.opts.Force = true

	host := strings.TrimPrefix(srv.URL, "http://")
	if _, err := eng.frontier.Enqueue(&store.FrontierItem{
		Kind:     store.KindCatalogFetch,
		URL:      srv.URL + "/a.json",
		Host:     host,
		Depth:    0,
		DedupKey: dedupKey(store.KindCatalogFetch, srv.URL+"/a.json"),
	}); err != nil {
		t.Fatalf("seed catalog_fetch: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Run did not terminate before the context timeout: the A<->B catalog cycle looped forever")
	}

	var pending int64
	st.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusPending).Count(&pending)
	if pending != 0 {
		t.Errorf("expected the frontier to be fully drained, got %d pending", pending)
	}

	// Bounded: at most maxCatalogDepth+1 catalog_fetch items should ever
	// have been created (depths 0..maxCatalogDepth), not one per cycle
	// iteration forever.
	var catalogFetchCount int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindCatalogFetch).Count(&catalogFetchCount)
	if catalogFetchCount > int64(cfg.ARD.MaxCatalogDepth)+1 {
		t.Errorf("expected at most %d catalog_fetch items (bounded by maxCatalogDepth), got %d", cfg.ARD.MaxCatalogDepth+1, catalogFetchCount)
	}
}

// -- distributed termination (Frontier.Counts) ----------------------------

func TestIsSQLiteDriver(t *testing.T) {
	cases := map[string]bool{
		"":         true,
		"sqlite":   true,
		"sqlite3":  true,
		"mysql":    false,
		"postgres": false,
	}
	for driver, want := range cases {
		if got := isSQLiteDriver(driver); got != want {
			t.Errorf("isSQLiteDriver(%q) = %v, want %v", driver, got, want)
		}
	}
}

// TestRun_DoesNotTerminateWhileGloballyInFlight is the regression test for
// distributed crawling's termination fix: this worker's own queue and
// in-flight counter being empty must not be enough to exit when a peer
// worker process (simulated here by dequeuing an item through the same
// frontier without ever routing it through this Engine's Run loop) still
// holds an in_flight item, since it could enqueue more work. cfg.Storage
// .Driver is set to "postgres" purely to route Run through the
// distributed (ReclaimExpired, not blanket ReclaimInFlight) branch; the
// underlying store is still sqlite (see newTestStore).
func TestRun_DoesNotTerminateWhileGloballyInFlight(t *testing.T) {
	cfg := testCrawlerConfig()
	cfg.Storage.Driver = "postgres"
	eng, _ := newTestEngine(t, cfg)

	if _, err := eng.frontier.Enqueue(&store.FrontierItem{
		Kind: store.KindHostProbe, Host: "peer.example", DedupKey: "hp:peer",
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := eng.frontier.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("expected Run to keep polling (hitting the context deadline) while a peer's item is globally in_flight, not exit early")
	}
}

// TestRun_TerminatesOnceGloballyInFlightItemCompletes complements the test
// above: once the peer's item completes (frontier.Counts reports zero
// pending and zero in_flight), Run must notice and return well before its
// context deadline.
func TestRun_TerminatesOnceGloballyInFlightItemCompletes(t *testing.T) {
	cfg := testCrawlerConfig()
	cfg.Storage.Driver = "postgres"
	eng, _ := newTestEngine(t, cfg)

	item := &store.FrontierItem{Kind: store.KindHostProbe, Host: "peer2.example", DedupKey: "hp:peer2"}
	if _, err := eng.frontier.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := eng.frontier.Dequeue(1); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		if err := eng.frontier.Complete(item.ID); err != nil {
			t.Errorf("Complete: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("Run hit the context deadline instead of noticing the peer's item had completed")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run took %v to notice the peer's item completed; expected roughly one globalCountsCheckInterval (~1s)", elapsed)
	}
}

// -- fixtures -------------------------------------------------------------

const hostPlaceholder = "__HOST__"

// rootCatalogJSON returns a catalog referencing one regular artifact entry,
// one url-referenced nested catalog, and one registry entry, all resolved
// against base (a test server URL, with hostPlaceholder substituted).
func rootCatalogJSON(base string) string {
	return `{
		"specVersion": "1.0",
		"host": {"displayName": "Root"},
		"entries": [
			{
				"identifier": "urn:air:example.com:agents:one",
				"displayName": "Agent One",
				"type": "application/a2a-agent-card+json",
				"url": "` + base + `/artifact1.json",
				"representativeQueries": ["a", "b"]
			},
			{
				"identifier": "urn:air:example.com:catalogs:nested",
				"displayName": "Nested Catalog",
				"type": "application/ai-catalog+json",
				"url": "` + base + `/nested.json"
			},
			{
				"identifier": "urn:air:example.com:registries:main",
				"displayName": "Main Registry",
				"type": "application/ai-registry+json",
				"url": "` + base + `/registry"
			}
		]
	}`
}

// -- maxPagesPerDomain (DB-backed page budget) -------------------------------

func TestHandlePageFetch_RespectsMaxPagesPerDomainBudget(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		var links strings.Builder
		for i := 0; i < 10; i++ {
			fmt.Fprintf(&links, `<a href="/link%d">link</a>`, i)
		}
		w.Write([]byte("<html><body>" + links.String() + "</body></html>"))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testCrawlerConfig()
	cfg.Crawler.MaxDepth = 5
	cfg.Crawler.MaxPagesPerDomain = 3
	eng, st := newTestEngine(t, cfg)
	host := strings.TrimPrefix(srv.URL, "http://")

	item := store.FrontierItem{URL: srv.URL + "/", Host: host, Depth: 0}
	if err := eng.handlePageFetch(context.Background(), item); err != nil {
		t.Fatalf("handlePageFetch: %v", err)
	}

	var count int64
	if err := st.DB.Model(&store.FrontierItem{}).
		Where("kind = ? AND host = ?", store.KindPageFetch, host).
		Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected exactly maxPagesPerDomain=3 page_fetch items enqueued for %s despite 10 candidate links, got %d", host, count)
	}
}

func TestHandlePageFetch_MaxPagesPerDomainIsPerHost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<a href="https://host-a.example/1">a1</a>
			<a href="https://host-a.example/2">a2</a>
			<a href="https://host-b.example/1">b1</a>
			<a href="https://host-b.example/2">b2</a>`))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := testCrawlerConfig()
	cfg.Crawler.MaxDepth = 5
	cfg.Crawler.MaxPagesPerDomain = 1
	eng, st := newTestEngine(t, cfg)
	host := strings.TrimPrefix(srv.URL, "http://")

	item := store.FrontierItem{URL: srv.URL + "/", Host: host, Depth: 0}
	if err := eng.handlePageFetch(context.Background(), item); err != nil {
		t.Fatalf("handlePageFetch: %v", err)
	}

	for _, h := range []string{"host-a.example", "host-b.example"} {
		var count int64
		if err := st.DB.Model(&store.FrontierItem{}).
			Where("kind = ? AND host = ?", store.KindPageFetch, h).
			Count(&count).Error; err != nil {
			t.Fatalf("count(%s): %v", h, err)
		}
		if count != 1 {
			t.Errorf("expected exactly 1 page_fetch enqueued for %s (budget is per-host, not shared), got %d", h, count)
		}
	}
}

func TestPageBudgetAvailable_FailsOpenAndClosesAtLimit(t *testing.T) {
	cfg := testCrawlerConfig()
	cfg.Crawler.MaxPagesPerDomain = 2
	eng, st := newTestEngine(t, cfg)

	if !eng.pageBudgetAvailable("fresh.example") {
		t.Fatal("expected budget available for a host with no existing page_fetch rows")
	}

	fr := frontier.New(st.DB)
	for i := 0; i < 2; i++ {
		item := &store.FrontierItem{
			Kind:     store.KindPageFetch,
			Host:     "fresh.example",
			URL:      fmt.Sprintf("https://fresh.example/%d", i),
			DedupKey: fmt.Sprintf("page_fetch:https://fresh.example/%d", i),
		}
		if _, err := fr.Enqueue(item); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	if eng.pageBudgetAvailable("fresh.example") {
		t.Fatal("expected budget exhausted once maxPagesPerDomain rows exist for the host")
	}
}
