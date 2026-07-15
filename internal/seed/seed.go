// Package seed bootstraps the crawl frontier from external domain sources
// when there is no seed list to start from: Certificate Transparency logs,
// crt.sh, Tranco, GitHub code search, the MCP server registry, curated
// awesome-lists, and Common Crawl web-graph domain ranks. Every
// source implements the Seeder interface and shares the same tail:
// sanitize domains (strip a leading "*.", lowercase, drop IPs and invalid
// hostnames, dedupe), leaving the caller (cmd/ardvark's seed subcommands)
// to upsert domains rows with the appropriate discovery_source and enqueue
// host_probe frontier items.
// See docs/superpowers/specs/2026-07-15-ardvark-crawler-design.md's
// "Seeding" section for the design this package implements.
package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/helgesverre/ardvark/internal/httpx"
)

// defaultHTTPTimeout is the client timeout used by every seeder that fetches
// small/paginated JSON responses (CT log list, crt.sh, GitHub, MCP
// registry).
const defaultHTTPTimeout = 30 * time.Second

// trancoHTTPTimeout is longer than defaultHTTPTimeout because the Tranco
// list is a tens-of-megabytes single download, not a paginated API call.
const trancoHTTPTimeout = 60 * time.Second

// newHTTPClient returns client if non-nil, else a client with the given
// timeout — the "default HTTP client" pattern every seeder's httpClient()
// method follows.
func newHTTPClient(client *http.Client, timeout time.Duration) *http.Client {
	if client != nil {
		return client
	}
	return &http.Client{Timeout: timeout}
}

// statusErrBody selects whether fetchJSON embeds the response body in a
// non-200 error. API sources whose error bodies are small JSON diagnostics
// (CT logs, GitHub, MCP registry) embed them because they carry the
// actionable message; sources whose error pages are whole HTML documents
// (crt.sh when rate-limited, the CT log-list CDN) stay body-free so a 503
// doesn't spill megabytes of markup into CLI error/log output.
type statusErrBody bool

const (
	includeStatusErrBody statusErrBody = true
	omitStatusErrBody    statusErrBody = false
)

// fetchJSON runs req, reads up to limitBytes of the response body, and
// decodes it as JSON into out. Callers build the request (method, URL,
// headers); this handles the repeated Do -> read -> status check -> decode
// tail shared by every seed source, including the guard against servers
// that serve an HTML error/rate-limit page where JSON was expected. Errors
// are unprefixed; wrap with the caller's own "seed: <source>: " prefix.
func fetchJSON(client *http.Client, req *http.Request, limitBytes int64, errBody statusErrBody, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s: %w", req.URL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, limitBytes))
	if err != nil {
		return fmt.Errorf("reading response from %s: %w", req.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		if errBody == includeStatusErrBody {
			return fmt.Errorf("%s returned status %d: %s", req.URL, resp.StatusCode, string(body))
		}
		return fmt.Errorf("%s returned status %d", req.URL, resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	if !httpx.LooksLikeJSON(contentType, body) {
		return fmt.Errorf("%s returned %s, not JSON", req.URL, ctOrUnknown(contentType))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response from %s: %w", req.URL, err)
	}
	return nil
}

// fetchBody runs req and returns up to limitBytes of the response body with
// the same politeness as fetchJSON (context via the request, limit reader,
// status check) but no JSON guard — for sources whose payloads are markdown
// or plain text (curated awesome-lists). Errors are unprefixed; wrap with the
// caller's own "seed: <source>: " prefix.
func fetchBody(client *http.Client, req *http.Request, limitBytes int64) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", req.URL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, limitBytes))
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", req.URL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", req.URL, resp.StatusCode)
	}
	return body, nil
}

// ctOrUnknown renders a Content-Type header for an error message, stripping
// parameters (e.g. "; charset=utf-8") and substituting a placeholder when
// the header is absent.
func ctOrUnknown(contentType string) string {
	if contentType == "" {
		return "an unknown content type"
	}
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return contentType
}

// domainCollector incrementally sanitizes and dedupes raw hostname-like
// values (see Sanitize) up to a target count, without re-sanitizing
// already-accepted names on every call — the loops in CrtshSeeder,
// GitHubSeeder, and MCPRegistrySeeder page through a source until they have
// n domains, and re-running Sanitize over the whole accumulated slice each
// page is O(n^2) in the number of pages.
type domainCollector struct {
	seen  map[string]struct{}
	names []string
	limit int
}

func newDomainCollector(limit int) *domainCollector {
	return &domainCollector{seen: make(map[string]struct{}), limit: limit}
}

// add sanitizes and dedupes each of raw, appending previously-unseen names
// until limit is reached. It's a no-op once full.
func (c *domainCollector) add(raw []string) {
	for _, r := range raw {
		if c.full() {
			return
		}
		name, ok := sanitizeName(r)
		if !ok {
			continue
		}
		if _, dup := c.seen[name]; dup {
			continue
		}
		c.seen[name] = struct{}{}
		c.names = append(c.names, name)
	}
}

// full reports whether limit names have been collected.
func (c *domainCollector) full() bool {
	return len(c.names) >= c.limit
}

// domains returns the collected names (never more than limit).
func (c *domainCollector) domains() []string {
	return c.names
}

// Seeder is implemented by every pluggable seed source (CT logs, crt.sh,
// Tranco, …).
type Seeder interface {
	// Domains streams sanitized hostnames until n collected or the source
	// is exhausted; ctx cancellation stops it.
	Domains(ctx context.Context, n int) ([]string, error)
	// Source is the discovery_source tag recorded on the domains row for
	// every hostname this seeder yields, e.g. "ct_log", "crtsh", "tranco".
	Source() string
}

// Sanitize normalizes a list of raw hostname-like values (SAN/CN entries,
// list-file rows, …) into a deduped list of plausible domain names:
//   - strips a leading "*." wildcard label (the apex is probed instead)
//   - lowercases
//   - drops IP addresses
//   - drops values with no dot or containing characters invalid in a
//     hostname
//   - dedupes while preserving first-seen order
func Sanitize(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))

	for _, raw := range names {
		name, ok := sanitizeName(raw)
		if !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	return out
}

// sanitizeName applies Sanitize's per-name normalization/validation rules to
// a single raw value, reporting ok=false if it should be dropped.
func sanitizeName(raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", false
	}
	name = strings.TrimPrefix(name, "*.")
	name = strings.ToLower(name)
	name = strings.TrimSuffix(name, ".")

	if !strings.Contains(name, ".") {
		return "", false
	}
	if net.ParseIP(name) != nil {
		return "", false
	}
	if !isValidHostname(name) {
		return "", false
	}
	return name, true
}

// isValidHostname reports whether s consists only of characters legal in a
// DNS hostname: letters, digits, hyphens and dots, with labels that don't
// start or end with a hyphen.
func isValidHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= '0' && r <= '9':
			case r == '-':
			default:
				return false
			}
		}
	}
	return true
}
