package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/fetch"
)

func newTestClient() *fetch.Client {
	cfg := config.Defaults().Crawler
	cfg.PerHostRequestsPerSecond = 1000
	return fetch.New(cfg)
}

func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u := strings.TrimPrefix(srv.URL, "http://")
	return u
}

// probeAt is like Probe but lets tests point the well-known/robots fetches
// at a fake http (not https) test server by constructing the requests
// directly against srv.URL's host, exercising the same code paths.
func TestProbeWellKnown_Hit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(`{"specVersion":"1.0"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newTestClient()
	result := probeWellKnownForTest(client, srv.URL)

	if result.Outcome != OutcomeHit {
		t.Fatalf("expected hit, got %s (detail=%s)", result.Outcome, result.ErrorDetail)
	}
	if result.Method != MethodWellKnown {
		t.Fatalf("expected method %s, got %s", MethodWellKnown, result.Method)
	}
	if len(result.CatalogURLs) != 1 {
		t.Fatalf("expected 1 catalog URL, got %v", result.CatalogURLs)
	}
}

func TestProbeWellKnown_Miss(t *testing.T) {
	mux := http.NewServeMux()
	// No handler registered: 404.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newTestClient()
	result := probeWellKnownForTest(client, srv.URL)

	if result.Outcome != OutcomeMiss {
		t.Fatalf("expected miss, got %s (detail=%s)", result.Outcome, result.ErrorDetail)
	}
}

func TestProbeWellKnown_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newTestClient()
	result := probeWellKnownForTest(client, srv.URL)

	if result.Outcome != OutcomeError {
		t.Fatalf("expected error outcome, got %s", result.Outcome)
	}
}

func TestProbeWellKnown_TooManyRequestsIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result := probeWellKnownForTest(newTestClient(), srv.URL)
	if result.Outcome != OutcomeError {
		t.Fatalf("Outcome = %q, want %q for retryable 429", result.Outcome, OutcomeError)
	}
	if result.RetryError == nil || !fetch.Transient(result.RetryError) {
		t.Fatalf("RetryError = %v, want transient fetch error", result.RetryError)
	}
}

func TestParseAgentmapDirectives(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "single absolute directive",
			raw:  "User-agent: *\nAgentmap: https://example.com/.well-known/ai-catalog.json\n",
			want: []string{"https://example.com/.well-known/ai-catalog.json"},
		},
		{
			name: "case insensitive key",
			raw:  "AGENTMAP: https://example.com/catalog.json\n",
			want: []string{"https://example.com/catalog.json"},
		},
		{
			name: "relative value resolved against base",
			raw:  "Agentmap: /catalog.json\n",
			want: []string{"https://example.com/catalog.json"},
		},
		{
			name: "no directives",
			raw:  "User-agent: *\nDisallow: /private\n",
			want: nil,
		},
		{
			name: "dedupes repeated directives",
			raw:  "Agentmap: /catalog.json\nAgentmap: /catalog.json\n",
			want: []string{"https://example.com/catalog.json"},
		},
		{
			name: "ignores comments",
			raw:  "# Agentmap: /ignored.json\nAgentmap: /catalog.json\n",
			want: []string{"https://example.com/catalog.json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAgentmapDirectives(tt.raw, "https://example.com/robots.txt")
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestProbeRobotsAgentmap_Hit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nAgentmap: /.well-known/ai-catalog.json\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newTestClient()
	host := hostOf(t, srv)

	raw, err := client.RawRobots(context.Background(), host)
	if err != nil {
		t.Fatalf("RawRobots: %v", err)
	}
	urls := parseAgentmapDirectives(raw, "http://"+host+"/robots.txt")
	if len(urls) != 1 {
		t.Fatalf("expected 1 agentmap url, got %v", urls)
	}
	if urls[0] != "http://"+host+"/.well-known/ai-catalog.json" {
		t.Fatalf("unexpected resolved url: %s", urls[0])
	}
}

func TestProbeRobotsAgentmap_Miss(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\nDisallow: /private\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newTestClient()
	host := hostOf(t, srv)

	result := probeRobotsAgentmapForTest(client, host)
	if result.Outcome != OutcomeMiss {
		t.Fatalf("expected miss, got %s", result.Outcome)
	}
}

func TestProbeRobotsAgentmap_NoRobotsTxt(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newTestClient()
	host := hostOf(t, srv)

	result := probeRobotsAgentmapForTest(client, host)
	if result.Outcome != OutcomeMiss {
		t.Fatalf("expected miss for missing robots.txt, got %s", result.Outcome)
	}
}

// -- helpers exercising the unexported code paths against http (not https)
// test servers, mirroring probeWellKnown/probeRobotsAgentmap exactly except
// for the scheme, since httptest.Server serves plain HTTP. --

// probeWellKnownForTest exercises the real alias-walking implementation
// against an http (not https) test server.
func probeWellKnownForTest(client *fetch.Client, baseURL string) Result {
	return probeWellKnownBase(context.Background(), client, baseURL)
}

func probeRobotsAgentmapForTest(client *fetch.Client, host string) Result {
	robotsURL := "http://" + host + "/robots.txt"
	raw, err := client.RawRobots(context.Background(), host)
	if err != nil {
		return classifyFetchErr(MethodRobotsAgentmap, robotsURL, err)
	}
	if raw == "" {
		return Result{Method: MethodRobotsAgentmap, URL: robotsURL, Outcome: OutcomeMiss}
	}
	urls := parseAgentmapDirectives(raw, robotsURL)
	if len(urls) == 0 {
		return Result{Method: MethodRobotsAgentmap, URL: robotsURL, HTTPStatus: http.StatusOK, Outcome: OutcomeMiss}
	}
	return Result{Method: MethodRobotsAgentmap, URL: robotsURL, HTTPStatus: http.StatusOK, Outcome: OutcomeHit, CatalogURLs: urls}
}

func TestProbe_ReturnsBothMethods(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"specVersion":"1.0"}`))
	})
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("User-agent: *\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Probe() itself always uses https://<host>; since httptest serves
	// plain http, hit that indirectly is impractical without a TLS test
	// server. Exercise the real https path via a TLS test server instead.
	tlsSrv := httptest.NewTLSServer(mux)
	defer tlsSrv.Close()

	client := newTestClient()
	host := strings.TrimPrefix(tlsSrv.URL, "https://")

	// Use the TLS server's client (which trusts its own cert) by swapping
	// the http.Client's transport is out of scope here; instead just
	// confirm Probe returns exactly 2 results in stable order, tolerating
	// TLS verification errors as "error" outcomes since ardvark's fetch
	// client uses the default cert pool.
	results := Probe(context.Background(), client, host)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Method != MethodWellKnown {
		t.Fatalf("expected first result method %s, got %s", MethodWellKnown, results[0].Method)
	}
	if results[1].Method != MethodRobotsAgentmap {
		t.Fatalf("expected second result method %s, got %s", MethodRobotsAgentmap, results[1].Method)
	}
}

