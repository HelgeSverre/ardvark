package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func testConfig() config.CrawlerConfig {
	return config.CrawlerConfig{
		Concurrency:              4,
		MaxDepth:                 2,
		MaxPagesPerDomain:        50,
		PerHostRequestsPerSecond: 1000, // fast by default; specific tests override
		RequestTimeoutSeconds:    5,
		MaxBodyBytes:             1024,
		UserAgent:                "ardvark-test/0.1",
		RespectRobotsTxt:         true,
		RefreshAfterHours:        168,
	}
}

func TestGet_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(testConfig())
	fetched, err := c.Get(context.Background(), srv.URL+"/foo")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if fetched.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", fetched.Status)
	}
	if fetched.ContentType != "application/json" {
		t.Errorf("ContentType = %q", fetched.ContentType)
	}
	if string(fetched.Body) != `{"ok":true}` {
		t.Errorf("Body = %q", fetched.Body)
	}
	if fetched.SHA256 == "" {
		t.Error("SHA256 is empty")
	}
	if fetched.URL != srv.URL+"/foo" {
		t.Errorf("URL = %q", fetched.URL)
	}
}

func TestGet_BodyTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 2048))
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.MaxBodyBytes = 100
	c := New(cfg)

	_, err := c.Get(context.Background(), srv.URL+"/big")
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge", err)
	}
	if Transient(err) {
		t.Error("body-too-large should be permanent, not transient")
	}
}

func TestGet_RedirectCap(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	// Register a chain of 10 redirects, more than the cap of 5.
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/hop/0", http.StatusFound)
	})
	for i := 0; i < 10; i++ {
		i := i
		mux.HandleFunc(fmt.Sprintf("/hop/%d", i), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, fmt.Sprintf("%s/hop/%d", srv.URL, i+1), http.StatusFound)
		})
	}
	mux.HandleFunc("/hop/10", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c := New(testConfig())
	_, err := c.Get(context.Background(), srv.URL+"/start")
	if err == nil {
		t.Fatal("expected redirect cap error, got nil")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("err = %v, want redirect-related message", err)
	}
}

func TestGet_RedirectWithinCap(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})

	c := New(testConfig())
	fetched, err := c.Get(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if fetched.URL != srv.URL+"/final" {
		t.Errorf("URL = %q, want final redirect target", fetched.URL)
	}
}

func TestGet_TransientAndPermanentStatus(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		wantTransient bool
	}{
		{"internal server error", http.StatusInternalServerError, true},
		{"bad gateway", http.StatusBadGateway, true},
		{"too many requests", http.StatusTooManyRequests, true},
		{"not found", http.StatusNotFound, false},
		{"forbidden", http.StatusForbidden, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mux http.ServeMux
			mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			})
			mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			})
			srv := httptest.NewServer(&mux)
			defer srv.Close()

			c := New(testConfig())
			_, err := c.Get(context.Background(), srv.URL+"/status")
			if err == nil {
				t.Fatalf("expected error for status %d", tt.status)
			}
			if got := Transient(err); got != tt.wantTransient {
				t.Errorf("Transient(err) = %v, want %v (status %d)", got, tt.wantTransient, tt.status)
			}
		})
	}
}

func TestGet_DisallowedScheme(t *testing.T) {
	c := New(testConfig())
	_, err := c.Get(context.Background(), "ftp://example.com/file")
	if !errors.Is(err, ErrDisallowedScheme) {
		t.Fatalf("err = %v, want ErrDisallowedScheme", err)
	}
	if Transient(err) {
		t.Error("disallowed scheme should be permanent")
	}
}

func TestGet_TimeoutIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.RequestTimeoutSeconds = 0 // set manually below via context instead
	c := New(cfg)
	c.httpClient.Timeout = 50 * time.Millisecond

	_, err := c.Get(context.Background(), srv.URL+"/slow")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !Transient(err) {
		t.Errorf("expected timeout to be transient, err = %v", err)
	}
}

func TestRobots_Deny(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
	})
	mux.HandleFunc("/private/secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret"))
	})
	mux.HandleFunc("/public", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("public"))
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	c := New(testConfig())

	allowed, err := c.Allowed(context.Background(), srv.URL+"/private/secret")
	if err != nil {
		t.Fatalf("Allowed error: %v", err)
	}
	if allowed {
		t.Error("expected /private/secret to be disallowed")
	}

	allowed, err = c.Allowed(context.Background(), srv.URL+"/public")
	if err != nil {
		t.Fatalf("Allowed error: %v", err)
	}
	if !allowed {
		t.Error("expected /public to be allowed")
	}

	_, err = c.Get(context.Background(), srv.URL+"/private/secret")
	if !errors.Is(err, ErrRobotsDisallowed) {
		t.Fatalf("err = %v, want ErrRobotsDisallowed", err)
	}
}

func TestRobots_NoRobotsTxtAllowsAll(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/anything", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	c := New(testConfig())
	allowed, err := c.Allowed(context.Background(), srv.URL+"/anything")
	if err != nil {
		t.Fatalf("Allowed error: %v", err)
	}
	if !allowed {
		t.Error("expected allow when no robots.txt present")
	}
}

