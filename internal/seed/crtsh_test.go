package seed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCrtshSeederDomains(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		records := []crtshRecord{
			{CommonName: "agent.example.com"},
			{NameValue: "mcp.example.com\n*.mcp.example.com"},
			{CommonName: "not-a-hostname"}, // dropped: no dot
		}
		_ = json.NewEncoder(w).Encode(records)
	}))
	defer srv.Close()

	seeder := &CrtshSeeder{Endpoint: srv.URL, Match: "mcp"}
	names, err := seeder.Domains(context.Background(), 10)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}

	want := map[string]bool{"agent.example.com": true, "mcp.example.com": true}
	if len(names) != len(want) {
		t.Fatalf("got %v, want set %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected domain %q", n)
		}
	}

	if gotQuery != "%mcp%" {
		t.Errorf("q param = %q, want %%mcp%%", gotQuery)
	}
	if seeder.Source() != "crtsh" {
		t.Errorf("Source() = %q, want crtsh", seeder.Source())
	}
}

func TestCrtshSeederDomains_TruncatesToN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := []crtshRecord{
			{CommonName: "one.example.com"},
			{CommonName: "two.example.com"},
			{CommonName: "three.example.com"},
		}
		_ = json.NewEncoder(w).Encode(records)
	}))
	defer srv.Close()

	seeder := &CrtshSeeder{Endpoint: srv.URL}
	names, err := seeder.Domains(context.Background(), 2)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("got %d names, want 2: %v", len(names), names)
	}
}

func TestCrtshSeederDomains_RejectsNonPositiveN(t *testing.T) {
	seeder := &CrtshSeeder{Endpoint: "http://example.invalid"}
	if _, err := seeder.Domains(context.Background(), 0); err == nil {
		t.Fatal("expected error for n=0")
	}
}

// TestCrtshSeederDomains_MultipleMatchesQueriesEachKeyword proves the fix
// for docs/FOLLOWUPS.md "seed crtsh with no --match": a curated multi-
// keyword set is queried one keyword at a time (crt.sh serves one identity
// pattern per request) and results are merged, rather than one bare "q=%"
// wildcard.
func TestCrtshSeederDomains_MultipleMatchesQueriesEachKeyword(t *testing.T) {
	var gotQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		gotQueries = append(gotQueries, q)
		var records []crtshRecord
		switch q {
		case "%agent%":
			records = []crtshRecord{{CommonName: "agent.example.com"}}
		case "%mcp%":
			records = []crtshRecord{{CommonName: "mcp.example.com"}}
		}
		_ = json.NewEncoder(w).Encode(records)
	}))
	defer srv.Close()

	seeder := &CrtshSeeder{Endpoint: srv.URL, Matches: []string{"agent", "mcp"}}
	names, err := seeder.Domains(context.Background(), 10)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}

	want := map[string]bool{"agent.example.com": true, "mcp.example.com": true}
	if len(names) != len(want) {
		t.Fatalf("got %v, want set %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected domain %q", n)
		}
	}

	wantQueries := []string{"%agent%", "%mcp%"}
	if len(gotQueries) != len(wantQueries) {
		t.Fatalf("queries = %v, want %v", gotQueries, wantQueries)
	}
	for i := range wantQueries {
		if gotQueries[i] != wantQueries[i] {
			t.Fatalf("queries = %v, want %v", gotQueries, wantQueries)
		}
	}
}

// TestCrtshSeederDomains_StopsEarlyOnceNSatisfied proves the second keyword
// is never queried once the first already satisfied n, bounding request
// volume.
func TestCrtshSeederDomains_StopsEarlyOnceNSatisfied(t *testing.T) {
	var gotQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		gotQueries = append(gotQueries, q)
		records := []crtshRecord{{CommonName: "agent.example.com"}, {CommonName: "agent2.example.com"}}
		_ = json.NewEncoder(w).Encode(records)
	}))
	defer srv.Close()

	seeder := &CrtshSeeder{Endpoint: srv.URL, Matches: []string{"agent", "mcp", "ai-catalog"}}
	names, err := seeder.Domains(context.Background(), 2)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("got %d names, want 2: %v", len(names), names)
	}
	if len(gotQueries) != 1 {
		t.Fatalf("queries = %v, want only the first keyword queried", gotQueries)
	}
}

func TestCrtshSeederDomains_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	seeder := &CrtshSeeder{Endpoint: srv.URL}
	if _, err := seeder.Domains(context.Background(), 1); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}
