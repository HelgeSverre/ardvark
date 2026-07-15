package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	// search. Empty matches everything crt.sh returns for a bare wildcard
	// query; prefer Matches (or leave both empty to fall back to
	// DefaultCrtshMatches) over relying on the bare-wildcard behavior.
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

// NewCrtshSeeder returns a CrtshSeeder that narrows results to certificates
// whose identity mentions match (may be empty).
func NewCrtshSeeder(match string) *CrtshSeeder {
	return &CrtshSeeder{Match: match}
}

// Source implements Seeder.
func (c *CrtshSeeder) Source() string { return "crtsh" }

func (c *CrtshSeeder) endpoint() string {
	if c.Endpoint != "" {
		return c.Endpoint
	}
	return crtshDefaultEndpoint
}

func (c *CrtshSeeder) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
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

	var names []string
	for _, match := range c.matches() {
		raw, err := c.queryOne(ctx, match)
		if err != nil {
			return nil, err
		}
		names = append(names, raw...)

		if len(Sanitize(names)) >= n {
			break
		}
	}

	sanitized := Sanitize(names)
	if len(sanitized) > n {
		sanitized = sanitized[:n]
	}
	return sanitized, nil
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
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("seed: crtsh: request to %s: %w", endpoint.String(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("seed: crtsh: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seed: crtsh: %s returned status %d (crt.sh is often rate-limited; retry shortly)", endpoint.String(), resp.StatusCode)
	}
	// crt.sh serves an HTML page instead of JSON when overloaded; decoding
	// that would spill markup into the error, so reject it up front.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") && !looksLikeJSON(body) {
		return nil, fmt.Errorf("seed: crtsh: %s returned %s, not JSON (crt.sh is often rate-limited; retry shortly)", endpoint.String(), ctOrUnknown(ct))
	}

	var records []crtshRecord
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("seed: crtsh: decoding response: %w", err)
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

// looksLikeJSON reports whether body begins with a JSON array or object, after
// leading whitespace — a cheap guard against HTML error pages.
func looksLikeJSON(body []byte) bool {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	return strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")
}

func ctOrUnknown(contentType string) string {
	if contentType == "" {
		return "an unknown content type"
	}
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return contentType
}
