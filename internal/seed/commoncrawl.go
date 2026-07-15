package seed

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/helgesverre/ardvark/internal/store"
)

// commonCrawlDefaultGraphInfoURL lists Common Crawl's web-graph releases as
// a JSON array ordered newest-first; each element's "id" (e.g.
// "cc-main-2026-apr-may-jun") names a release.
const commonCrawlDefaultGraphInfoURL = "https://index.commoncrawl.org/graphinfo.json"

// commonCrawlDefaultDataURL is the base URL the per-release domain-ranks
// file is served under, at
// /projects/hyperlinkgraph/<id>/domain/<id>-domain-ranks.txt.gz.
const commonCrawlDefaultDataURL = "https://data.commoncrawl.org"

// commonCrawlHTTPTimeout is far longer than defaultHTTPTimeout because the
// ranks file is a ~1 GB gzip stream read incrementally until enough domains
// are collected — a 30s client timeout would kill any large-N (or
// large-offset) run mid-stream. Context cancellation still applies.
const commonCrawlHTTPTimeout = 5 * time.Minute

// commonCrawlMaxLineBytes bounds the ranks-file line scanner. Real lines are
// a few dozen bytes; 64 KB tolerates pathological hosts without letting a
// corrupt stream buffer unbounded memory.
const commonCrawlMaxLineBytes = 64 << 10

// CommonCrawlSeeder streams the domain-level ranks file of a Common Crawl
// web-graph release and yields the top N (optionally offset) registrable
// domains, best harmonic-centrality rank first. It implements Seeder with
// Source() "commoncrawl". This is the "established web at Common Crawl
// scale" source: ~121M ranked domains versus Tranco's 1M, so --offset lets
// runs sample far deeper slices than any top-1m list reaches. Reading stops
// as soon as enough domains are collected — the full file is never
// downloaded.
type CommonCrawlSeeder struct {
	// GraphInfoURL lists available graph releases. Defaults to
	// commonCrawlDefaultGraphInfoURL if empty.
	GraphInfoURL string

	// Graph is the release id to read (e.g. "cc-main-2026-apr-may-jun").
	// Empty resolves the newest release from GraphInfoURL.
	Graph string

	// DataURL is the base URL serving the ranks files. Defaults to
	// commonCrawlDefaultDataURL if empty; overridable for tests/mirrors.
	DataURL string

	// Offset skips the first Offset ranked domains before collecting, so
	// runs can sample deeper slices of the ranking.
	Offset int

	// HTTPClient is used for all requests. Defaults to a client with a 5m
	// timeout if nil (the ranks file is a large stream).
	HTTPClient *http.Client
}

// NewCommonCrawlSeeder returns a CommonCrawlSeeder for the given graph
// release id (empty resolves the newest release), skipping the first offset
// ranked domains.
func NewCommonCrawlSeeder(graphInfoURL, graph string, offset int) *CommonCrawlSeeder {
	return &CommonCrawlSeeder{GraphInfoURL: graphInfoURL, Graph: graph, Offset: offset}
}

// Source implements Seeder.
func (c *CommonCrawlSeeder) Source() string { return store.DiscoverySourceCommonCrawl }

func (c *CommonCrawlSeeder) graphInfoURL() string {
	if c.GraphInfoURL != "" {
		return c.GraphInfoURL
	}
	return commonCrawlDefaultGraphInfoURL
}

func (c *CommonCrawlSeeder) dataURL() string {
	if c.DataURL != "" {
		return strings.TrimSuffix(c.DataURL, "/")
	}
	return commonCrawlDefaultDataURL
}

func (c *CommonCrawlSeeder) httpClient() *http.Client {
	return newHTTPClient(c.HTTPClient, commonCrawlHTTPTimeout)
}

// Domains implements Seeder: it resolves the graph release (configured id or
// newest from graphinfo.json), then streams the release's gzipped
// domain-ranks TSV — skipping the header and the first Offset ranked rows —
// collecting reversed host_rev values ("com.googleapis" -> "googleapis.com")
// until n unique sanitized domains are gathered, at which point it stops
// reading and closes the stream.
func (c *CommonCrawlSeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: commoncrawl: n must be positive, got %d", n)
	}

	graph := c.Graph
	if graph == "" {
		var err error
		graph, err = c.latestGraph(ctx)
		if err != nil {
			return nil, err
		}
	}

	ranksURL := fmt.Sprintf("%s/projects/hyperlinkgraph/%s/domain/%s-domain-ranks.txt.gz",
		c.dataURL(), graph, graph)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ranksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("seed: commoncrawl: %w", err)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("seed: commoncrawl: request to %s: %w", ranksURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seed: commoncrawl: %s returned status %d", ranksURL, resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("seed: commoncrawl: reading gzip stream from %s: %w", ranksURL, err)
	}
	defer gz.Close()

	collector := newDomainCollector(n)
	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 64<<10), commonCrawlMaxLineBytes)

	skipped := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Columns: harmonicc_pos, harmonicc_val, pr_pos, pr_val, host_rev,
		// n_hosts — rows sorted by harmonic-centrality rank, best first.
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if skipped < c.Offset {
			skipped++
			continue
		}
		collector.add([]string{reverseHost(fields[4])})
		if collector.full() {
			// Stop reading: never pull the rest of the ~1 GB file.
			return collector.domains(), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("seed: commoncrawl: reading %s: %w", ranksURL, err)
	}

	return collector.domains(), nil
}

// latestGraph resolves the newest web-graph release id from graphinfo.json
// (the array is ordered newest-first).
func (c *CommonCrawlSeeder) latestGraph(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.graphInfoURL(), nil)
	if err != nil {
		return "", fmt.Errorf("seed: commoncrawl: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	var releases []struct {
		ID string `json:"id"`
	}
	if err := fetchJSON(c.httpClient(), req, 4<<20, omitStatusErrBody, &releases); err != nil {
		return "", fmt.Errorf("seed: commoncrawl: %w", err)
	}
	if len(releases) == 0 || releases[0].ID == "" {
		return "", fmt.Errorf("seed: commoncrawl: %s listed no graph releases", c.graphInfoURL())
	}
	return releases[0].ID, nil
}

// reverseHost decodes a Common Crawl host_rev value — a registrable domain
// with its labels reversed, e.g. "com.googleapis" -> "googleapis.com".
func reverseHost(hostRev string) string {
	labels := strings.Split(hostRev, ".")
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}
	return strings.Join(labels, ".")
}
