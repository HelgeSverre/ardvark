// Package harvest parses HTML documents to extract outbound links, the set
// of hosts referenced by those links, ARD discovery hints found in
// <link rel="ard"> / <link rel="ai-catalog"> tags, and the raw contents of
// any <script type="application/ld+json"> blocks (which may carry an ARD
// catalog inline; interpreting them is the caller's job).
package harvest

import (
	"io"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"github.com/helgesverre/ardvark/internal/httpx"
)

// Result is the outcome of extracting links from an HTML document.
type Result struct {
	// Links holds unique, absolute http/https URLs found in <a href> and
	// <area href> attributes, with any URL fragment stripped.
	Links []string
	// Hosts holds the unique set of hostnames (host:port form as it
	// appears in the resolved URL's Host component) referenced by Links.
	Hosts []string
	// AICatalogHints holds absolute URLs from <link rel="ai-catalog"
	// href="..."> tags, resolved the same way as anchor links.
	AICatalogHints []string
	// JSONLD holds the raw contents of the page's
	// <script type="application/ld+json"> blocks, in document order and
	// bounded by maxJSONLDBlocks/maxJSONLDBlockBytes. Contents are NOT
	// parsed or validated here: most JSON-LD on the web is Schema.org
	// markup that has nothing to do with ARD, so deciding what a block
	// means is the caller's job.
	JSONLD [][]byte
}

// Bounds on inline JSON-LD extraction. A page's JSON-LD is fetched as part
// of the page body, which the crawler already size-caps, but these keep one
// pathological page from turning into unbounded downstream parsing and
// fan-out work.
const (
	maxJSONLDBlocks     = 32
	maxJSONLDBlockBytes = 1 << 20 // 1 MiB per block
)

// ExtractLinks parses body as HTML relative to baseURL and extracts anchor
// links, the set of unique hosts among them, and ai-catalog link hints.
//
// baseURL is used to resolve relative and protocol-relative URLs, and may
// itself be overridden by an in-document <base href="..."> tag. Only
// http/https URLs are returned; malformed or unresolvable hrefs are
// skipped. URL fragments are stripped before dedup.
func ExtractLinks(baseURL string, body io.Reader) (Result, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return Result{}, err
	}

	doc, err := html.Parse(body)
	if err != nil {
		return Result{}, err
	}

	// First pass: find a <base href> tag, which (if present and valid)
	// overrides baseURL for resolving all relative references.
	effectiveBase := base
	if bref, ok := findBaseHref(doc); ok {
		if resolved, err := base.Parse(bref); err == nil {
			effectiveBase = resolved
		}
	}

	linkSet := map[string]string{} // normalized key -> original resolved URL
	hostSet := map[string]struct{}{}
	catalogSet := map[string]string{}
	var jsonLD [][]byte

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script":
				if typ, ok := attr(n, "type"); ok && isJSONLDType(typ) {
					if block, ok := scriptText(n); ok && len(jsonLD) < maxJSONLDBlocks {
						jsonLD = append(jsonLD, block)
					}
				}
			case "a", "area":
				if href, ok := attr(n, "href"); ok {
					if resolved, ok := resolveHTTPURL(effectiveBase, href); ok {
						key := resolved.String()
						if _, exists := linkSet[key]; !exists {
							linkSet[key] = key
							hostSet[resolved.Host] = struct{}{}
						}
					}
				}
			case "link":
				rel, _ := attr(n, "rel")
				if isAICatalogRel(rel) {
					if href, ok := attr(n, "href"); ok {
						if resolved, ok := resolveHTTPURL(effectiveBase, href); ok {
							catalogSet[resolved.String()] = resolved.String()
						}
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return Result{
		Links:          sortedValues(linkSet),
		Hosts:          sortedKeys(hostSet),
		AICatalogHints: sortedValues(catalogSet),
		JSONLD:         jsonLD,
	}, nil
}

// isJSONLDType reports whether a <script> type attribute names JSON-LD.
// Comparison ignores case and any media-type parameters (e.g. charset).
func isJSONLDType(typ string) bool {
	base, _, _ := strings.Cut(typ, ";")
	return strings.EqualFold(strings.TrimSpace(base), "application/ld+json")
}

// scriptText returns a script element's text content, or ok=false when it is
// empty, or larger than maxJSONLDBlockBytes. Oversized blocks are dropped
// rather than truncated: a partial JSON document would only fail to parse
// later, with a confusing error.
func scriptText(n *html.Node) ([]byte, bool) {
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.TextNode {
			continue
		}
		if sb.Len()+len(c.Data) > maxJSONLDBlockBytes {
			return nil, false
		}
		sb.WriteString(c.Data)
	}
	text := strings.TrimSpace(sb.String())
	if text == "" {
		return nil, false
	}
	return []byte(text), true
}

// ardRelTokens are the link relation tokens that name an ARD document.
// "ard" is preferred; "ai-catalog" is the original token and remains
// supported, since that is what published pages currently use.
var ardRelTokens = []string{"ard", "ai-catalog"}

// isAICatalogRel reports whether rel (a possibly space-separated list of
// link relation tokens, per HTML's rel attribute) contains a token naming an
// ARD document. Matching is per whole token and case-insensitive, so
// rel="guard" and rel="ard-extra" do not match.
func isAICatalogRel(rel string) bool {
	for _, tok := range strings.Fields(rel) {
		for _, want := range ardRelTokens {
			if strings.EqualFold(tok, want) {
				return true
			}
		}
	}
	return false
}

// resolveHTTPURL resolves ref against base, stripping any fragment, and
// returns ok=false if the href is empty, malformed, or not http/https.
func resolveHTTPURL(base *url.URL, ref string) (*url.URL, bool) {
	return httpx.ResolveHTTPURL(base, ref, true)
}

// findBaseHref returns the href of the first <base> element in doc, if any.
func findBaseHref(n *html.Node) (string, bool) {
	if n.Type == html.ElementNode && n.Data == "base" {
		if href, ok := attr(n, "href"); ok && strings.TrimSpace(href) != "" {
			return href, true
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if href, ok := findBaseHref(c); ok {
			return href, ok
		}
	}
	return "", false
}

// attr returns the value of the named attribute on n, if present.
func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
