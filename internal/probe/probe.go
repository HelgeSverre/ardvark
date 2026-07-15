// Package probe discovers ARD (Agentic Resource Discovery) documents on a
// host via the two web discovery mechanisms in scope for v1: the
// well-known path /.well-known/ai-catalog.json, and Agentmap: directives in
// robots.txt. (DNS Service Binding discovery and <link rel="ai-catalog">
// scanning of harvested pages are handled elsewhere — the latter by
// internal/harvest during page_fetch.)
package probe

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/helgesverre/ardvark/internal/fetch"
)

// Probe methods, matching the values expected by store.Probe.Method.
const (
	MethodWellKnown      = "well_known"
	MethodRobotsAgentmap = "robots_agentmap"
)

// Probe outcomes, matching the values expected by store.Probe.Outcome.
const (
	OutcomeHit   = "hit"
	OutcomeMiss  = "miss"
	OutcomeError = "error"
)

// wellKnownPath is the standardized ARD discovery path.
const wellKnownPath = "/.well-known/ai-catalog.json"

// agentmapPrefix is the (case-insensitive) robots.txt directive key that
// points at ARD catalog document(s). This is a non-standard directive, so
// it is scanned for by hand rather than via the robots.txt parser.
const agentmapPrefix = "agentmap:"

// Result is the outcome of one probe attempt, ready for persistence as a
// store.Probe row.
type Result struct {
	// Method is one of MethodWellKnown or MethodRobotsAgentmap.
	Method string
	// URL is the URL that was fetched for this probe attempt (the
	// well-known URL, or the robots.txt URL for the Agentmap scan).
	URL string
	// HTTPStatus is the HTTP status code observed, or 0 if no response was
	// received (e.g. a network-level error).
	HTTPStatus int
	// ContentType is the response's Content-Type header, if any.
	ContentType string
	// Outcome is one of OutcomeHit, OutcomeMiss, or OutcomeError.
	Outcome string
	// ErrorDetail carries a human-readable error description when Outcome
	// is OutcomeError.
	ErrorDetail string
	// CatalogURLs holds the ARD catalog document URL(s) discovered by this
	// probe. Empty unless Outcome is OutcomeHit.
	CatalogURLs []string
}

// Probe attempts both discovery mechanisms for host (a bare hostname, e.g.
// "example.com") and returns one Result per method attempted, in a stable
// order (well-known first, then robots Agentmap).
func Probe(ctx context.Context, client *fetch.Client, host string) []Result {
	return []Result{
		probeWellKnown(ctx, client, host),
		probeRobotsAgentmap(ctx, client, host),
	}
}

// probeWellKnown fetches https://<host>/.well-known/ai-catalog.json,
// bypassing the robots.txt gate (well-known probes are always attempted)
// while still going through the client's rate limiting and politeness
// caps.
func probeWellKnown(ctx context.Context, client *fetch.Client, host string) Result {
	wellKnownURL := "https://" + host + wellKnownPath

	fetched, err := client.GetWellKnown(ctx, wellKnownURL)
	if err != nil {
		return classifyFetchErr(MethodWellKnown, wellKnownURL, err)
	}

	// A 200 alone isn't an ARD document: parked domains and SPA catch-alls
	// serve a generic HTML page at every path. Require a JSON-ish response so
	// those don't pollute the catalog table as "invalid" false positives.
	if !looksLikeJSON(fetched.ContentType, fetched.Body) {
		return Result{
			Method:      MethodWellKnown,
			URL:         fetched.URL,
			HTTPStatus:  fetched.Status,
			ContentType: fetched.ContentType,
			Outcome:     OutcomeMiss,
			ErrorDetail: "non-JSON response",
		}
	}

	return Result{
		Method:      MethodWellKnown,
		URL:         fetched.URL,
		HTTPStatus:  fetched.Status,
		ContentType: fetched.ContentType,
		Outcome:     OutcomeHit,
		CatalogURLs: []string{fetched.URL},
	}
}

// looksLikeJSON reports whether a well-known response is plausibly a JSON
// document, by content type or by the body's first non-space byte. It stays
// lenient on content type (servers often mislabel .json) but rejects the HTML
// that parked domains and SPA catch-alls return.
func looksLikeJSON(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "json") {
		return true
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

// probeRobotsAgentmap fetches host's robots.txt (via the client's cached
// robots fetch) and scans its raw lines for "Agentmap:" directives, each of
// which names an ARD catalog document URL. The directive's value is
// resolved relative to the robots.txt URL if it is not already absolute.
func probeRobotsAgentmap(ctx context.Context, client *fetch.Client, host string) Result {
	robotsURL := "https://" + host + "/robots.txt"

	raw, err := client.RawRobots(ctx, host)
	if err != nil {
		return classifyFetchErr(MethodRobotsAgentmap, robotsURL, err)
	}

	if raw == "" {
		return Result{
			Method:  MethodRobotsAgentmap,
			URL:     robotsURL,
			Outcome: OutcomeMiss,
		}
	}

	urls := parseAgentmapDirectives(raw, robotsURL)
	if len(urls) == 0 {
		return Result{
			Method:     MethodRobotsAgentmap,
			URL:        robotsURL,
			HTTPStatus: http.StatusOK,
			Outcome:    OutcomeMiss,
		}
	}

	return Result{
		Method:      MethodRobotsAgentmap,
		URL:         robotsURL,
		HTTPStatus:  http.StatusOK,
		Outcome:     OutcomeHit,
		CatalogURLs: urls,
	}
}

// parseAgentmapDirectives scans raw robots.txt content line by line for a
// case-insensitive "Agentmap:" key and collects its values as absolute
// URLs, resolved against baseURL when relative. Malformed values are
// skipped.
func parseAgentmapDirectives(raw, baseURL string) []string {
	var urls []string
	seen := make(map[string]struct{})

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(line), agentmapPrefix) {
			continue
		}
		value := strings.TrimSpace(line[len(agentmapPrefix):])
		if value == "" {
			continue
		}

		resolved, ok := resolveAgainst(baseURL, value)
		if !ok {
			continue
		}
		if _, dup := seen[resolved]; dup {
			continue
		}
		seen[resolved] = struct{}{}
		urls = append(urls, resolved)
	}

	return urls
}

// classifyFetchErr turns a fetch package error into an Error-outcome
// Result. A "not found" style permanent 4xx failure is treated as a miss
// rather than an error, since that is the expected outcome when a host
// simply doesn't publish the probed document.
func classifyFetchErr(method, rawURL string, err error) Result {
	var fe *fetch.Error
	if errors.As(err, &fe) {
		if fe.Status >= 400 && fe.Status < 500 {
			return Result{
				Method:     method,
				URL:        rawURL,
				HTTPStatus: fe.Status,
				Outcome:    OutcomeMiss,
			}
		}
		return Result{
			Method:      method,
			URL:         rawURL,
			HTTPStatus:  fe.Status,
			Outcome:     OutcomeError,
			ErrorDetail: fe.Error(),
		}
	}
	return Result{
		Method:      method,
		URL:         rawURL,
		Outcome:     OutcomeError,
		ErrorDetail: err.Error(),
	}
}

// resolveAgainst resolves ref against baseURL, returning the absolute
// http/https URL string, or ok=false if ref is empty, malformed, or not
// http/https after resolution.
func resolveAgainst(baseURL, ref string) (string, bool) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	refURL, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	resolved := base.ResolveReference(refURL)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", false
	}
	return resolved.String(), true
}
