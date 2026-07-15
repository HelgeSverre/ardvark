// Package integration drives end-to-end runs of internal/crawler against
// real httptest.Server fixtures and an in-memory SQLite store, per the
// design doc's testing-strategy section ("Integration tests"). Each test
// simulates a small fake website (or set of hosts) and exercises the real
// crawler.Engine — no crawler internals are mocked.
//
// A quirk this file works around: internal/probe's well-known probe always
// dials https://<host>/.well-known/ai-catalog.json (per the ARD spec, that
// is the correct, non-configurable behavior). Any test that needs a
// well-known probe to actually hit therefore uses httptest.NewTLSServer and
// builds the crawler's fetch.Client with fetch.WithTransport(srv.Client().
// Transport) so the client trusts that server's self-signed certificate.
// Tests that only need robots.txt-based (Agentmap) discovery, or that only
// exercise page_fetch/artifact_fetch in isolation, use plain httptest.Server
// since internal/fetch's robots.txt lookup already falls back from https to
// http on a connection-level failure.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/registry"
	"github.com/helgesverre/ardvark/internal/store"
)

// -- shared test helpers -----------------------------------------------------

// discardWriter is an io.Writer that swallows everything, used to build a
// logger that stays silent during tests but still exercises every real
// slog.Logger call site in the engine.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

var testDSNCounter int64

