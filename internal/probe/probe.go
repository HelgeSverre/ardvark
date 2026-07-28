// Package probe discovers ARD (Agentic Resource Discovery) documents on a
// host via two web discovery mechanisms: the well-known paths (see
// wellKnownPaths), and Agentmap: directives in robots.txt.
//
// The other web mechanisms are handled elsewhere: <link rel="ard"> tags and
// inline <script type="application/ld+json"> markup are both extracted by
// internal/harvest during page_fetch. DNS Service Binding discovery is not
// implemented.
package probe

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/httpx"
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

// wellKnownPaths are the standardized ARD discovery paths, in preference
// order. They are aliases for the same resource, so the first hit wins and
// no further path is fetched.
//
// /.well-known/ard.json is the preferred name; /.well-known/ai-catalog.json
// is the original one and remains supported, since it is where essentially
// every catalog published so far still lives. Dropping it would un-discover
// the existing adopter base.
//
// Cost: a host publishing neither costs two well-known requests instead of
// one. That is the unavoidable price of covering both names — a host that
// publishes the preferred path still costs exactly one.
var wellKnownPaths = []string{
	"/.well-known/ard.json",
	"/.well-known/ai-catalog.json",
}

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
	// RetryError retains the underlying retryable fetch error for the crawler.
	// It is not persisted; ErrorDetail is the durable human-readable form.
	RetryError error
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

// probeWellKnown fetches host's well-known ARD document, trying each alias
// in wellKnownPaths order. Probing is https-only by policy (unlike fetch,
// which parameterizes scheme) since well-known discovery targets are assumed
// to serve over TLS.
func probeWellKnown(ctx context.Context, client *fetch.Client, host string) Result {
	return probeWellKnownBase(ctx, client, "https://"+host)
}

// probeWellKnownBase is probeWellKnown against an explicit scheme+host base,
// returning the first alias that hits.
//
// When nothing hits, an indeterminate result outranks a miss: a 429 or 5xx on
// one alias means we do not actually know whether the host publishes ARD, so
// reporting a miss there would let a transient failure be recorded as a
// durable "not found" and suppress rediscovery.
func probeWellKnownBase(ctx context.Context, client *fetch.Client, base string) Result {
	var firstMiss, firstErr *Result
	for _, path := range wellKnownPaths {
		result := probeWellKnownURL(ctx, client, base+path)
		switch result.Outcome {
		case OutcomeHit:
			return result
		case OutcomeError:
			if firstErr == nil {
				firstErr = &result
			}
		default:
			if firstMiss == nil {
				firstMiss = &result
			}
		}
	}
	if firstErr != nil {
		return *firstErr
	}
	return *firstMiss
}

// probeWellKnownURL fetches one well-known URL, bypassing the robots.txt gate
// (well-known probes are always attempted) while still going through the
// client's rate limiting and politeness caps.
func probeWellKnownURL(ctx context.Context, client *fetch.Client, wellKnownURL string) Result {
	fetched, err := client.GetWellKnown(ctx, wellKnownURL)
	if err != nil {
		return classifyFetchErr(MethodWellKnown, wellKnownURL, err)
	}

	// A 200 alone isn't an ARD document: parked domains and SPA catch-alls
	// serve a generic HTML page at every path. Require a JSON-ish response so
	// those don't pollute the catalog table as "invalid" false positives.
	if !httpx.LooksLikeJSON(fetched.ContentType, fetched.Body) {
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

// probeRobotsAgentmap fetches host's robots.txt (via the client's cached
// robots fetch) and scans its raw lines for "Agentmap:" directives, each of
// which names an ARD catalog document URL. The directive's value is
// resolved relative to the robots.txt URL if it is not already absolute.
func probeRobotsAgentmap(ctx context.Context, client *fetch.Client, host string) Result {
	// https-only by policy, same as probeWellKnown.
	robotsURL := "https://" + host + "/robots.txt"

	raw, err := client.RawRobots(ctx, host)
	if err != nil {
		return classifyFetchErr(MethodRobotsAgentmap, robotsURL, err)
	}

	if raw == "" {
		return Result{
			Method:      MethodRobotsAgentmap,
			URL:         robotsURL,
			Outcome:     OutcomeMiss,
			ErrorDetail: "no robots.txt",
		}
	}

	urls := parseAgentmapDirectives(raw, robotsURL)
	if len(urls) == 0 {
		return Result{
			Method:      MethodRobotsAgentmap,
			URL:         robotsURL,
			HTTPStatus:  http.StatusOK,
			Outcome:     OutcomeMiss,
			ErrorDetail: "no Agentmap directive",
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
		if fe.Status >= 400 && fe.Status < 500 && fe.Status != http.StatusTooManyRequests {
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
			RetryError:  retryError(err),
		}
	}
	return Result{
		Method:      method,
		URL:         rawURL,
		Outcome:     OutcomeError,
		ErrorDetail: err.Error(),
		RetryError:  retryError(err),
	}
}

func retryError(err error) error {
	if fetch.Transient(err) {
		return err
	}
	return nil
}

// resolveAgainst resolves ref against baseURL, returning the absolute
// http/https URL string, or ok=false if ref is empty, malformed, or not
// http/https after resolution. Unlike harvest's link resolution, an
// Agentmap directive's fragment (if any) is kept as-is rather than
// stripped, since it's a pointer to a single named document rather than a
// link being deduped among many.
func resolveAgainst(baseURL, ref string) (string, bool) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	resolved, ok := httpx.ResolveHTTPURL(base, ref, false)
	if !ok {
		return "", false
	}
	return resolved.String(), true
}
