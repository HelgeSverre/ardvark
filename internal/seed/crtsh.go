package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/helgesverre/ardvark/internal/store"
)

// crtshDefaultEndpoint is crt.sh's public JSON search endpoint.
const crtshDefaultEndpoint = "https://crt.sh"

// DefaultCrtshMatches is a curated keyword set used when the caller supplies
// no --match: crt.sh cannot serve a bare "q=%" wildcard (it either rejects
// it or times out scanning the whole corpus), and even if it could, an
// unfiltered stream is low-signal for an ARD-focused crawl. Each keyword is
// queried in turn (crt.sh only accepts one identity pattern per request)
// until enough domains are collected.
var DefaultCrtshMatches = []string{"agent", "mcp", "ai-catalog"}

// crtshMaxRecordsPerQuery bounds how many rows crt.sh's response is allowed
// to contribute per keyword query, keeping a single broad keyword (e.g.
// "ai") from dominating memory/results even on a very active keyword.
const crtshMaxRecordsPerQuery = 100000

// CrtshSeeder queries crt.sh's JSON API for recent certificates matching a
// keyword and extracts candidate domain names from their common names and
// SAN entries. It implements Seeder with Source() "crtsh". crt.sh has
// already parsed and deduped the certificate transparency data, making it a
// higher-signal, lower-effort seed source than walking raw CT logs.
type CrtshSeeder struct {
	// Endpoint is the crt.sh base URL. Defaults to crtshDefaultEndpoint if
	// empty.
	Endpoint string

	// Match narrows results to certificates whose identity mentions this
	// keyword (e.g. "agent", "mcp"), via crt.sh's "%keyword%" identity
	// search. Empty sends a bare "q=%" wildcard, which crt.sh cannot
	// reliably serve (it either rejects it or times out scanning the whole
	// corpus) — leave Match empty only for direct programmatic/test use
	// against a stub server; production callers should set Matches (the CLI
	// falls back to DefaultCrtshMatches when the user supplies no keyword).
	Match string

	// Matches, if non-empty, overrides Match: each keyword is queried in
	// turn (crt.sh serves one identity pattern per request) and results are
	// merged until n domains are collected. This is how the curated
	// DefaultCrtshMatches keyword set is applied.
	Matches []string

	// HTTPClient is used for all requests. Defaults to a client with a 30s
	// timeout if nil.
	HTTPClient *http.Client
}

// Source implements Seeder.
func (c *CrtshSeeder) Source() string { return store.DiscoverySourceCrtsh }

func (c *CrtshSeeder) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return crtshDefaultEndpoint
}

func (c *CrtshSeeder) httpClient() *http.Client {
	return newHTTPClient(c.HTTPClient, defaultHTTPTimeout)
}

// crtshRecord is one row of crt.sh's JSON search response. Only the fields
// useful for domain extraction are decoded.
type crtshRecord struct {
	CommonName string `json:"common_name"`
	NameValue  string `json:"name_value"`
}

// Domains implements Seeder: it queries crt.sh's JSON search API for
// certificates whose identity mentions one of the configured keywords
// (Matches, falling back to a single Match, falling back to
// DefaultCrtshMatches), extracts SAN/common-name domains from the
// response(s), sanitizes and dedupes them, and returns up to n. Keywords
// are queried in order and stop early once n domains are collected.
func (c *CrtshSeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: crtsh: n must be positive, got %d", n)
	}

	collector := newDomainCollector(n)
	for _, match := range c.matches() {
		raw, err := c.queryOne(ctx, match)
		if err != nil {
			return nil, err
		}
		collector.add(raw)
		if collector.full() {
			break
		}
	}

	return collector.domains(), nil
}

// matches resolves the effective keyword list for a Domains call:
// c.Matches if set, else a single-element slice from c.Match (which may be
// the empty string, matching everything — only sensible for direct
// programmatic/test use, since the CLI always supplies a keyword).
func (c *CrtshSeeder) matches() []string {
	if len(c.Matches) > 0 {
		return c.Matches
	}
	return []string{c.Match}
}

// queryOne runs a single crt.sh identity search for match (may be empty for
// a bare wildcard) and returns the raw, unsanitized SAN/common-name values
// found in the response.
func (c *CrtshSeeder) queryOne(ctx context.Context, match string) ([]string, error) {
	query := "%"
	if match != "" {
		query = "%" + match + "%"
	}

	endpoint, err := url.Parse(strings.TrimSuffix(c.endpoint(), "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("seed: crtsh: invalid endpoint: %w", err)
	}
	q := endpoint.Query()
	q.Set("q", query)
	q.Set("output", "json")
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}

	var records []crtshRecord
	// crt.sh is often rate-limited or overloaded, in which case it serves
	// an HTML page (status error or otherwise) instead of JSON; fetchJSON's
	// content-type/body guard catches that case before it would otherwise
	// spill markup into the decode error.
	if err := fetchJSON(c.httpClient(), req, 64<<20, omitStatusErrBody, &records); err != nil {
		return nil, fmt.Errorf("seed: crtsh: %w (crt.sh is often rate-limited; retry shortly)", err)
	}
	// Bound how many rows a single broad keyword can contribute, regardless
	// of how many crt.sh actually returns.
	if len(records) > crtshMaxRecordsPerQuery {
		records = records[:crtshMaxRecordsPerQuery]
	}

	var names []string
	for _, rec := range records {
		if rec.CommonName != "" {
			names = append(names, rec.CommonName)
		}
		for _, line := range strings.Split(rec.NameValue, "\n") {
			if line != "" {
				names = append(names, line)
			}
		}
	}
	return names, nil
}
