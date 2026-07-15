package seed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// curatedFixtureMarkdown mixes infrastructure links (repos, badges, social,
// glama.ai mirrors) with real product domains, the shape of a genuine
// awesome-mcp-servers README.
const curatedFixtureMarkdown = `# Awesome MCP Servers

[![stars](https://img.shields.io/github/stars/punkpeye/awesome-mcp-servers)](https://github.com/punkpeye/awesome-mcp-servers)

- [TickTick](https://ticktick.com/mcp) - task management ([repo](https://github.com/acme/ticktick-mcp), [glama](https://glama.ai/mcp/servers/abc123))
- [Langfuse](https://langfuse.com/docs/mcp) - observability ([discord](https://discord.gg/xyz))
- [Project Site](https://project.github.io/docs) - a real project page on GitHub Pages
- Duplicate link: https://ticktick.com/other/page
- Plain text URL http://plain.example.com:8080/path and a video https://www.youtube.com/watch?v=1
- Subdomain infra: https://gist.github.com/acme/deadbeef
`

func TestCuratedSeederDomains_BlocklistAndDedupe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(curatedFixtureMarkdown))
	}))
	defer srv.Close()

	seeder := NewCuratedSeeder([]string{srv.URL})
	names, err := seeder.Domains(context.Background(), 100)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}

	want := []string{"ticktick.com", "langfuse.com", "project.github.io", "plain.example.com"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}

	if seeder.Source() != "curated_list" {
		t.Errorf("Source() = %q, want curated_list", seeder.Source())
	}
}

func TestCuratedSeederDomains_CountCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(curatedFixtureMarkdown))
	}))
	defer srv.Close()

	seeder := NewCuratedSeeder([]string{srv.URL})
	names, err := seeder.Domains(context.Background(), 2)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 2 || names[0] != "ticktick.com" || names[1] != "langfuse.com" {
		t.Fatalf("got %v, want [ticktick.com langfuse.com]", names)
	}
}

// Explicit ListURLs must REPLACE the default set, not extend it: with a
// single fixture URL configured, only that server is fetched (any request
// for a default list would fail loudly since the defaults aren't reachable
// from the fixture server).
func TestCuratedSeederDomains_ExplicitURLsReplaceDefaults(t *testing.T) {
	var requests []string
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, "a")
		w.Write([]byte("see https://only-a.example.com/x"))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, "b")
		w.Write([]byte("see https://only-b.example.com/x"))
	}))
	defer srvB.Close()

	seeder := NewCuratedSeeder([]string{srvA.URL})
	names, err := seeder.Domains(context.Background(), 100)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 1 || names[0] != "only-a.example.com" {
		t.Fatalf("got %v, want [only-a.example.com]", names)
	}
	if len(requests) != 1 || requests[0] != "a" {
		t.Fatalf("requests = %v, want only server A fetched", requests)
	}
}

// Multiple lists are scanned in order, deduping across them, and scanning
// stops once the count is reached.
func TestCuratedSeederDomains_MultipleLists(t *testing.T) {
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("https://shared.example.com/a https://first.example.com/a"))
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("https://shared.example.com/b https://second.example.com/b"))
	}))
	defer srvB.Close()

	seeder := NewCuratedSeeder([]string{srvA.URL, srvB.URL})
	names, err := seeder.Domains(context.Background(), 100)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	want := []string{"shared.example.com", "first.example.com", "second.example.com"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestCuratedSeederDomains_RejectsNonPositiveN(t *testing.T) {
	seeder := NewCuratedSeeder([]string{"http://example.invalid"})
	if _, err := seeder.Domains(context.Background(), 0); err == nil {
		t.Fatal("expected error for n=0")
	}
}

func TestCuratedSeederDomains_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	seeder := NewCuratedSeeder([]string{srv.URL})
	if _, err := seeder.Domains(context.Background(), 10); err == nil {
		t.Fatal("expected error for 503 list fetch")
	}
}