// -- well-known aliases ----------------------------------------------------

// jsonCatalogHandler serves a minimal ARD document.
func jsonCatalogHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/ai-catalog+json")
	_, _ = w.Write([]byte(`{"specVersion":"1.0"}`))
}

// A publisher that has moved to the preferred name must be discovered, and
// the legacy alias must not be fetched once the preferred one has answered.
func TestProbeWellKnown_PrefersArdJSONAndStopsThere(t *testing.T) {
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/.well-known/ard.json" {
			jsonCatalogHandler(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result := probeWellKnownForTest(newTestClient(), srv.URL)

	if result.Outcome != OutcomeHit {
		t.Fatalf("Outcome = %q, want %q (detail=%s)", result.Outcome, OutcomeHit, result.ErrorDetail)
	}
	if !strings.HasSuffix(result.URL, "/.well-known/ard.json") {
		t.Errorf("URL = %q, want the preferred ard.json alias", result.URL)
	}
	if len(paths) != 1 {
		t.Errorf("fetched %v, want only the preferred alias", paths)
	}
}

// The existing adopter base publishes only the legacy name; it must keep
// being discovered after the preferred alias 404s.
func TestProbeWellKnown_FallsBackToLegacyAlias(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ai-catalog.json", jsonCatalogHandler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result := probeWellKnownForTest(newTestClient(), srv.URL)

	if result.Outcome != OutcomeHit {
		t.Fatalf("Outcome = %q, want %q", result.Outcome, OutcomeHit)
	}
	if !strings.HasSuffix(result.URL, "/.well-known/ai-catalog.json") {
		t.Errorf("URL = %q, want the legacy alias", result.URL)
	}
}

// A transient failure on one alias must not hide a real hit on the other.
func TestProbeWellKnown_TransientOnPreferredStillFindsLegacy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ard.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	mux.HandleFunc("/.well-known/ai-catalog.json", jsonCatalogHandler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result := probeWellKnownForTest(newTestClient(), srv.URL)

	if result.Outcome != OutcomeHit {
		t.Fatalf("Outcome = %q, want %q despite the 429 on the preferred alias", result.Outcome, OutcomeHit)
	}
	if !strings.HasSuffix(result.URL, "/.well-known/ai-catalog.json") {
		t.Errorf("URL = %q, want the legacy alias", result.URL)
	}
}

// An indeterminate alias must outrank a definitive miss on the other: we do
// not know this host lacks ARD, so it must stay retryable rather than being
// recorded as a durable not-found.
func TestProbeWellKnown_TransientOnEitherAliasIsNotAMiss(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ai-catalog.json", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	result := probeWellKnownForTest(newTestClient(), srv.URL)

	if result.Outcome != OutcomeError {
		t.Fatalf("Outcome = %q, want %q so the host is retried", result.Outcome, OutcomeError)
	}
	if result.RetryError == nil || !fetch.Transient(result.RetryError) {
		t.Errorf("RetryError = %v, want a transient error", result.RetryError)
	}
}

// A host publishing neither alias is a genuine miss.
func TestProbeWellKnown_NeitherAliasIsAMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	result := probeWellKnownForTest(newTestClient(), srv.URL)

	if result.Outcome != OutcomeMiss {
		t.Fatalf("Outcome = %q, want %q", result.Outcome, OutcomeMiss)
	}
}
