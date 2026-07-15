package seed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const githubSearchFixturePage1 = `{
  "total_count": 3,
  "items": [
    {
      "repository": {
        "full_name": "acme/agent-site",
        "homepage": "https://agents.acme.example",
        "html_url": "https://github.com/acme/agent-site"
      }
    },
    {
      "repository": {
        "full_name": "octocat/octocat.github.io",
        "homepage": "",
        "html_url": "https://github.com/octocat/octocat.github.io"
      }
    },
    {
      "repository": {
        "full_name": "someorg/no-homepage",
        "homepage": "",
        "html_url": "https://github.com/someorg/no-homepage"
      }
    }
  ]
}`

func TestGitHubSeederDomains(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(githubSearchFixturePage1))
	}))
	defer srv.Close()

	seeder := &GitHubSeeder{Endpoint: srv.URL, Token: "test-token"}
	names, err := seeder.Domains(context.Background(), 10)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}

	want := map[string]bool{
		"agents.acme.example": true,
		"octocat.github.io":   true,
	}
	if len(names) != len(want) {
		t.Fatalf("got %v, want set %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected domain %q", n)
		}
	}

	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization header = %q, want Bearer test-token", gotAuth)
	}
	if gotQuery != githubDefaultQuery {
		t.Errorf("q param = %q, want %q", gotQuery, githubDefaultQuery)
	}
	if seeder.Source() != "github" {
		t.Errorf("Source() = %q, want github", seeder.Source())
	}
}

func TestGitHubSeederDomains_UsesEnvToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
	}))
	defer srv.Close()

	t.Setenv("GITHUB_TOKEN", "env-token")
	seeder := &GitHubSeeder{Endpoint: srv.URL}
	if _, err := seeder.Domains(context.Background(), 1); err != nil {
		t.Fatalf("Domains: %v", err)
	}
}

func TestGitHubSeederDomains_NoTokenErrors(t *testing.T) {
	// Ensure a clean environment regardless of what's set on the host
	// running the test.
	t.Setenv("GITHUB_TOKEN", "")
	os.Unsetenv("GITHUB_TOKEN")

	seeder := &GitHubSeeder{Endpoint: "http://example.invalid"}
	_, err := seeder.Domains(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error when GITHUB_TOKEN is unset")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("error = %q, want mention of GITHUB_TOKEN", err.Error())
	}
}

func TestGitHubSeederDomains_RejectsNonPositiveN(t *testing.T) {
	seeder := &GitHubSeeder{Token: "t"}
	if _, err := seeder.Domains(context.Background(), 0); err == nil {
		t.Fatal("expected error for n=0")
	}
}

func TestGitHubSeederDomains_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	seeder := &GitHubSeeder{Endpoint: srv.URL, Token: "t"}
	if _, err := seeder.Domains(context.Background(), 1); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

func TestDomainFromRepository(t *testing.T) {
	cases := []struct {
		name string
		repo githubRepository
		want string
	}{
		{
			name: "homepage host wins",
			repo: githubRepository{FullName: "acme/site", Homepage: "https://agents.acme.example/path"},
			want: "agents.acme.example",
		},
		{
			name: "github pages fallback",
			repo: githubRepository{FullName: "octocat/octocat.github.io"},
			want: "octocat.github.io",
		},
		{
			name: "no derivable domain",
			repo: githubRepository{FullName: "someorg/tools"},
			want: "",
		},
		{
			name: "malformed homepage falls through",
			repo: githubRepository{FullName: "someorg/tools", Homepage: "not a url\x7f"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domainFromRepository(tc.repo); got != tc.want {
				t.Errorf("domainFromRepository(%+v) = %q, want %q", tc.repo, got, tc.want)
			}
		})
	}
}
