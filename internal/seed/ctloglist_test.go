package seed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testLogList = `{
  "operators": [
    {
      "name": "Let's Encrypt",
      "logs": [
        {
          "description": "Let's Encrypt 'Oak2026h1'",
          "url": "https://oak.ct.letsencrypt.org/2026h1/",
          "state": {"usable": {"timestamp": "2025-01-01T00:00:00Z"}},
          "temporal_interval": {"start_inclusive": "2026-01-01T00:00:00Z", "end_exclusive": "2026-07-01T00:00:00Z"}
        },
        {
          "description": "Let's Encrypt 'Oak2026h2'",
          "url": "https://oak.ct.letsencrypt.org/2026h2/",
          "state": {"usable": {"timestamp": "2025-01-01T00:00:00Z"}},
          "temporal_interval": {"start_inclusive": "2026-07-01T00:00:00Z", "end_exclusive": "2027-01-01T00:00:00Z"}
        }
      ]
    },
    {
      "name": "Google",
      "logs": [
        {
          "description": "Google 'Argon2026h2'",
          "url": "https://ct.googleapis.com/logs/us1/argon2026h2/",
          "state": {"usable": {"timestamp": "2025-01-01T00:00:00Z"}},
          "temporal_interval": {"start_inclusive": "2026-07-01T00:00:00Z", "end_exclusive": "2027-01-01T00:00:00Z"}
        },
        {
          "description": "Google 'Xenon2026h2' (readonly)",
          "url": "https://ct.googleapis.com/logs/us1/xenon2026h2/",
          "state": {"readonly": {"timestamp": "2025-01-01T00:00:00Z"}},
          "temporal_interval": {"start_inclusive": "2026-07-01T00:00:00Z", "end_exclusive": "2027-01-01T00:00:00Z"}
        }
      ]
    }
  ]
}`

func logListServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testLogList))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// now falls in the 2026h2 shards.
var testNow = time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)

