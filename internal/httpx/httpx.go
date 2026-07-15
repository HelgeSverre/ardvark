// Package httpx holds small HTTP response heuristics shared by the probe,
// seed, and verification layers, so the "is this JSON?" rule lives in one
// place instead of drifting across per-package copies.
package httpx

import (
	"bytes"
	"net/url"
	"strings"
)

// IsJSONContentType reports whether contentType declares a JSON payload.
// Matching is lenient: any media type mentioning "json" counts, including
// suffixed types like application/ai-catalog+json.
func IsJSONContentType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "json")
}

// LooksLikeJSON reports whether a response is plausibly a JSON document, by
// content type or by the body's first non-space byte. It stays lenient on
// content type (servers often mislabel .json) but rejects the HTML that
// parked domains, SPA catch-alls, and error pages return. Either argument
// may be empty; an empty content type falls through to body sniffing.
func LooksLikeJSON(contentType string, body []byte) bool {
	if contentType != "" && IsJSONContentType(contentType) {
		return true
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

// ResolveHTTPURL resolves ref against base and returns the absolute URL, or
// ok=false if ref is empty, malformed, or the resolved URL isn't http/https
// with a non-empty host. If stripFragment is true, any fragment on the
// resolved URL is removed before it's returned (callers that dedup links by
// their string form want this; callers that just need an absolute pointer
// URL generally don't care either way).
func ResolveHTTPURL(base *url.URL, ref string, stripFragment bool) (*url.URL, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, false
	}

	refURL, err := url.Parse(ref)
	if err != nil {
		return nil, false
	}

	resolved := base.ResolveReference(refURL)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return nil, false
	}
	if resolved.Host == "" {
		return nil, false
	}

	if stripFragment {
		resolved.Fragment = ""
		resolved.RawFragment = ""
	}
	return resolved, true
}
