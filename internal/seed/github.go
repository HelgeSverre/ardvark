package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/helgesverre/ardvark/internal/store"
)

// githubDefaultSearchEndpoint is GitHub's code-search REST API.
const githubDefaultSearchEndpoint = "https://api.github.com/search/code"

// githubDefaultQuery targets the one artifact every ARD deployment is
// required to publish at a well-known path, making it the highest-
// precision seed source available: a hit is (almost) certainly a real ARD
// deployment, not just a keyword coincidence.
const githubDefaultQuery = "filename:ai-catalog.json path:.well-known"

// githubPerPage is GitHub code search's maximum page size.
const githubPerPage = 100

// githubMaxPages bounds how many pages are walked per Domains call,
// independent of n: GitHub code search caps results around 1000 per query
// regardless, so this just keeps a pathological n from spinning forever.
const githubMaxPages = 10

// GitHubSeeder queries the GitHub code-search API for files matching Query
// (defaulting to a search for ai-catalog.json under .well-known/) and
// extracts a candidate domain per matching repository: its homepage URL
// host, or — where the homepage is unset — a guessed GitHub Pages
// user-site host derived from the repo name, the one case a site domain is
// derivable without an extra API call per result. It implements Seeder with
// Source() "github".
type GitHubSeeder struct {
	// Query is the GitHub code-search query. Defaults to githubDefaultQuery
	// if empty.
	Query string

	// Token is the GitHub API token (a classic or fine-grained PAT with
	// public read access suffices). GitHub's code search API rejects
	// unauthenticated requests, so this must resolve to something.
	// Defaults to the GITHUB_TOKEN environment variable if empty.
	Token string

	// Endpoint is the code-search API base URL. Defaults to
	// githubDefaultSearchEndpoint if empty.
	Endpoint string

	// HTTPClient is used for all requests. Defaults to a client with a 30s
	// timeout if nil.
	HTTPClient *http.Client
}

// NewGitHubSeeder returns a GitHubSeeder for the given code-search query
// (empty uses githubDefaultQuery). The token is read from GITHUB_TOKEN.
func NewGitHubSeeder(query string) *GitHubSeeder {
	return &GitHubSeeder{Query: query}
}

// Source implements Seeder.
func (g *GitHubSeeder) Source() string { return store.DiscoverySourceGitHub }

func (g *GitHubSeeder) query() string {
	if g.Query != "" {
		return g.Query
	}
	return githubDefaultQuery
}

func (g *GitHubSeeder) endpoint() string {
	if g.Endpoint != "" {
		return g.Endpoint
	}
	return githubDefaultSearchEndpoint
}

func (g *GitHubSeeder) token() string {
	if g.Token != "" {
		return g.Token
	}
	return os.Getenv("GITHUB_TOKEN")
}

func (g *GitHubSeeder) httpClient() *http.Client {
	return newHTTPClient(g.HTTPClient, defaultHTTPTimeout)
}

type githubSearchResponse struct {
	Items []githubSearchItem `json:"items"`
}

type githubSearchItem struct {
	Repository githubRepository `json:"repository"`
}

type githubRepository struct {
	FullName string `json:"full_name"`
	Homepage string `json:"homepage"`
}

// Domains implements Seeder: it pages through GitHub code-search results
// for Query, extracts a candidate domain per matching repository, sanitizes
// and dedupes them, and returns up to n.
func (g *GitHubSeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: github: n must be positive, got %d", n)
	}
	token := g.token()
	if token == "" {
		return nil, fmt.Errorf("seed: github: GITHUB_TOKEN is not set (GitHub's code search API requires authentication; set it to a personal access token)")
	}

	collector := newDomainCollector(n)
	for page := 1; page <= githubMaxPages; page++ {
		items, err := g.searchPage(ctx, token, page)
		if err != nil {
			return nil, err
		}
		var names []string
		for _, item := range items {
			if d := domainFromRepository(item.Repository); d != "" {
				names = append(names, d)
			}
		}
		collector.add(names)
		if collector.full() || len(items) < githubPerPage {
			break
		}
	}

	return collector.domains(), nil
}

// searchPage fetches one page of code-search results.
func (g *GitHubSeeder) searchPage(ctx context.Context, token string, page int) ([]githubSearchItem, error) {
	endpoint, err := url.Parse(g.endpoint())
	if err != nil {
		return nil, fmt.Errorf("seed: github: invalid endpoint: %w", err)
	}
	q := endpoint.Query()
	q.Set("q", g.query())
	q.Set("per_page", strconv.Itoa(githubPerPage))
	q.Set("page", strconv.Itoa(page))
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	var parsed githubSearchResponse
	if err := fetchJSON(g.httpClient(), req, 16<<20, includeStatusErrBody, &parsed); err != nil {
		return nil, fmt.Errorf("seed: github: %w", err)
	}
	return parsed.Items, nil
}

// domainFromRepository derives a candidate domain for repo: its homepage
// host if set, else a guessed GitHub Pages user-site host
// ("<owner>.github.io") when the repo's own name follows that convention —
// the one case a site domain is derivable without an extra API call.
func domainFromRepository(repo githubRepository) string {
	if repo.Homepage != "" {
		if u, err := url.Parse(repo.Homepage); err == nil && u.Host != "" {
			return u.Host
		}
	}

	owner, name, ok := strings.Cut(repo.FullName, "/")
	if ok && strings.EqualFold(name, owner+".github.io") {
		return strings.ToLower(name)
	}
	return ""
}