func TestResolveCTLogs(t *testing.T) {
	srv := logListServer(t)

	cases := []struct {
		name      string
		operators []string
		want      []string
	}{
		{
			name:      "oak selects only the current oak shard",
			operators: []string{"oak"},
			want:      []string{"https://oak.ct.letsencrypt.org/2026h2/"},
		},
		{
			name:      "all selects every usable current log (readonly excluded)",
			operators: []string{"all"},
			want: []string{
				"https://oak.ct.letsencrypt.org/2026h2/",
				"https://ct.googleapis.com/logs/us1/argon2026h2/",
			},
		},
		{
			name:      "argon token matches by description",
			operators: []string{"argon"},
			want:      []string{"https://ct.googleapis.com/logs/us1/argon2026h2/"},
		},
		{
			name:      "empty filter behaves like all",
			operators: nil,
			want: []string{
				"https://oak.ct.letsencrypt.org/2026h2/",
				"https://ct.googleapis.com/logs/us1/argon2026h2/",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveCTLogs(context.Background(), srv.Client(), srv.URL, tc.operators, testNow)
			if err != nil {
				t.Fatalf("ResolveCTLogs: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveCTLogsNoMatch(t *testing.T) {
	srv := logListServer(t)
	if _, err := ResolveCTLogs(context.Background(), srv.Client(), srv.URL, []string{"nonesuch"}, testNow); err == nil {
		t.Fatal("expected error when no log matches, got nil")
	}
}

func TestResolveCTLogsExcludesExpiredShard(t *testing.T) {
	srv := logListServer(t)
	// A time in the first half of 2026 with no more-distant shard in the
	// fixture to accidentally pull in; only the current h1 shard should be
	// selected (h2 is a valid "nearest future" candidate too — see
	// TestResolveCTLogsIncludesNextShardNearBoundary — but that's a
	// distinct assertion from "expired/past shards aren't selected").
	early := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	got, err := ResolveCTLogs(context.Background(), srv.Client(), srv.URL, []string{"oak"}, early)
	if err != nil {
		t.Fatalf("ResolveCTLogs: %v", err)
	}
	if len(got) == 0 || got[0] != "https://oak.ct.letsencrypt.org/2026h1/" {
		t.Fatalf("got %v, want current shard first", got)
	}
}

// TestResolveCTLogsExcludesDistantFutureShard proves selectOperatorShards
// stays conservative: a usable shard starting more than ctFreshCertWindow
// away isn't "about" freshly issued certs, so it must not be pulled in just
// because it's the nearest future shard.
func TestResolveCTLogsExcludesDistantFutureShard(t *testing.T) {
	const distantFutureLogList = `{
	  "operators": [
	    {
	      "name": "Let's Encrypt",
	      "logs": [
	        {
	          "description": "Let's Encrypt 'Oak2026h1'",
	          "url": "https://oak.ct.letsencrypt.org/2026h1/",
	          "state": {"usable": {"timestamp": "2025-01-01T00:00:00Z"}},
	          "temporal_interval": {"start_inclusive": "2026-01-01T00:00:00Z", "end_exclusive": "2026-07-01T00:00:00Z"}
	        },
	        {
	          "description": "Let's Encrypt 'Oak2027h2' (far future)",
	          "url": "https://oak.ct.letsencrypt.org/2027h2/",
	          "state": {"usable": {"timestamp": "2025-01-01T00:00:00Z"}},
	          "temporal_interval": {"start_inclusive": "2027-07-01T00:00:00Z", "end_exclusive": "2028-01-01T00:00:00Z"}
	        }
	      ]
	    }
	  ]
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(distantFutureLogList))
	}))
	defer srv.Close()

	early := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	got, err := ResolveCTLogs(context.Background(), srv.Client(), srv.URL, []string{"oak"}, early)
	if err != nil {
		t.Fatalf("ResolveCTLogs: %v", err)
	}
	want := []string{"https://oak.ct.letsencrypt.org/2026h1/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (far-future shard must be excluded)", got, want)
	}
}

// TestResolveCTLogsIncludesNextShardNearBoundary proves the coversNow fix
// (docs/FOLLOWUPS.md "CT coversNow shard selection"): near the h1/h2
// half-year boundary, a freshly issued long-validity cert can already land
// in the *next* shard even though its window hasn't started by wall clock,
// because shards partition by certificate NotAfter, not issuance. Within
// ctFreshCertWindow (~400 days) of the next shard's start, both the current
// and upcoming shard must be selected so freshly-issued certs aren't
// missed.
func TestResolveCTLogsIncludesNextShardNearBoundary(t *testing.T) {
	srv := logListServer(t)
	// h2 starts 2026-07-01; this is well within 400 days of that boundary,
	// so h2 should be pulled in alongside the current h1 shard.
	nearBoundary := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	got, err := ResolveCTLogs(context.Background(), srv.Client(), srv.URL, []string{"oak"}, nearBoundary)
	if err != nil {
		t.Fatalf("ResolveCTLogs: %v", err)
	}
	want := []string{
		"https://oak.ct.letsencrypt.org/2026h1/",
		"https://oak.ct.letsencrypt.org/2026h2/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestResolveCTLogsNoMatchListsUsableOperators exercises the friendly-error
// path (e.g. "seed ct --log oak" after Oak retires): the error should name
// which operators currently have a usable log, drawn from the same fetched
// list, instead of just failing silently.
func TestResolveCTLogsNoMatchListsUsableOperators(t *testing.T) {
	srv := logListServer(t)
	_, err := ResolveCTLogs(context.Background(), srv.Client(), srv.URL, []string{"definitely-retired"}, testNow)
	if err == nil {
		t.Fatal("expected error when no log matches")
	}
	for _, want := range []string{"Let's Encrypt", "Google"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention usable operator %q", err.Error(), want)
		}
	}
}
