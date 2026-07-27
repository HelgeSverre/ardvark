package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	ct "github.com/google/certificate-transparency-go"
	"github.com/helgesverre/ardvark/internal/store"
)

// ctDefaultEntriesPerPage is the chunk size requested per get-entries call.
// Real logs cap this (commonly 256-1024); requesting fewer entries per page
// keeps memory bounded and works against any server-side cap, since logs are
// free to return fewer entries than requested (a "server-truncated"
// response).
const ctDefaultEntriesPerPage = 256

// ctMaxOverfetchFactor bounds how much further back a single log's window
// widens when a first pass of n leaf entries sanitizes/dedupes down to
// fewer than n usable domains. Reading raw leaf-entry count is only an
// approximation of usable domain count (a leaf may have no parsable SAN,
// or all its SANs may already be seen), so the window is allowed to widen
// up to this many times the original size before domainsFromLog gives up
// and returns whatever it has; this keeps the extra network cost of
// correcting for sanitization loss bounded rather than open-ended.
const ctMaxOverfetchFactor = 4

// CTSeeder fetches domain names from one or more Certificate Transparency
// logs' get-sth / get-entries endpoints (RFC 6962). It implements Seeder with
// Source() "ct_log".
type CTSeeder struct {
	// Logs are the base URLs of the CT logs to read, e.g.
	// "https://oak.ct.letsencrypt.org/2026h2/". Trailing slashes are added
	// if missing. Domains draws from each log in turn until n names are
	// collected.
	Logs []string

	// HTTPClient is used for all requests. Defaults to a client with a
	// 30s timeout if nil.
	HTTPClient *http.Client

	// EntriesPerPage overrides the chunk size used for get-entries
	// pagination. Defaults to ctDefaultEntriesPerPage if zero.
	EntriesPerPage int
}

// NewCTSeeder returns a CTSeeder reading the given CT log base URL(s).
func NewCTSeeder(logURLs ...string) *CTSeeder {
	return &CTSeeder{Logs: logURLs}
}

// NewCTSeederFromLogList resolves usable current logs for the given operator
// tokens from the CT log list at logListURL (empty = DefaultCTLogListURL) and
// returns a CTSeeder reading them. See ResolveCTLogs for token semantics.
func NewCTSeederFromLogList(ctx context.Context, httpClient *http.Client, logListURL string, operators []string, now time.Time) (*CTSeeder, error) {
	urls, err := ResolveCTLogs(ctx, httpClient, logListURL, operators, now)
	if err != nil {
		return nil, err
	}
	return &CTSeeder{Logs: urls, HTTPClient: httpClient}, nil
}

// Source implements Seeder.
func (c *CTSeeder) Source() string { return store.DiscoverySourceCTLog }

type ctGetSTHResponse struct {
	TreeSize int64 `json:"tree_size"`
}

type ctGetEntriesResponse struct {
	Entries []ct.LeafEntry `json:"entries"`
}

func (c *CTSeeder) httpClient() *http.Client {
	return newHTTPClient(c.HTTPClient, defaultHTTPTimeout)
}

func (c *CTSeeder) entriesPerPage() int {
	if c.EntriesPerPage > 0 {
		return c.EntriesPerPage
	}
	return ctDefaultEntriesPerPage
}

