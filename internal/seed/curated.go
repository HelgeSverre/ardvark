package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/helgesverre/ardvark/internal/store"
)

// DefaultCuratedListURLs are the curated awesome-list documents scanned by
// default: the largest community-maintained MCP server lists, whose entries
// link to real product domains alongside repository and badge infrastructure
// (which CuratedInfraDomains filters out).
var DefaultCuratedListURLs = []string{
	"https://raw.githubusercontent.com/punkpeye/awesome-mcp-servers/main/README.md",
	"https://raw.githubusercontent.com/wong2/awesome-mcp-servers/main/README.md",
	"https://raw.githubusercontent.com/appcypher/awesome-mcp-servers/main/README.md",
}

// CuratedInfraDomains are registrable domains dropped from curated-list
// extraction because they are hosting/badge/social infrastructure rather
// than candidate ARD publishers — an awesome-list README links to thousands
// of github.com repos and shields.io badges per real product domain.
// Matching is on the URL host's registrable domain (eTLD+1), so subdomains
// like gist.github.com match too, while *.github.io project sites (their own
// registrable domains under the github.io public suffix) are kept. Tweak
// this set if a new list source drags in a new flavor of infrastructure.
var CuratedInfraDomains = map[string]struct{}{
	"github.com":         {},
	"gitlab.com":         {},
	"bitbucket.org":      {},
	"npmjs.com":          {},
	"pypi.org":           {},
	"shields.io":         {},
	"star-history.com":   {},
	"youtube.com":        {},
	"youtu.be":           {},
	"reddit.com":         {},
	"wikipedia.org":      {},
	"twitter.com":        {},
	"x.com":              {},
	"discord.com":        {},
	"discord.gg":         {},
	"medium.com":         {},
	"dev.to":             {},
	"linkedin.com":       {},
	"buymeacoffee.com":   {},
	"patreon.com":        {},
	"opencollective.com": {},
	"glama.ai":           {},
}

// curatedListMaxBytes caps how much of one list document is read (the
// largest awesome-mcp-servers README is ~1 MB; 10 MB bounds a misconfigured
// URL without truncating any plausible list).
const curatedListMaxBytes = 10 << 20

// curatedURLPattern matches absolute http(s) URLs in raw text. It excludes
// characters that commonly terminate a URL in markdown/HTML (whitespace,
// quotes, brackets, closing paren); only the host is extracted downstream,
// so imprecise trailing-path truncation is harmless.
var curatedURLPattern = regexp.MustCompile(`https?://[^\s<>"')\]]+`)

// CuratedSeeder fetches curated awesome-list documents (markdown/text over
// HTTP), extracts every absolute http(s) URL, reduces them to unique
// candidate domains, and drops infrastructure hosts (CuratedInfraDomains).
// It implements Seeder with Source() "curated_list". Scanning is
// format-agnostic — plain URL extraction over the raw text — so any
// link-bearing text document works as a source.
type CuratedSeeder struct {
	// ListURLs are the documents to scan. Defaults to
	// DefaultCuratedListURLs if empty.
	ListURLs []string

	// HTTPClient is used for all requests. Defaults to a client with a 30s
	// timeout if nil.
	HTTPClient *http.Client
}

// NewCuratedSeeder returns a CuratedSeeder scanning the given list URLs
// (empty uses DefaultCuratedListURLs).
func NewCuratedSeeder(listURLs []string) *CuratedSeeder {
	return &CuratedSeeder{ListURLs: listURLs}
}

// Source implements Seeder.
func (c *CuratedSeeder) Source() string { return store.DiscoverySourceCurated }

func (c *CuratedSeeder) listURLs() []string {
	if len(c.ListURLs) > 0 {
		return c.ListURLs
	}
	return DefaultCuratedListURLs
}

func (c *CuratedSeeder) httpClient() *http.Client {
	return newHTTPClient(c.HTTPClient, defaultHTTPTimeout)
}

// Domains implements Seeder: it fetches each list document in order,
// extracts candidate hosts from every absolute URL, filters infrastructure
// domains, and returns up to n sanitized unique domains.
func (c *CuratedSeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: curated: n must be positive, got %d", n)
	}

	collector := newDomainCollector(n)
	for _, listURL := range c.listURLs() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
		if err != nil {
			return nil, fmt.Errorf("seed: curated: %w", err)
		}
		body, err := fetchBody(c.httpClient(), req, curatedListMaxBytes)
		if err != nil {
			return nil, fmt.Errorf("seed: curated: %w", err)
		}
		collector.add(hostsFromText(string(body)))
		if collector.full() {
			break
		}
	}

	return collector.domains(), nil
}

// hostsFromText extracts the host of every absolute http(s) URL in text
// (port and userinfo stripped), dropping infrastructure hosts. Order of
// first appearance is preserved; dedupe happens in the caller's collector.
func hostsFromText(text string) []string {
	var hosts []string
	for _, match := range curatedURLPattern.FindAllString(text, -1) {
		u, err := url.Parse(match)
		if err != nil {
			continue
		}
		host := u.Hostname() // strips port, userinfo, and IPv6 brackets
		if host == "" || isInfraHost(host) {
			continue
		}
		hosts = append(hosts, host)
	}
	return hosts
}

// isInfraHost reports whether host's registrable domain (eTLD+1) is in
// CuratedInfraDomains. Hosts whose registrable domain can't be derived
// (bare TLDs, private suffixes) fall back to an exact-map lookup.
func isInfraHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	reg, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		reg = host
	}
	_, blocked := CuratedInfraDomains[reg]
	return blocked
}
