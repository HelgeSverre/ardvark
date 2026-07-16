package seed

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/helgesverre/ardvark/internal/store"
)

// trancoDefaultListURL is the default Tranco top-domains list, a zipped CSV
// of "rank,domain" rows.
const trancoDefaultListURL = "https://tranco-list.eu/top-1m.csv.zip"

// TrancoSeeder downloads the Tranco (https://tranco-list.eu) top-domains
// list and yields the top N domains. It implements Seeder with
// Source() "tranco". Tranco covers the established web that CT-log seeding
// (which only sees freshly issued certs) misses; it is complementary to
// CTSeeder, not a replacement.
type TrancoSeeder struct {
	// ListURL is the zipped CSV list to download. Defaults to
	// trancoDefaultListURL if empty.
	ListURL string

	// HTTPClient is used for the list download. Defaults to a client with
	// a 60s timeout if nil (the list is tens of megabytes).
	HTTPClient *http.Client
}

// NewTrancoSeeder returns a TrancoSeeder for the given list URL (empty uses
// the default Tranco top-1m list).
func NewTrancoSeeder(listURL string) *TrancoSeeder {
	return &TrancoSeeder{ListURL: listURL}
}

// Source implements Seeder.
func (t *TrancoSeeder) Source() string { return store.DiscoverySourceTranco }

func (t *TrancoSeeder) listURL() string {
	if t.ListURL != "" {
		return t.ListURL
	}
	return trancoDefaultListURL
}

func (t *TrancoSeeder) httpClient() *http.Client {
	return newHTTPClient(t.HTTPClient, trancoHTTPTimeout)
}

// Domains implements Seeder: it downloads the Tranco list (a zip archive
// containing a single "rank,domain" CSV) and returns the top n domains,
// sanitized. Ranking order from the source list is preserved.
func (t *TrancoSeeder) Domains(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("seed: tranco: n must be positive, got %d", n)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.listURL(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("seed: tranco: request to %s: %w", t.listURL(), err)
	}
	defer resp.Body.Close()

	// The list is tens of megabytes zipped; cap generously to bound memory
	// against a misbehaving/misconfigured URL.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("seed: tranco: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("seed: tranco: %s returned status %d", t.listURL(), resp.StatusCode)
	}

	return domainsFromTrancoZip(body, n)
}

// domainsFromTrancoZip reads "domain" values (second column) from the first
// CSV file found in a Tranco zip archive, in file order (Tranco lists are
// already rank-sorted), sanitizing and deduping as it goes via a
// domainCollector. It keeps consuming rows — not merely counting them —
// until n valid domains are collected or the CSV is exhausted, since some
// rows are filtered out by sanitization (wildcards resolve to a domain
// already seen, malformed hostnames, …) and stopping at n raw rows read
// would silently under-return.
func domainsFromTrancoZip(zipBytes []byte, n int) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("seed: tranco: reading zip archive: %w", err)
	}

	var csvFile *zip.File
	for _, f := range zr.File {
		if strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
			csvFile = f
			break
		}
	}
	if csvFile == nil {
		return nil, fmt.Errorf("seed: tranco: no .csv file found in archive")
	}

	rc, err := csvFile.Open()
	if err != nil {
		return nil, fmt.Errorf("seed: tranco: opening %s: %w", csvFile.Name, err)
	}
	defer rc.Close()

	reader := csv.NewReader(rc)
	reader.FieldsPerRecord = -1

	collector := newDomainCollector(n)
	for !collector.full() {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("seed: tranco: parsing %s: %w", csvFile.Name, err)
		}
		if len(record) < 2 {
			continue
		}
		collector.add([]string{record[1]})
	}

	return collector.domains(), nil
}
