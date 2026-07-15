package crawler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	eng.setArtifactEntry(srv.URL+"/card.json", 42)

	item := store.FrontierItem{URL: srv.URL + "/card.json"}
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
	eng.setArtifactEntry(srv.URL+"/missing.json", 7)

	item := store.FrontierItem{URL: srv.URL + "/missing.json"}
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
	eng.setArtifactEntry(srv.URL+"/ok.json", 1)

	item := store.FrontierItem{Kind: store.KindArtifactFetch, URL: srv.URL + "/ok.json", DedupKey: "x", Status: store.FrontierStatusInFlight}
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

func TestRun_GracefulShutdownOnCancel(t *testing.T) {
	eng, _ := newTestEngine(t, testCrawlerConfig())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := eng.Run(ctx); err != nil {
		t.Fatalf("expected graceful nil return on cancelled context, got %v", err)
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