func TestRobots_TooManyRequestsNotCachedAsAllowAll(t *testing.T) {
	var hits int32

	var mux http.ServeMux
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
	})
	mux.HandleFunc("/private/secret", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret"))
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	c := New(testConfig())

	// First fetch of robots.txt is rate-limited: this must be refused, not
	// treated as "no robots.txt exists".
	_, err := c.Allowed(context.Background(), srv.URL+"/private/secret")
	if err == nil {
		t.Fatal("expected error on 429 robots.txt fetch, got nil")
	}
	if !Transient(err) {
		t.Fatalf("expected a transient error for 429, got %v (%T)", err, err)
	}

	// The 429 must not have poisoned the cache with an allow-all verdict.
	allowed, err := c.Allowed(context.Background(), srv.URL+"/private/secret")
	if err != nil {
		t.Fatalf("Allowed error on retry: %v", err)
	}
	if allowed {
		t.Error("expected /private/secret to be disallowed once robots.txt is honored")
	}
}

func TestRawRobots_TooManyRequestsDoesNotDowngradeToHTTP(t *testing.T) {
	var schemes []string

	c := New(testConfig())
	c.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		schemes = append(schemes, req.URL.Scheme)
		if req.URL.Scheme == "https" {
			return testResponse(req, http.StatusTooManyRequests, ""), nil
		}
		return testResponse(req, http.StatusNotFound, ""), nil
	})

	_, err := c.RawRobots(context.Background(), "example.test")
	if err == nil {
		t.Fatal("RawRobots error = nil, want transient HTTPS 429")
	}
	if !Transient(err) {
		t.Fatalf("RawRobots error = %v, want transient", err)
	}
	if len(schemes) != 1 || schemes[0] != "https" {
		t.Fatalf("requested schemes = %v, want [https] with no HTTP downgrade", schemes)
	}
}

func TestRobotsCache_IsScopedToOriginScheme(t *testing.T) {
	c := New(testConfig())
	c.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/robots.txt" {
			return testResponse(req, http.StatusOK, "page"), nil
		}
		if req.URL.Scheme == "https" {
			return testResponse(req, http.StatusOK, "User-agent: *\nDisallow: /private\n"), nil
		}
		return testResponse(req, http.StatusOK, "User-agent: *\nAllow: /\n"), nil
	})

	allowed, err := c.Allowed(context.Background(), "https://example.test/private")
	if err != nil {
		t.Fatalf("Allowed(https): %v", err)
	}
	if allowed {
		t.Fatal("Allowed(https) = true, want false")
	}

	allowed, err = c.Allowed(context.Background(), "http://example.test/private")
	if err != nil {
		t.Fatalf("Allowed(http): %v", err)
	}
	if !allowed {
		t.Fatal("Allowed(http) = false, want true from the HTTP origin's own robots.txt")
	}
}

func TestGet_RobotsTooManyRequestsIsRetryable(t *testing.T) {
	c := New(testConfig())
	c.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/robots.txt" {
			return testResponse(req, http.StatusTooManyRequests, ""), nil
		}
		return testResponse(req, http.StatusOK, "page"), nil
	})

	_, err := c.Get(context.Background(), "https://example.test/page")
	if err == nil {
		t.Fatal("Get error = nil, want robots.txt 429 to block this attempt")
	}
	if !Transient(err) {
		t.Fatalf("Get error = %v, want transient robots.txt failure", err)
	}
}

func TestRawRobots(t *testing.T) {
	const body = "User-agent: *\nDisallow: /nope\nAgentmap: /.well-known/ai-catalog.json\n"

	var mux http.ServeMux
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	u, err := parseHost(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	c := New(testConfig())
	raw, err := c.RawRobots(context.Background(), u)
	if err != nil {
		t.Fatalf("RawRobots error: %v", err)
	}
	if raw != body {
		t.Errorf("RawRobots = %q, want %q", raw, body)
	}

	// Cached path returns identical content.
	raw2, err := c.RawRobots(context.Background(), u)
	if err != nil {
		t.Fatalf("RawRobots (cached) error: %v", err)
	}
	if raw2 != body {
		t.Errorf("cached RawRobots = %q, want %q", raw2, body)
	}
}

func TestRateLimiting(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	cfg := testConfig()
	cfg.PerHostRequestsPerSecond = 5 // 5 req/s => ~200ms between requests
	c := New(cfg)

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.Get(context.Background(), srv.URL+"/ping"); err != nil {
			t.Fatalf("Get #%d error: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// 3 requests at 5rps with burst 1 should take at least ~2 intervals
	// (~400ms), well above what an unrated client would take.
	if elapsed < 300*time.Millisecond {
		t.Errorf("elapsed = %v, expected rate limiting to slow requests to >= 300ms", elapsed)
	}
}

// parseHost extracts the host:port portion of a test server URL, since
// RawRobots takes a bare host, not a full URL.
func parseHost(rawURL string) (string, error) {
	trimmed := strings.TrimPrefix(rawURL, "http://")
	trimmed = strings.TrimPrefix(trimmed, "https://")
	if trimmed == "" {
		return "", errors.New("empty host")
	}
	return trimmed, nil
}
