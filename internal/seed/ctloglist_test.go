package seed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	// A time in the first half of 2026 should select only the h1 oak shard.
	h1 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	got, err := ResolveCTLogs(context.Background(), srv.Client(), srv.URL, []string{"oak"}, h1)
	if err != nil {
		t.Fatalf("ResolveCTLogs: %v", err)
	}
	want := []string{"https://oak.ct.letsencrypt.org/2026h1/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
