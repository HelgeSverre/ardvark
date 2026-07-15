package seed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const mcpRegistryFixturePage1 = `{
  "servers": [
    {
      "server": {
        "name": "com.acme.tools/foo-server",
        "remotes": [{"url": "https://mcp.acme.example/sse"}]
      }
    },
    {
      "server": {
        "name": "io.github.exampledev/bar-server",
        "remotes": []
      }
    },
    {
      "server": {
        "name": "not-namespaced",
        "remotes": []
      }
    }
  ],
  "metadata": {"next_cursor": "page2"}
}`

const mcpRegistryFixturePage2 = `{
  "servers": [
    {
      "server": {
        "name": "com.other/baz-server",
        "remotes": [{"url": "https://api.other.example/mcp"}]
      }
    }
  ],
  "metadata": {"next_cursor": ""}
}`

func TestMCPRegistrySeederDomains(t *testing.T) {
	var gotCursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		gotCursors = append(gotCursors, cursor)
		w.Header().Set("Content-Type", "application/json")
		if cursor == "" {
			_, _ = w.Write([]byte(mcpRegistryFixturePage1))
		} else {
			_, _ = w.Write([]byte(mcpRegistryFixturePage2))
		}
	}))
	defer srv.Close()

	seeder := &MCPRegistrySeeder{RegistryURL: srv.URL}
	names, err := seeder.Domains(context.Background(), 10)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}

	want := map[string]bool{
		"mcp.acme.example":     true, // remote host
		"tools.acme.com":       true, // reversed name
		"exampledev.github.io": true, // reversed io.github.* name
		"api.other.example":    true, // remote host page 2
		"other.com":            true, // reversed name page 2
	}
	if len(names) != len(want) {
		t.Fatalf("got %v, want set %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected domain %q", n)
		}
	}

	if len(gotCursors) != 2 {
		t.Fatalf("cursors = %v, want two page fetches", gotCursors)
	}
	if seeder.Source() != "mcp_registry" {
		t.Errorf("Source() = %q, want mcp_registry", seeder.Source())
	}
}

func TestMCPRegistrySeederDomains_RejectsNonPositiveN(t *testing.T) {
	seeder := &MCPRegistrySeeder{RegistryURL: "http://example.invalid"}
	if _, err := seeder.Domains(context.Background(), 0); err == nil {
		t.Fatal("expected error for n=0")
	}
}

func TestMCPRegistrySeederDomains_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	seeder := &MCPRegistrySeeder{RegistryURL: srv.URL}
	if _, err := seeder.Domains(context.Background(), 1); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestDomainFromReverseDNSName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"two-label reverses to registrable domain", "com.acme/foo-server", "acme.com"},
		{"three-label reverses fully", "com.acme.tools/foo-server", "tools.acme.com"},
		{"github namespace reverses to pages host", "io.github.exampledev/bar-server", "exampledev.github.io"},
		{"no slash yields nothing", "not-namespaced", ""},
		{"single label yields nothing", "acme/foo-server", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domainFromReverseDNSName(tc.in); got != tc.want {
				t.Errorf("domainFromReverseDNSName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