// newStore opens a fresh, uniquely-named in-memory SQLite database (shared
// cache, so a second Open call against the same dsn reconnects to the same
// data — used by the resumability test to simulate a process restart).
func newStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	s, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("store.Open(%q): %v", dsn, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newDSN returns a fresh unique in-memory sqlite DSN for a self-contained
// test.
func newDSN(t *testing.T) string {
	t.Helper()
	n := atomic.AddInt64(&testDSNCounter, 1)
	return "file:ardvark_integration_" + t.Name() + "_" + time.Now().Format("150405.000000") + "_" +
		strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared&_x=" + itoa(n)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// baseTestConfig returns a fast, deterministic crawler config suitable for
// hitting httptest fixtures: high per-host rate limit, short timeouts, and
// the design doc's default recursion depths.
func baseTestConfig() config.Config {
	cfg := config.Defaults()
	cfg.Crawler.Concurrency = 4
	cfg.Crawler.PerHostRequestsPerSecond = 1000
	cfg.Crawler.RequestTimeoutSeconds = 5
	cfg.Crawler.RespectRobotsTxt = true
	cfg.Crawler.MaxDepth = 1
	cfg.Crawler.MaxPagesPerDomain = 10
	cfg.ARD.MaxCatalogDepth = 3
	cfg.ARD.FetchArtifacts = true
	cfg.Registry.Harvest = true
	cfg.Registry.PageLimit = 5
	return cfg
}

// newEngine wires up a crawler.Engine against a fresh store, backed by fc
// (built by the caller so tests can inject fetch.WithTransport for TLS
// fixtures).
func newEngine(t *testing.T, cfg config.Config, st *store.Store, fc *fetch.Client, opts crawler.Options) *crawler.Engine {
	t.Helper()
	fr := frontier.New(st.DB)
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 2
	}
	return crawler.New(cfg, st, fr, fc, discardLogger(), opts)
}

// tlsFetchClient builds a fetch.Client whose transport trusts srv's
// self-signed certificate, so the client can reach both srv (over https,
// e.g. a well-known probe) and any plain httptest.Server (over http) fixture
// in the same test.
func tlsFetchClient(cfg config.Config, srv *httptest.Server) *fetch.Client {
	return fetch.New(cfg.Crawler, fetch.WithTransport(srv.Client().Transport))
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

func hostOfURL(t *testing.T, rawURL string) string {
	t.Helper()
	rawURL = strings.TrimPrefix(rawURL, "https://")
	rawURL = strings.TrimPrefix(rawURL, "http://")
	return rawURL
}

func runEngine(t *testing.T, eng *crawler.Engine, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
}

// -- 1. Happy path: page harvest -> well-known catalog -> nested embedded
//      catalog -> artifacts -> registry (2 pages + 1 referral). -------------

func TestHappyPath_FullDiscoveryChain(t *testing.T) {
	// -- fake registry #2 (referral target): one page, one result, no
	// further referrals.
	registry2Mux := http.NewServeMux()
	registry2 := httptest.NewServer(registry2Mux)
	defer registry2.Close()
	registry2Mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(registry.SearchResponse{
			Results: []registry.Result{
				{Entry: ard.Entry{
					Identifier:  "urn:air:registry2.example:agents:three",
					DisplayName: "Referral Result",
					Type:        "application/a2a-agent-card+json",
					URL:         "https://registry2.example/three.json",
				}},
			},
		})
	})

	// -- fake registry #1: two pages of results, referral to registry #2 on
	// the last page.
	registry1Mux := http.NewServeMux()
	registry1 := httptest.NewServer(registry1Mux)
	defer registry1.Close()
	registry1Mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		var req registry.SearchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.PageToken == "" {
			_ = json.NewEncoder(w).Encode(registry.SearchResponse{
				Results: []registry.Result{
					{Entry: ard.Entry{
						Identifier:  "urn:air:registry.example:agents:one",
						DisplayName: "Registry Result One",
						Type:        "application/a2a-agent-card+json",
						URL:         "https://registry.example/one.json",
					}},
				},
				PageToken: "page-2",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(registry.SearchResponse{
			Results: []registry.Result{
				{Entry: ard.Entry{
					Identifier:  "urn:air:registry.example:agents:two",
					DisplayName: "Registry Result Two",
					Type:        "application/a2a-agent-card+json",
					URL:         "https://registry.example/two.json",
				}},
			},
			Referrals: []ard.Entry{
				{
					Identifier:  "urn:air:registry2.example:root",
					DisplayName: "Registry Two",
					Type:        "application/ai-registry+json",
					URL:         registry2.URL,
				},
			},
		})
	})

	// -- Site B: the catalog host, served over TLS so the hardcoded
	// well-known https:// probe can hit it.
	siteBMux := http.NewServeMux()
	siteB := httptest.NewTLSServer(siteBMux)
	defer siteB.Close()

	siteBMux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	siteBMux.HandleFunc("/artifacts/support-bot.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/a2a-agent-card+json")
		_, _ = w.Write([]byte(`{"name":"support-bot"}`))
	})
	siteBMux.HandleFunc("/artifacts/nested-artifact.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-skill+json")
		_, _ = w.Write([]byte(`{"name":"recipe-finder"}`))
	})

	// Root catalog modeled on the spec's enterprise example: entry 0 is
	// patched to a url-referenced artifact on Site B; entry 1 (a
	// data-embedded MCP server card) is left as-is; a nested data-embedded
	// catalog (the spec's solo-developer example, url-patched) and a
	// registry entry are appended.
	var enterprise ard.Catalog
	if err := json.Unmarshal(loadFixture(t, "enterprise-catalog.json"), &enterprise); err != nil {
		t.Fatalf("unmarshal enterprise fixture: %v", err)
	}
	enterprise.Entries[0].URL = siteB.URL + "/artifacts/support-bot.json"

	var soloDev ard.Catalog
	if err := json.Unmarshal(loadFixture(t, "solo-dev-catalog.json"), &soloDev); err != nil {
		t.Fatalf("unmarshal solo-dev fixture: %v", err)
	}
	soloDev.Entries[0].URL = siteB.URL + "/artifacts/nested-artifact.json"
	nestedBytes, err := json.Marshal(soloDev)
	if err != nil {
		t.Fatalf("marshal nested catalog: %v", err)
	}

	enterprise.Entries = append(enterprise.Entries,
		ard.Entry{
			Identifier:  "urn:air:acme.example:catalogs:nested",
			DisplayName: "Nested Catalog",
			Type:        "application/ai-catalog+json",
			Data:        nestedBytes,
		},
		ard.Entry{
			Identifier:  "urn:air:acme.example:registries:main",
			DisplayName: "Main Registry",
			Type:        "application/ai-registry+json",
			URL:         registry1.URL,
		},
	)
	rootBytes, err := json.Marshal(enterprise)
	if err != nil {
		t.Fatalf("marshal root catalog: %v", err)
	}

	siteBMux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		_, _ = w.Write(rootBytes)
	})

	// -- Site A: homepage links to Site B, the entry point for domain
	// harvesting.
	siteAMux := http.NewServeMux()
	siteA := httptest.NewServer(siteAMux)
	defer siteA.Close()
	siteAMux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	siteAMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><a href="` + siteB.URL + `/">Site B</a></body></html>`))
	})

	cfg := baseTestConfig()
	st := newStore(t, newDSN(t))
	fc := tlsFetchClient(cfg, siteB)
	eng := newEngine(t, cfg, st, fc, crawler.Options{})

	if _, err := eng.EnqueueSeedURL(siteA.URL + "/"); err != nil {
		t.Fatalf("EnqueueSeedURL: %v", err)
	}

	runEngine(t, eng, 20*time.Second)

	siteBHost := hostOfURL(t, siteB.URL)

	// -- domains: Site B only (discovered via Site A's anchor).
	var domainCount int64
	st.DB.Model(&store.Domain{}).Count(&domainCount)
	if domainCount != 1 {
		t.Fatalf("expected 1 domain row, got %d", domainCount)
	}
	domain, err := st.DomainByHost(siteBHost)
	if err != nil {
		t.Fatalf("expected domain row for %s: %v", siteBHost, err)
	}
	if domain.ARDStatus != store.ARDStatusFoundValid {
		t.Errorf("expected domain ARDStatus found_valid, got %q", domain.ARDStatus)
	}

	// -- probes: well_known hit + robots_agentmap miss.
	var probes []store.Probe
	st.DB.Where("domain_id = ?", domain.ID).Order("method").Find(&probes)
	if len(probes) != 2 {
		t.Fatalf("expected 2 probe rows, got %d", len(probes))
	}
	byMethod := map[string]store.Probe{}
	for _, p := range probes {
		byMethod[p.Method] = p
	}
	if byMethod[store.ProbeMethodWellKnown].Outcome != store.ProbeOutcomeHit {
		t.Errorf("expected well_known probe outcome hit, got %q", byMethod[store.ProbeMethodWellKnown].Outcome)
	}
	if byMethod[store.ProbeMethodRobotsAgentmap].Outcome != store.ProbeOutcomeMiss {
		t.Errorf("expected robots_agentmap probe outcome miss, got %q", byMethod[store.ProbeMethodRobotsAgentmap].Outcome)
	}

	// -- catalogs: root (no parent) + nested (parent = root).
	var catalogs []store.Catalog
	st.DB.Preload("Entries").Order("id").Find(&catalogs)
	if len(catalogs) != 2 {
		t.Fatalf("expected 2 catalog rows, got %d", len(catalogs))
	}
	root, nested := catalogs[0], catalogs[1]
	if root.ParentCatalogID != nil {
		t.Errorf("expected root catalog to have no parent, got %v", *root.ParentCatalogID)
	}
	if nested.ParentCatalogID == nil || *nested.ParentCatalogID != root.ID {
		t.Fatalf("expected nested catalog parent_catalog_id = %d, got %v", root.ID, nested.ParentCatalogID)
	}
	if root.VerificationStatus == store.VerificationStatusInvalid {
		t.Errorf("expected root catalog not invalid, got %q", root.VerificationStatus)
	}
	if nested.VerificationStatus == store.VerificationStatusInvalid {
		t.Errorf("expected nested catalog not invalid, got %q", nested.VerificationStatus)
	}
	if nested.HostDisplayName != "Jane's Weekend Projects" {
		t.Errorf("expected nested catalog host display name from the solo-dev fixture, got %q", nested.HostDisplayName)
	}

	// -- catalog entries, source=catalog: 4 on root (support-bot,
	// invoice-lookup MCP, nested-catalog pointer, registry pointer) + 1 on
	// nested (recipe-finder).
	var rootEntryCount, nestedEntryCount int64
	st.DB.Model(&store.CatalogEntry{}).Where("catalog_id = ? AND source = ?", root.ID, store.EntrySourceCatalog).Count(&rootEntryCount)
	st.DB.Model(&store.CatalogEntry{}).Where("catalog_id = ? AND source = ?", nested.ID, store.EntrySourceCatalog).Count(&nestedEntryCount)
	if rootEntryCount != 4 {
		t.Errorf("expected 4 catalog-sourced entries on root catalog, got %d", rootEntryCount)
	}
	if nestedEntryCount != 1 {
		t.Errorf("expected 1 catalog-sourced entry on nested catalog, got %d", nestedEntryCount)
	}

	// -- registries: main + one referral, both harvested ok.
	var registries []store.Registry
	st.DB.Order("id").Find(&registries)
	if len(registries) != 2 {
		t.Fatalf("expected 2 registries rows, got %d", len(registries))
	}
	mainReg, referralReg := registries[0], registries[1]
	if mainReg.HarvestStatus != store.HarvestStatusOK {
		t.Errorf("expected main registry harvest_status ok, got %q", mainReg.HarvestStatus)
	}
	if referralReg.HarvestStatus != store.HarvestStatusOK {
		t.Errorf("expected referral registry harvest_status ok, got %q", referralReg.HarvestStatus)
	}
	if referralReg.ReferralSourceID == nil || *referralReg.ReferralSourceID != mainReg.ID {
		t.Fatalf("expected referral registry's referral_source_id = %d, got %v", mainReg.ID, referralReg.ReferralSourceID)
	}
	if referralReg.BaseURL != registry2.URL {
		t.Errorf("expected referral registry base_url %q, got %q", registry2.URL, referralReg.BaseURL)
	}

	// -- catalog entries, source=registry: 2 (main, across 2 pages) + 1
	// (referral) = 3, all attributed to the root catalog.
	var registryEntries []store.CatalogEntry
	st.DB.Where("source = ?", store.EntrySourceRegistry).Order("id").Find(&registryEntries)
	if len(registryEntries) != 3 {
		t.Fatalf("expected 3 registry-sourced entries, got %d", len(registryEntries))
	}
	for _, e := range registryEntries {
		if e.CatalogID != root.ID {
			t.Errorf("expected registry-sourced entry %d to be attributed to root catalog %d, got %d", e.ID, root.ID, e.CatalogID)
		}
		if e.SourceRegistryID == nil {
			t.Errorf("expected registry-sourced entry %d to carry a source_registry_id", e.ID)
		}
	}

	// -- artifacts: support-bot + recipe-finder (nested), both fetched ok.
	var artifacts []store.Artifact
	st.DB.Order("id").Find(&artifacts)
	if len(artifacts) != 2 {
		t.Fatalf("expected 2 artifact rows, got %d", len(artifacts))
	}
	for _, a := range artifacts {
		if a.FetchStatus != store.FetchStatusOK {
			t.Errorf("expected artifact %d fetch_status ok, got %q", a.ID, a.FetchStatus)
		}
	}

	// -- verification checks recorded for both catalogs.
	var rootChecks, nestedChecks int64
	st.DB.Model(&store.VerificationCheck{}).Where("subject_type = ? AND subject_id = ?", store.SubjectTypeCatalog, root.ID).Count(&rootChecks)
	st.DB.Model(&store.VerificationCheck{}).Where("subject_type = ? AND subject_id = ?", store.SubjectTypeCatalog, nested.ID).Count(&nestedChecks)
	if rootChecks == 0 {
		t.Error("expected at least one verification check for the root catalog")
	}
	if nestedChecks == 0 {
		t.Error("expected at least one verification check for the nested catalog")
	}
}

// -- 2. Invalid catalog: schema- and URN-violating document is still
//      stored, with verdict invalid and failing check rows. -----------------

func TestInvalidCatalog_StoredWithFailingChecks(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		_, _ = w.Write(loadFixture(t, "invalid-catalog.json"))
	})

	cfg := baseTestConfig()
	st := newStore(t, newDSN(t))
	fc := tlsFetchClient(cfg, srv)
	eng := newEngine(t, cfg, st, fc, crawler.Options{})

	host := hostOfURL(t, srv.URL)
	if _, err := eng.EnqueueSeedHost(host, store.DiscoverySourceSeed); err != nil {
		t.Fatalf("EnqueueSeedHost: %v", err)
	}

	runEngine(t, eng, 10*time.Second)

	var cat store.Catalog
	if err := st.DB.Preload("Entries").First(&cat).Error; err != nil {
		t.Fatalf("expected catalog row: %v", err)
	}
	if cat.VerificationStatus != store.VerificationStatusInvalid {
		t.Fatalf("expected verdict invalid, got %q", cat.VerificationStatus)
	}
	if len(cat.Entries) != 1 {
		t.Fatalf("expected the invalid catalog's entry to still be stored, got %d entries", len(cat.Entries))
	}

	var failingErrorChecks int64
	st.DB.Model(&store.VerificationCheck{}).
		Where("subject_type = ? AND subject_id = ? AND severity = ? AND passed = ?",
			store.SubjectTypeCatalog, cat.ID, store.SeverityError, false).
		Count(&failingErrorChecks)
	var failingEntryErrorChecks int64
	st.DB.Model(&store.VerificationCheck{}).
		Where("subject_type = ? AND subject_id = ? AND severity = ? AND passed = ?",
			store.SubjectTypeEntry, cat.Entries[0].ID, store.SeverityError, false).
		Count(&failingEntryErrorChecks)

	if failingErrorChecks+failingEntryErrorChecks == 0 {
		t.Error("expected at least one failing error-severity verification check")
	}
}

// -- 3. robots.txt blocks page crawling, but the well-known probe (which
//      always bypasses robots.txt) still succeeds. --------------------------

func TestRobotsBlockedPage_WellKnownStillProbed(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		_, _ = w.Write([]byte(`{"specVersion":"1.0","host":{"displayName":"Blocked Pages Host"},"entries":[]}`))
	})
	mux.HandleFunc("/private-page", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>should never be fetched</body></html>`))
	})

	cfg := baseTestConfig()
	st := newStore(t, newDSN(t))
	fc := tlsFetchClient(cfg, srv)
	eng := newEngine(t, cfg, st, fc, crawler.Options{})

	if _, err := eng.EnqueueSeedURL(srv.URL + "/private-page"); err != nil {
		t.Fatalf("EnqueueSeedURL: %v", err)
	}
	host := hostOfURL(t, srv.URL)
	if _, err := eng.EnqueueSeedHost(host, store.DiscoverySourceSeed); err != nil {
		t.Fatalf("EnqueueSeedHost: %v", err)
	}

	runEngine(t, eng, 10*time.Second)

	// The page_fetch item completes (robots-disallowed is a graceful skip,
	// not a failure), and no run-wide failure occurs.
	var pageItem store.FrontierItem
	if err := st.DB.Where("kind = ? AND url = ?", store.KindPageFetch, srv.URL+"/private-page").First(&pageItem).Error; err != nil {
		t.Fatalf("expected page_fetch frontier row: %v", err)
	}
	if pageItem.Status != store.FrontierStatusDone {
		t.Errorf("expected robots-blocked page_fetch to complete (not fail), got status %q", pageItem.Status)
	}

	// The well-known probe still hit despite robots.txt disallowing "/".
	domain, err := st.DomainByHost(host)
	if err != nil {
		t.Fatalf("expected domain row: %v", err)
	}
	var wellKnownProbe store.Probe
	if err := st.DB.Where("domain_id = ? AND method = ?", domain.ID, store.ProbeMethodWellKnown).First(&wellKnownProbe).Error; err != nil {
		t.Fatalf("expected well_known probe row: %v", err)
	}
	if wellKnownProbe.Outcome != store.ProbeOutcomeHit {
		t.Errorf("expected well_known probe to hit despite robots.txt disallowing pages, got %q", wellKnownProbe.Outcome)
	}

	var catalogCount int64
	st.DB.Model(&store.Catalog{}).Count(&catalogCount)
	if catalogCount != 1 {
		t.Errorf("expected 1 catalog row from the well-known hit, got %d", catalogCount)
	}
}

