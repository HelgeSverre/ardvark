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

	ct "github.com/google/certificate-transparency-go"
)

// ctDefaultEntriesPerPage is the chunk size requested per get-entries call.
// Real logs cap this (commonly 256-1024); requesting fewer entries per page
// keeps memory bounded and works against any server-side cap, since logs are
// free to return fewer entries than requested (a "server-truncated"
// response).
const ctDefaultEntriesPerPage = 256

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
func (c *CTSeeder) Source() string { return "ct_log" }

type ctGetSTHResponse struct {
	TreeSize int64 `json:"tree_size"`
}

type ctGetEntriesResponse struct {
	Entries []ct.LeafEntry `json:"entries"`
}

func (c *CTSeeder) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
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
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("seed: ct: request to %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("seed: ct: reading response from %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("seed: ct: %s returned status %d: %s", endpoint, resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("seed: ct: decoding response from %s: %w", endpoint, err)
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
		"start": {fmt.Sprintf("%d", start)},
		"end":   {fmt.Sprintf("%d", end)},
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
// names are collected or all logs are exhausted. Returned domains are
// sanitized (see Sanitize) but not yet deduped against any external store.
func (c *CTSeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: ct: n must be positive, got %d", n)
	}
	if len(c.Logs) == 0 {
		return nil, fmt.Errorf("seed: ct: no logs configured")
	}

	var names []string
	for _, logURL := range c.Logs {
		remaining := n - len(names)
		if remaining <= 0 {
			break
		}
		logNames, err := c.domainsFromLog(ctx, logURL, remaining)
		if err != nil {
			return nil, err
		}
		names = append(names, logNames...)
	}

	return Sanitize(names), nil
}

// domainsFromLog harvests up to n raw (unsanitized) SAN/CN names from the
// latest entries of a single CT log. It reads the window
// [treeSize-n, treeSize-1] forward in page-sized chunks; get-entries returns a
// contiguous prefix of each requested [start, end] range (servers truncate
// from the end at their per-response cap), so advancing the cursor by the
// number of entries actually returned correctly handles truncation.
func (c *CTSeeder) domainsFromLog(ctx context.Context, logURL string, n int) ([]string, error) {
	treeSize, err := c.getSTH(ctx, logURL)
	if err != nil {
		return nil, fmt.Errorf("seed: ct: get-sth: %w", err)
	}

	target := treeSize - 1
	cursor := target - int64(n) + 1
	if cursor < 0 {
		cursor = 0
	}
	page := int64(c.entriesPerPage())

	var names []string
	for cursor <= target {
		end := cursor + page - 1
		if end > target {
			end = target
		}

		entries, err := c.getEntries(ctx, logURL, cursor, end)
		if err != nil {
			return nil, fmt.Errorf("seed: ct: get-entries[%d:%d]: %w", cursor, end, err)
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
			names = append(names, leafNames...)
		}

		cursor += int64(len(entries))
	}

	return names, nil
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
