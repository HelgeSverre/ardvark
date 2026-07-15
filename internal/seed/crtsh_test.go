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
