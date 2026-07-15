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

// CTSeeder fetches domain names from a Certificate Transparency log's
// get-sth / get-entries endpoints (RFC 6962). It implements Seeder with
// Source() "ct_log".
type CTSeeder struct {
	// LogURL is the base URL of the CT log, e.g.
	// "https://oak.ct.letsencrypt.org/2026h1/". A trailing slash is
	// added if missing.
	LogURL string

	// HTTPClient is used for all requests. Defaults to a client with a
	// 30s timeout if nil.
	HTTPClient *http.Client

	// EntriesPerPage overrides the chunk size used for get-entries
	// pagination. Defaults to ctDefaultEntriesPerPage if zero.
	EntriesPerPage int
}

// NewCTSeeder returns a CTSeeder for the given CT log base URL.
func NewCTSeeder(logURL string) *CTSeeder {
	return &CTSeeder{LogURL: logURL}
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

func (c *CTSeeder) endpoint(path string, query url.Values) (string, error) {
	base := c.LogURL
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
func (c *CTSeeder) getSTH(ctx context.Context) (int64, error) {
	endpoint, err := c.endpoint("ct/v1/get-sth", nil)
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
func (c *CTSeeder) getEntries(ctx context.Context, start, end int64) ([]ct.LeafEntry, error) {
	endpoint, err := c.endpoint("ct/v1/get-entries", url.Values{
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

// Domains implements Seeder: it fetches domain names harvested from the
// most recent n entries of the CT log. It walks backward from the current
// tree size, paginating get-entries in chunks, until n entries have been
// collected or the log is exhausted. Returned domains are sanitized (see
// Sanitize) but not yet deduped against any external store.
func (c *CTSeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: ct: n must be positive, got %d", n)
	}

	treeSize, err := c.getSTH(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: ct: get-sth: %w", err)
	}

	// The log contains leaf indices [0, treeSize-1]. We want the latest n
	// entries, i.e. the window [treeSize-n, treeSize-1], walked forward in
	// page-sized chunks. get-entries responses are always a contiguous
	// prefix of the requested [start, end] range (servers truncate from the
	// end when the requested span exceeds their per-response cap), so
	// advancing the cursor by the number of entries actually returned
	// correctly handles truncation.
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

		entries, err := c.getEntries(ctx, cursor, end)
		if err != nil {
			return nil, fmt.Errorf("seed: ct: get-entries[%d:%d]: %w", cursor, end, err)
		}
		if len(entries) == 0 {
			// Nothing more available; stop paginating.
			break
		}

		for i, entry := range entries {
			index := cursor + int64(i)
			leafNames, err := domainsFromLeaf(index, &entry)
			if err != nil {
				// A single unparsable leaf shouldn't abort the whole
				// fetch; skip it and continue.
				continue
			}
			names = append(names, leafNames...)
		}

		cursor += int64(len(entries))
	}

	return Sanitize(names), nil
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
