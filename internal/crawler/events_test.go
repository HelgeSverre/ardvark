package crawler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/probe"
	"github.com/helgesverre/ardvark/internal/store"
)

// eventRecorder is a goroutine-safe OnProbe sink for tests.
type eventRecorder struct {
	mu     sync.Mutex
	events []ProbeEvent
}

func (r *eventRecorder) record(ev ProbeEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *eventRecorder) all() []ProbeEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ProbeEvent(nil), r.events...)
}

func (r *eventRecorder) byMethod(method string) (ProbeEvent, bool) {
	for _, ev := range r.all() {
		if ev.Method == method {
			return ev, true
		}
	}
	return ProbeEvent{}, false
}

func TestOnProbe_HostProbeMissFires(t *testing.T) {
	mux := http.NewServeMux()
	// No handlers: well-known 404s, robots.txt 404s.
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	rec := &eventRecorder{}
	st := newTestStore(t)
	cfg := testCrawlerConfig()
	fc := fetch.New(cfg.Crawler, fetch.WithTransport(srv.Client().Transport))
	eng := New(cfg, st, frontier.New(st.DB), fc, discardLogger(), Options{MaxAttempts: 2, OnProbe: rec.record})

	host := strings.TrimPrefix(srv.URL, "https://")
	if err := eng.handleHostProbe(context.Background(), store.FrontierItem{Host: host}); err != nil {
		t.Fatalf("handleHostProbe: %v", err)
	}

	events := rec.all()
	if len(events) != 2 {
		t.Fatalf("expected 2 OnProbe events (well_known + robots_agentmap miss), got %d: %+v", len(events), events)
	}

	wk, ok := rec.byMethod(probe.MethodWellKnown)
	if !ok {
		t.Fatal("expected a well_known event")
	}
	if wk.Outcome != probe.OutcomeMiss {
		t.Errorf("well_known outcome = %q, want miss", wk.Outcome)
	}
	if wk.Host != host {
		t.Errorf("well_known host = %q, want %q", wk.Host, host)
	}
	if wk.Detail != "404" {
		t.Errorf("well_known detail = %q, want \"404\"", wk.Detail)
	}
	if wk.Verdict != "" {
		t.Errorf("miss event verdict = %q, want empty", wk.Verdict)
	}

	robots, ok := rec.byMethod(probe.MethodRobotsAgentmap)
	if !ok {
		t.Fatal("expected a robots_agentmap event")
	}
	if robots.Outcome != probe.OutcomeMiss {
		t.Errorf("robots_agentmap outcome = %q, want miss", robots.Outcome)
	}
}