// -- 4. Redirect loop and oversized body both end their frontier items
//      failed, and the run completes without hanging or erroring. ---------

func TestRedirectLoopAndOversizedBody_ItemsFailRunCompletes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/loop-a", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop-b", http.StatusFound)
	})
	mux.HandleFunc("/loop-b", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop-a", http.StatusFound)
	})
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := baseTestConfig()
	cfg.Crawler.MaxBodyBytes = 64 // small cap so /huge trips ErrBodyTooLarge
	st := newStore(t, newDSN(t))
	fc := fetch.New(cfg.Crawler)
	eng := newEngine(t, cfg, st, fc, crawler.Options{MaxAttempts: 2})

	if _, err := eng.EnqueueSeedURL(srv.URL + "/loop-a"); err != nil {
		t.Fatalf("EnqueueSeedURL(loop-a): %v", err)
	}
	if _, err := eng.EnqueueSeedURL(srv.URL + "/huge"); err != nil {
		t.Fatalf("EnqueueSeedURL(huge): %v", err)
	}

	runEngine(t, eng, 15*time.Second)

	var pending int64
	st.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusPending).Count(&pending)
	if pending != 0 {
		t.Errorf("expected the frontier to be fully drained (no items stuck pending), got %d", pending)
	}

	var loopItem, hugeItem store.FrontierItem
	if err := st.DB.Where("kind = ? AND url = ?", store.KindPageFetch, srv.URL+"/loop-a").First(&loopItem).Error; err != nil {
		t.Fatalf("expected loop-a frontier row: %v", err)
	}
	if err := st.DB.Where("kind = ? AND url = ?", store.KindPageFetch, srv.URL+"/huge").First(&hugeItem).Error; err != nil {
		t.Fatalf("expected huge frontier row: %v", err)
	}

	if loopItem.Status != store.FrontierStatusFailed {
		t.Errorf("expected the redirect-loop item to end failed, got %q (last_error=%q)", loopItem.Status, loopItem.LastError)
	}
	if loopItem.LastError == "" {
		t.Error("expected the redirect-loop item to record last_error")
	}
	if hugeItem.Status != store.FrontierStatusFailed {
		t.Errorf("expected the oversized-body item to end failed, got %q (last_error=%q)", hugeItem.Status, hugeItem.LastError)
	}
	if hugeItem.LastError == "" {
		t.Error("expected the oversized-body item to record last_error")
	}
}

