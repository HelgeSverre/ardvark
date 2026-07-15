// Package ctseed bootstraps the crawl frontier by fetching recent entries
// from a Certificate Transparency (CT) log and extracting candidate domain
// names from the certificates' Subject Alternative Names.
package ctseed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	ct "github.com/google/certificate-transparency-go"
)

// defaultEntriesPerPage is the chunk size requested per get-entries call.
// Real logs cap this (commonly 256-1024); requesting fewer entries per page
// keeps memory bounded and works against any server-side cap, since logs are
// free to return fewer entries than requested (a "server-truncated"
// response).
const defaultEntriesPerPage = 256

// Client fetches domain names from a CT log's get-sth / get-entries
// endpoints.
type Client struct {
	// LogURL is the base URL of the CT log, e.g.
	// "https://oak.ct.letsencrypt.org/2026h1/". A trailing slash is
	// added if missing.
	LogURL string

	// HTTPClient is used for all requests. Defaults to a client with a
	// 30s timeout if nil.
	HTTPClient *http.Client

	// EntriesPerPage overrides the chunk size used for get-entries
	// pagination. Defaults to defaultEntriesPerPage if zero.
	EntriesPerPage int
}

// NewClient returns a Client for the given CT log base URL.
func NewClient(logURL string) *Client {
	return &Client{LogURL: logURL}
}

type getSTHResponse struct {
	TreeSize int64 `json:"tree_size"`
}

type getEntriesResponse struct {
	Entries []ct.LeafEntry `json:"entries"`
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) entriesPerPage() int {
	if c.EntriesPerPage > 0 {
		return c.EntriesPerPage
	}
	return defaultEntriesPerPage
}

func (c *Client) endpoint(path string, query url.Values) (string, error) {
	base := c.LogURL
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	u, err := url.Parse(base + path)
	if err != nil {
		return "", fmt.Errorf("ctseed: invalid log URL: %w", err)
	}
	if query != nil {
		u.RawQuery = query.Encode()
	}
	return u.String(), nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("ctseed: request to %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("ctseed: reading response from %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ctseed: %s returned status %d: %s", endpoint, resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("ctseed: decoding response from %s: %w", endpoint, err)
	}
	return nil
}

// getSTH fetches the current tree size from the log's get-sth endpoint.
func (c *Client) getSTH(ctx context.Context) (int64, error) {
	endpoint, err := c.endpoint("ct/v1/get-sth", nil)
	if err != nil {
		return 0, err
	}
	var sth getSTHResponse
	if err := c.getJSON(ctx, endpoint, &sth); err != nil {
		return 0, err
	}
	if sth.TreeSize <= 0 {
		return 0, fmt.Errorf("ctseed: log reported non-positive tree size %d", sth.TreeSize)
	}
	return sth.TreeSize, nil
}

// getEntries fetches leaf entries in the inclusive range [start, end] from
// the log's get-entries endpoint. Logs may return fewer entries than
// requested (server-truncated responses); callers must paginate using the
// returned entry count.
func (c *Client) getEntries(ctx context.Context, start, end int64) ([]ct.LeafEntry, error) {
	endpoint, err := c.endpoint("ct/v1/get-entries", url.Values{
		"start": {fmt.Sprintf("%d", start)},
		"end":   {fmt.Sprintf("%d", end)},
	})
	if err != nil {
		return nil, err
	}
	var resp getEntriesResponse
	if err := c.getJSON(ctx, endpoint, &resp); err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

// FetchLatest fetches domain names harvested from the most recent n entries
// of the CT log. It walks backward from the current tree size, paginating
// get-entries in chunks, until n entries have been collected or the log is
// exhausted. Returned domains are sanitized (see Sanitize) but not yet
// deduped against any external store.
func (c *Client) FetchLatest(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("ctseed: n must be positive, got %d", n)
	}

	treeSize, err := c.getSTH(ctx)
	if err != nil {
		return nil, fmt.Errorf("ctseed: get-sth: %w", err)
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
			return nil, fmt.Errorf("ctseed: get-entries[%d:%d]: %w", cursor, end, err)
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
		return nil, fmt.Errorf("ctseed: parsing leaf %d: %w", index, err)
	}
	logEntry, err := rawEntry.ToLogEntry()
	if err != nil {
		return nil, fmt.Errorf("ctseed: converting leaf %d: %w", index, err)
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
		return nil, fmt.Errorf("ctseed: leaf %d has neither X.509 cert nor precert", index)
	}

	if commonName != "" {
		dnsNames = append(dnsNames, commonName)
	}
	return dnsNames, nil
}

// Sanitize normalizes a list of raw SAN/CN values into a deduped list of
// plausible domain names:
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
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		name = strings.TrimPrefix(name, "*.")
		name = strings.ToLower(name)
		name = strings.TrimSuffix(name, ".")

		if !strings.Contains(name, ".") {
			continue
		}
		if net.ParseIP(name) != nil {
			continue
		}
		if !isValidHostname(name) {
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