func (c *CTSeeder) endpoint(logURL, path string, query url.Values) (string, error) {
	base := logURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	u, err := url.Parse(base + path)
	if err != nil {
		return "", fmt.Errorf("seed: ct: invalid log URL: %w", err)
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

func (c *CTSeeder) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if err := fetchJSON(c.httpClient(), req, 32<<20, includeStatusErrBody, out); err != nil {
		return fmt.Errorf("seed: ct: %w", err)
	}
	return nil
}

// getSTH fetches the current tree size from the log's get-sth endpoint.
func (c *CTSeeder) getSTH(ctx context.Context, logURL string) (int64, error) {
	endpoint, err := c.endpoint(logURL, "ct/v1/get-sth", nil)
	if err != nil {
		return 0, err
	}
	var sth ctGetSTHResponse
	if err := c.getJSON(ctx, endpoint, &sth); err != nil {
		return 0, err
	}
	if sth.TreeSize <= 0 {
		return 0, fmt.Errorf("seed: ct: log reported non-positive tree size %d", sth.TreeSize)
	}
	return sth.TreeSize, nil
}

// getEntries fetches leaf entries in the inclusive range [start, end] from
// the log's get-entries endpoint. Logs may return fewer entries than
// requested (server-truncated responses); callers must paginate using the
// returned entry count.
func (c *CTSeeder) getEntries(ctx context.Context, logURL string, start, end int64) ([]ct.LeafEntry, error) {
	endpoint, err := c.endpoint(logURL, "ct/v1/get-entries", url.Values{
		"start": {strconv.FormatInt(start, 10)},
		"end":   {strconv.FormatInt(end, 10)},
	})
	if err != nil {
		return nil, err
	}
	var resp ctGetEntriesResponse
	if err := c.getJSON(ctx, endpoint, &resp); err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

// Domains implements Seeder: it harvests domain names from the most recent
// entries across the configured CT logs, drawing from each in turn until n
// sanitized, deduped domains are collected or all logs are exhausted. The
// per-log budget (remaining) is computed from the sanitized/deduped count
// collected so far, not the raw leaf-entry count, so sanitization loss in
// one log doesn't silently undercount the total.
func (c *CTSeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: ct: n must be positive, got %d", n)
	}
	if len(c.Logs) == 0 {
		return nil, fmt.Errorf("seed: ct: no logs configured")
	}

	collector := newDomainCollector(n)
	for _, logURL := range c.Logs {
		remaining := n - len(collector.domains())
		if remaining <= 0 {
			break
		}
		logNames, err := c.domainsFromLog(ctx, logURL, remaining)
		if err != nil {
			return nil, err
		}
		collector.add(logNames)
	}

	return collector.domains(), nil
}

// domainsFromLog harvests up to n sanitized, deduped SAN/CN domains from the
// latest entries of a single CT log. It reads windows of leaf entries
// backward from the newest, widening the window (up to
// ctMaxOverfetchFactor times the initial size) whenever a pass sanitizes
// down to fewer than n domains, since raw leaf-entry count only
// approximates usable domain count. Within each window, get-entries
// returns a contiguous prefix of each requested [start, end] range (servers
// truncate from the end at their per-response cap), so advancing the cursor
// by the number of entries actually returned correctly handles truncation.
func (c *CTSeeder) domainsFromLog(ctx context.Context, logURL string, n int) ([]string, error) {
	treeSize, err := c.getSTH(ctx, logURL)
	if err != nil {
		return nil, fmt.Errorf("seed: ct: get-sth: %w", err)
	}

	windowSize := int64(n)
	if windowSize < 1 {
		windowSize = 1
	}
	maxFetch := windowSize * ctMaxOverfetchFactor
	if maxFetch > treeSize {
		maxFetch = treeSize
	}
	page := int64(c.entriesPerPage())

	collector := newDomainCollector(n)
	end := treeSize - 1
	var fetched int64
	for end >= 0 && !collector.full() && fetched < maxFetch {
		start := end - windowSize + 1
		if start < 0 {
			start = 0
		}

		cursor := start
		for cursor <= end {
			pageEnd := cursor + page - 1
			if pageEnd > end {
				pageEnd = end
			}

			entries, err := c.getEntries(ctx, logURL, cursor, pageEnd)
			if err != nil {
				return nil, fmt.Errorf("seed: ct: get-entries[%d:%d]: %w", cursor, pageEnd, err)
			}
			if len(entries) == 0 {
				break
			}

			for i, entry := range entries {
				index := cursor + int64(i)
				leafNames, err := domainsFromLeaf(index, &entry)
				if err != nil {
					// A single unparsable leaf shouldn't abort the fetch.
					continue
				}
				collector.add(leafNames)
			}

			fetched += int64(len(entries))
			cursor += int64(len(entries))
			if collector.full() {
				break
			}
		}

		end = start - 1
	}

	return collector.domains(), nil
}

// domainsFromLeaf parses a single CT log leaf entry (X.509 or precert) and
// returns its SAN DNS names, plus the certificate subject common name.
func domainsFromLeaf(index int64, entry *ct.LeafEntry) ([]string, error) {
	rawEntry, err := ct.RawLogEntryFromLeaf(index, entry)
	if err != nil {
		return nil, fmt.Errorf("seed: ct: parsing leaf %d: %w", index, err)
	}
	logEntry, err := rawEntry.ToLogEntry()
	if err != nil {
		return nil, fmt.Errorf("seed: ct: converting leaf %d: %w", index, err)
	}

	var dnsNames []string
	var commonName string
	switch {
	case logEntry.X509Cert != nil:
		dnsNames = logEntry.X509Cert.DNSNames
		commonName = logEntry.X509Cert.Subject.CommonName
	case logEntry.Precert != nil && logEntry.Precert.TBSCertificate != nil:
		dnsNames = logEntry.Precert.TBSCertificate.DNSNames
		commonName = logEntry.Precert.TBSCertificate.Subject.CommonName
	default:
		return nil, fmt.Errorf("seed: ct: leaf %d has neither X.509 cert nor precert", index)
	}

	if commonName != "" {
		dnsNames = append(dnsNames, commonName)
	}
	return dnsNames, nil
}