func TestOnProbe_HitFiresOnCatalogVerification(t *testing.T) {
	// A schema-valid catalog whose URN publisher (jane.dev) cannot match the
	// httptest serving host, so the deterministic verdict is
	// valid_with_warnings with urn.publisher_matches as the failing check.
	const catalogJSON = `{
		"specVersion": "1.0",
		"host": {"displayName": "Jane's Weekend Projects"},
		"entries": [{
			"identifier": "urn:air:jane.dev:skills:recipe-finder",
			"displayName": "Recipe Finder Skill",
			"type": "application/ai-skill+json",
			"url": "https://jane.dev/skills/recipe-finder.json",
			"representativeQueries": ["What can I make with chicken and rice?", "Suggest a recipe"]
		}]
	}`

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(catalogJSON))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	rec := &eventRecorder{}
	st := newTestStore(t)
	cfg := testCrawlerConfig()
	cfg.ARD.FetchArtifacts = false // the entry's url points at jane.dev; don't fetch it
	fc := fetch.New(cfg.Crawler, fetch.WithTransport(srv.Client().Transport))
	eng := New(cfg, st, frontier.New(st.DB), fc, discardLogger(), Options{MaxAttempts: 2, OnProbe: rec.record})

	host := strings.TrimPrefix(srv.URL, "https://")
	if _, err := eng.EnqueueSeedHost(host, store.DiscoverySourceSeed); err != nil {
		t.Fatalf("EnqueueSeedHost: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := eng.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	hit, ok := rec.byMethod(probe.MethodWellKnown)
	if !ok {
		t.Fatalf("expected a well_known hit event, got %+v", rec.all())
	}
	if hit.Outcome != probe.OutcomeHit {
		t.Errorf("outcome = %q, want hit", hit.Outcome)
	}
	if hit.Host != host {
		t.Errorf("host = %q, want %q", hit.Host, host)
	}
	if hit.Verdict != ard.VerdictValidWithWarnings {
		t.Errorf("verdict = %q, want %q", hit.Verdict, ard.VerdictValidWithWarnings)
	}
	if !strings.Contains(hit.Detail, "urn.publisher_matches") {
		t.Errorf("detail = %q, want it to name the failing check urn.publisher_matches", hit.Detail)
	}

	// The robots miss should also have produced its own event.
	if robots, ok := rec.byMethod(probe.MethodRobotsAgentmap); !ok || robots.Outcome != probe.OutcomeMiss {
		t.Errorf("expected a robots_agentmap miss event, got %+v", rec.all())
	}
}

func TestOnProbe_InvalidCatalogSummarizesFailingChecks(t *testing.T) {
	// Three entries with malformed URNs: verdict invalid. The malformed
	// identifiers fail JSON Schema validation directly, and semantic
	// checks are short-circuited once schema validation has already
	// failed (see ard.Verify's doc comment) to avoid double-reporting the
	// same defect as both "schema.validation" and "urn.format" — so the
	// failing-check summary should read "schema.validation ×3".
	const catalogJSON = `{
		"specVersion": "1.0",
		"host": {"displayName": "Broken"},
		"entries": [
			{"identifier": "not-a-urn-1", "displayName": "A", "type": "application/ai-skill+json", "url": "https://x.example/a.json"},
			{"identifier": "not-a-urn-2", "displayName": "B", "type": "application/ai-skill+json", "url": "https://x.example/b.json"},
			{"identifier": "not-a-urn-3", "displayName": "C", "type": "application/ai-skill+json", "url": "https://x.example/c.json"}
		]
	}`

	mux := http.NewServeMux()
	mux.HandleFunc("/broken.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(catalogJSON))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	rec := &eventRecorder{}
	st := newTestStore(t)
	cfg := testCrawlerConfig()
	cfg.ARD.FetchArtifacts = false
	fc := fetch.New(cfg.Crawler)
	eng := New(cfg, st, frontier.New(st.DB), fc, discardLogger(), Options{MaxAttempts: 2, OnProbe: rec.record})

	host := strings.TrimPrefix(srv.URL, "http://")
	item := store.FrontierItem{URL: srv.URL + "/broken.json", Host: host, Depth: 0}
	if err := eng.handleCatalogFetch(context.Background(), item); err != nil {
		t.Fatalf("handleCatalogFetch: %v", err)
	}

	events := rec.all()
	if len(events) != 1 {
		t.Fatalf("expected 1 OnProbe event, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Outcome != probe.OutcomeHit {
		t.Errorf("outcome = %q, want hit", ev.Outcome)
	}
	if ev.Verdict != ard.VerdictInvalid {
		t.Errorf("verdict = %q, want %q", ev.Verdict, ard.VerdictInvalid)
	}
	if !strings.Contains(ev.Detail, "schema.validation ×3") {
		t.Errorf("detail = %q, want it to contain \"schema.validation ×3\"", ev.Detail)
	}
	// The catalog was fetched directly (no probe hit recorded the method).
	if ev.Method != "" {
		t.Errorf("method = %q, want empty for an untracked catalog fetch", ev.Method)
	}
}

func TestOnProbe_NilCallbackIsSafe(t *testing.T) {
	eng, _ := newTestEngine(t, testCrawlerConfig())
	// Must not panic with Options.OnProbe unset.
	eng.emit(ProbeEvent{Host: "example.com", Outcome: probe.OutcomeMiss})
}

func TestFailingChecksSummary(t *testing.T) {
	checks := []ard.Check{
		{CheckID: "urn.format", Severity: ard.SeverityError, Passed: false},
		{CheckID: "urn.format", Severity: ard.SeverityError, Passed: false},
		{CheckID: "urn.format", Severity: ard.SeverityError, Passed: false},
		{CheckID: "schema.validation", Severity: ard.SeverityError, Passed: false},
		{CheckID: "queries.count", Severity: ard.SeverityWarning, Passed: false},
		{CheckID: "catalog.spec_version", Severity: ard.SeverityError, Passed: true},
	}

	if got, want := failingChecksSummary(checks, ard.SeverityError), "urn.format ×3, schema.validation"; got != want {
		t.Errorf("error summary = %q, want %q", got, want)
	}
	if got, want := failingChecksSummary(checks, ard.SeverityWarning), "queries.count"; got != want {
		t.Errorf("warning summary = %q, want %q", got, want)
	}
}

func TestCatalogEvent_ValidCatalogReportsEntryCount(t *testing.T) {
	ev := catalogEvent("acme.com", probe.MethodWellKnown, ard.Report{Verdict: ard.VerdictValid}, 14)
	if ev.Detail != "14 entries" {
		t.Errorf("detail = %q, want \"14 entries\"", ev.Detail)
	}
	if ev.Verdict != ard.VerdictValid || ev.Outcome != probe.OutcomeHit || ev.Method != probe.MethodWellKnown {
		t.Errorf("unexpected event %+v", ev)
	}

	single := catalogEvent("acme.com", probe.MethodWellKnown, ard.Report{Verdict: ard.VerdictValid}, 1)
	if single.Detail != "1 entry" {
		t.Errorf("detail = %q, want \"1 entry\"", single.Detail)
	}
}
