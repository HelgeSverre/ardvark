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

func probeWellKnownForTest(client *fetch.Client, baseURL string) Result {
	wellKnownURL := baseURL + wellKnownPath
	fetched, err := client.GetWellKnown(context.Background(), wellKnownURL)
	if err != nil {
		return classifyFetchErr(MethodWellKnown, wellKnownURL, err)
	}
	return Result{
		Method:      MethodWellKnown,
		URL:         fetched.URL,
		HTTPStatus:  fetched.Status,
		ContentType: fetched.ContentType,
		Outcome:     OutcomeHit,
		CatalogURLs: []string{fetched.URL},
	}
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
