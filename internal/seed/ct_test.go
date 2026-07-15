package seed

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ct "github.com/google/certificate-transparency-go"
	"github.com/google/certificate-transparency-go/tls"
)

// makeLeafEntry builds a real, self-signed X.509 certificate with the given
// SAN DNS names (and CN) and wraps it into a CT LeafEntry, exactly like a
// production CT log would serve from get-entries.
func makeLeafEntry(t *testing.T, commonName string, dnsNames []string) ct.LeafEntry {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     dnsNames,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	leaf := ct.MerkleTreeLeaf{
		Version:  ct.V1,
		LeafType: ct.TimestampedEntryLeafType,
		TimestampedEntry: &ct.TimestampedEntry{
			Timestamp: uint64(time.Now().UnixMilli()),
			EntryType: ct.X509LogEntryType,
			X509Entry: &ct.ASN1Cert{Data: der},
		},
	}

	leafInput, err := tls.Marshal(leaf)
	if err != nil {
		t.Fatalf("marshaling leaf: %v", err)
	}

	extraData, err := tls.Marshal(ct.CertificateChain{})
	if err != nil {
		t.Fatalf("marshaling extra data: %v", err)
	}

	return ct.LeafEntry{LeafInput: leafInput, ExtraData: extraData}
}

// ctLogFixture is a minimal in-memory CT log server backing get-sth and
// get-entries, with a configurable per-response entry cap to exercise
// server-truncated responses.
type ctLogFixture struct {
	entries  []ct.LeafEntry
	pageCap  int
	requests []string
}

func newCTLogFixture(entries []ct.LeafEntry, pageCap int) *ctLogFixture {
	return &ctLogFixture{entries: entries, pageCap: pageCap}
}

func (f *ctLogFixture) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/ct/v1/get-sth", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.String())
		_ = json.NewEncoder(w).Encode(ctGetSTHResponse{TreeSize: int64(len(f.entries))})
	})
	mux.HandleFunc("/ct/v1/get-entries", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.String())
		var start, end int64
		fmt.Sscanf(r.URL.Query().Get("start"), "%d", &start)
		fmt.Sscanf(r.URL.Query().Get("end"), "%d", &end)

		if start < 0 || start >= int64(len(f.entries)) || end < start {
			_ = json.NewEncoder(w).Encode(ctGetEntriesResponse{})
			return
		}
		if end >= int64(len(f.entries)) {
			end = int64(len(f.entries)) - 1
		}

		count := end - start + 1
		if f.pageCap > 0 && count > int64(f.pageCap) {
			count = int64(f.pageCap)
		}
		out := f.entries[start : start+count]
		_ = json.NewEncoder(w).Encode(ctGetEntriesResponse{Entries: out})
	})
	return httptest.NewServer(mux)
}

func TestCTSeederDomains_SinglePage(t *testing.T) {
	entries := []ct.LeafEntry{
		makeLeafEntry(t, "one.example.com", []string{"one.example.com"}),
		makeLeafEntry(t, "two.example.com", []string{"two.example.com", "*.two.example.com"}),
		makeLeafEntry(t, "three.example.com", []string{"three.example.com"}),
	}
	fixture := newCTLogFixture(entries, 0)
	srv := fixture.server()
	defer srv.Close()

	seeder := &CTSeeder{Logs: []string{srv.URL}}
	names, err := seeder.Domains(context.Background(), 3)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}

	want := map[string]bool{"one.example.com": true, "two.example.com": true, "three.example.com": true}
	if len(names) != len(want) {
		t.Fatalf("got %v, want set %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected domain %q", n)
		}
	}

	if seeder.Source() != "ct_log" {
		t.Errorf("Source() = %q, want ct_log", seeder.Source())
	}
}

func TestCTSeederDomains_PaginationWithServerTruncation(t *testing.T) {
	const total = 10
	entries := make([]ct.LeafEntry, total)
	for i := 0; i < total; i++ {
		host := fmt.Sprintf("host%d.example.com", i)
		entries[i] = makeLeafEntry(t, host, []string{host})
	}
	// Cap the server at 3 entries per response, well below both the page
	// size we request and the total we want, forcing multiple truncated
	// round trips.
	fixture := newCTLogFixture(entries, 3)
	srv := fixture.server()
	defer srv.Close()

	seeder := &CTSeeder{Logs: []string{srv.URL}, EntriesPerPage: 7}
	names, err := seeder.Domains(context.Background(), 8)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}

	if len(names) != 8 {
		t.Fatalf("got %d names, want 8: %v", len(names), names)
	}
	// We asked for the latest 8 of 10 entries, i.e. indices 2..9.
	for i := 2; i < total; i++ {
		host := fmt.Sprintf("host%d.example.com", i)
		found := false
		for _, n := range names {
			if n == host {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q among fetched domains, got %v", host, names)
		}
	}

	if len(fixture.requests) < 4 {
		t.Errorf("expected multiple paginated requests due to truncation, got %d: %v", len(fixture.requests), fixture.requests)
	}
}

func TestCTSeederDomains_FewerEntriesThanRequested(t *testing.T) {
	entries := []ct.LeafEntry{
		makeLeafEntry(t, "only.example.com", []string{"only.example.com"}),
	}
	fixture := newCTLogFixture(entries, 0)
	srv := fixture.server()
	defer srv.Close()

	seeder := &CTSeeder{Logs: []string{srv.URL}}
	names, err := seeder.Domains(context.Background(), 100)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 1 || names[0] != "only.example.com" {
		t.Fatalf("got %v, want [only.example.com]", names)
	}
}

func TestCTSeederDomains_RejectsNonPositiveN(t *testing.T) {
	seeder := &CTSeeder{Logs: []string{"http://example.invalid"}}
	if _, err := seeder.Domains(context.Background(), 0); err == nil {
		t.Fatal("expected error for n=0")
	}
	if _, err := seeder.Domains(context.Background(), -1); err == nil {
		t.Fatal("expected error for negative n")
	}
}

func TestDomainsFromLeaf(t *testing.T) {
	entry := makeLeafEntry(t, "cn.example.com", []string{"san1.example.com", "san2.example.com"})
	names, err := domainsFromLeaf(0, &entry)
	if err != nil {
		t.Fatalf("domainsFromLeaf: %v", err)
	}
	want := map[string]bool{"san1.example.com": true, "san2.example.com": true, "cn.example.com": true}
	if len(names) != len(want) {
		t.Fatalf("got %v, want set %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected name %q", n)
		}
	}
}