// -- 5. Agentmap discovery: robots.txt names a custom catalog path, there is
//      no well-known document, and the catalog is found via the
//      robots_agentmap probe method. ----------------------------------------

func TestAgentmapDiscovery_NoWellKnown(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nAgentmap: /custom/catalog.json\n"))
	})
	// No handler registered for /.well-known/ai-catalog.json: the mux 404s.

	var soloDev ard.Catalog
	if err := json.Unmarshal(loadFixture(t, "solo-dev-catalog.json"), &soloDev); err != nil {
		t.Fatalf("unmarshal solo-dev fixture: %v", err)
	}
	soloDev.Entries[0].URL = srv.URL + "/artifacts/recipe-finder.json"
	catalogBytes, err := json.Marshal(soloDev)
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	mux.HandleFunc("/custom/catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		_, _ = w.Write(catalogBytes)
	})
	mux.HandleFunc("/artifacts/recipe-finder.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-skill+json")
		_, _ = w.Write([]byte(`{"name":"recipe-finder"}`))
	})

	cfg := baseTestConfig()
	st := newStore(t, newDSN(t))
	fc := tlsFetchClient(cfg, srv)
	eng := newEngine(t, cfg, st, fc, crawler.Options{})

	host := hostOfURL(t, srv.URL)
	if _, err := eng.EnqueueSeedHost(host, store.DiscoverySourceSeed); err != nil {
		t.Fatalf("EnqueueSeedHost: %v", err)
	}

	runEngine(t, eng, 10*time.Second)

	domain, err := st.DomainByHost(host)
	if err != nil {
		t.Fatalf("expected domain row: %v", err)
	}

	var probes []store.Probe
	st.DB.Where("domain_id = ?", domain.ID).Find(&probes)
	byMethod := map[string]store.Probe{}
	for _, p := range probes {
		byMethod[p.Method] = p
	}
	if byMethod[store.ProbeMethodWellKnown].Outcome != store.ProbeOutcomeMiss {
		t.Errorf("expected well_known probe outcome miss (no well-known document), got %q", byMethod[store.ProbeMethodWellKnown].Outcome)
	}
	if byMethod[store.ProbeMethodRobotsAgentmap].Outcome != store.ProbeOutcomeHit {
		t.Fatalf("expected robots_agentmap probe outcome hit, got %q", byMethod[store.ProbeMethodRobotsAgentmap].Outcome)
	}

	var cat store.Catalog
	if err := st.DB.First(&cat).Error; err != nil {
		t.Fatalf("expected a catalog row discovered via robots_agentmap: %v", err)
	}
	if cat.SourceURL != srv.URL+"/custom/catalog.json" {
		t.Errorf("expected catalog source_url %q, got %q", srv.URL+"/custom/catalog.json", cat.SourceURL)
	}
	if cat.VerificationStatus == store.VerificationStatusInvalid {
		t.Errorf("expected the agentmap-discovered catalog to verify cleanly, got %q", cat.VerificationStatus)
	}
}

// -- 6. Resumability: a run that stops with pending frontier items (process
//      restart simulated by reopening the same shared-cache in-memory
//      database) picks the pending work back up, with no duplicate rows
//      thanks to frontier dedup keys. ----------------------------------------

func TestResumability_PendingItemsSurviveRestartAndDedup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	mux.HandleFunc("/pageA", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>page A, no links</body></html>`))
	})
	mux.HandleFunc("/pageB", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>page B, no links</body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dsn := newDSN(t)
	cfg := baseTestConfig()

	// "Run 1": a crawl that gets through pageA, then the process is killed
	// (simulated by simply never draining pageB) — followed by an explicit
	// cancelled-context Run() to confirm graceful shutdown leaves pending
	// work untouched rather than corrupting it.
	st1 := newStore(t, dsn)
	fc1 := fetch.New(cfg.Crawler)
	eng1 := newEngine(t, cfg, st1, fc1, crawler.Options{})

	pageAURL := srv.URL + "/pageA"
	pageBURL := srv.URL + "/pageB"
	if _, err := eng1.EnqueueSeedURL(pageAURL); err != nil {
		t.Fatalf("EnqueueSeedURL(pageA): %v", err)
	}
	if _, err := eng1.EnqueueSeedURL(pageBURL); err != nil {
		t.Fatalf("EnqueueSeedURL(pageB): %v", err)
	}

	items, err := frontier.New(st1.DB).Dequeue(1)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected to dequeue exactly 1 item, got %d", len(items))
	}
	eng1.ProcessItem(context.Background(), items[0])

	// A cancelled-context Run() must be a graceful no-op: the still-pending
	// item is left untouched, not corrupted or duplicated.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := eng1.Run(cancelledCtx); err != nil {
		t.Fatalf("Run(cancelled): %v", err)
	}

	var doneAfterRun1, pendingAfterRun1 int64
	st1.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusDone).Count(&doneAfterRun1)
	st1.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusPending).Count(&pendingAfterRun1)
	if doneAfterRun1 != 1 {
		t.Fatalf("expected exactly 1 done item after run 1, got %d", doneAfterRun1)
	}
	if pendingAfterRun1 != 1 {
		t.Fatalf("expected exactly 1 pending item after run 1, got %d", pendingAfterRun1)
	}

	// "Restart": a brand-new Store/Frontier/Engine reconnecting to the same
	// shared-cache in-memory database, exactly as a fresh process invocation
	// would reopen the on-disk DB. Resumes and drains what's left.
	st2 := newStore(t, dsn)
	fc2 := fetch.New(cfg.Crawler)
	eng2 := newEngine(t, cfg, st2, fc2, crawler.Options{})

	runEngine(t, eng2, 10*time.Second)

	var pendingAfterResume int64
	st2.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusPending).Count(&pendingAfterResume)
	if pendingAfterResume != 0 {
		t.Errorf("expected no pending items after resuming, got %d", pendingAfterResume)
	}

	var pageAItem, pageBItem store.FrontierItem
	if err := st2.DB.Where("kind = ? AND url = ?", store.KindPageFetch, pageAURL).First(&pageAItem).Error; err != nil {
		t.Fatalf("expected pageA frontier row: %v", err)
	}
	if err := st2.DB.Where("kind = ? AND url = ?", store.KindPageFetch, pageBURL).First(&pageBItem).Error; err != nil {
		t.Fatalf("expected pageB frontier row: %v", err)
	}
	if pageAItem.Status != store.FrontierStatusDone {
		t.Errorf("expected pageA status done, got %q", pageAItem.Status)
	}
	if pageBItem.Status != store.FrontierStatusDone {
		t.Errorf("expected pageB (resumed) status done, got %q", pageBItem.Status)
	}

	// Dedup: re-enqueueing the same seed URLs after "resuming" must never
	// create duplicate frontier rows (the unique dedup_key constraint). The
	// prior items are already "done", so re-enqueueing resets them back to
	// "pending" in place (so a later run can actually re-crawl them, honoring
	// the freshness-window/--force re-probe design) rather than being a
	// silent no-op forever.
	okA, err := eng2.EnqueueSeedURL(pageAURL)
	if err != nil {
		t.Fatalf("re-EnqueueSeedURL(pageA): %v", err)
	}
	if !okA {
		t.Error("expected re-enqueuing a done seed URL to reset it to pending")
	}
	okB, err := eng2.EnqueueSeedURL(pageBURL)
	if err != nil {
		t.Fatalf("re-EnqueueSeedURL(pageB): %v", err)
	}
	if !okB {
		t.Error("expected re-enqueuing a done seed URL to reset it to pending")
	}

	var totalItems int64
	st2.DB.Model(&store.FrontierItem{}).Where("url IN ?", []string{pageAURL, pageBURL}).Count(&totalItems)
	if totalItems != 2 {
		t.Errorf("expected exactly 2 frontier rows total for pageA/pageB (no duplicates), got %d", totalItems)
	}
}
