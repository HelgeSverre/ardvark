package seed

import (
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// gzipRanksFixture compresses TSV lines into a gzip body matching the
// Common Crawl domain-ranks distribution format.
func gzipRanksFixture(t *testing.T, lines []string) []byte {
	t.Helper()

	var buf []byte
	w := newGzipBuffer(&buf)
	for _, line := range lines {
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("writing gzip fixture: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing gzip fixture: %v", err)
	}
	return buf
}

// newGzipBuffer returns a gzip.Writer appending to *buf via a tiny adapter.
func newGzipBuffer(buf *[]byte) *gzip.Writer {
	return gzip.NewWriter(appendWriter{buf})
}

type appendWriter struct{ buf *[]byte }

func (a appendWriter) Write(p []byte) (int, error) {
	*a.buf = append(*a.buf, p...)
	return len(p), nil
}

// ranksFixtureLines is a small domain-ranks file: header, ranked rows with
// reversed hosts, and one bogus row that must be skipped.
var ranksFixtureLines = []string{
	"#harmonicc_pos\tharmonicc_val\tpr_pos\tpr_val\thost_rev\tn_hosts",
	"1\t0.9\t1\t0.9\tcom.googleapis\t100",
	"2\t0.8\t2\t0.8\torg.wikipedia\t50",
	"bogus line without enough columns",
	"3\t0.7\t3\t0.7\tio.github.acme\t10",
	"4\t0.6\t4\t0.6\tcom.example\t5",
}

// newCommonCrawlTestServer serves graphinfo.json (newest release first) and
// the corresponding ranks file for graph id "cc-test-2026".
func newCommonCrawlTestServer(t *testing.T, ranksBody []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/graphinfo.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id": "cc-test-2026"}, {"id": "cc-test-2025"}]`))
	})
	mux.HandleFunc("/projects/hyperlinkgraph/cc-test-2026/domain/cc-test-2026-domain-ranks.txt.gz",
		func(w http.ResponseWriter, r *http.Request) {
			w.Write(ranksBody)
		})
	return httptest.NewServer(mux)
}

func TestCommonCrawlSeederDomains_ReversalAndGraphResolution(t *testing.T) {
	srv := newCommonCrawlTestServer(t, gzipRanksFixture(t, ranksFixtureLines))
	defer srv.Close()

	seeder := &CommonCrawlSeeder{
		GraphInfoURL: srv.URL + "/graphinfo.json",
		DataURL:      srv.URL,
	}
	names, err := seeder.Domains(context.Background(), 100)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	want := []string{"googleapis.com", "wikipedia.org", "acme.github.io", "example.com"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}

	if seeder.Source() != "commoncrawl" {
		t.Errorf("Source() = %q, want commoncrawl", seeder.Source())
	}
}

func TestCommonCrawlSeederDomains_ExplicitGraphSkipsGraphInfo(t *testing.T) {
	srv := newCommonCrawlTestServer(t, gzipRanksFixture(t, ranksFixtureLines))
	defer srv.Close()

	seeder := &CommonCrawlSeeder{
		GraphInfoURL: "http://graphinfo.invalid/graphinfo.json", // must not be fetched
		Graph:        "cc-test-2026",
		DataURL:      srv.URL,
	}
	names, err := seeder.Domains(context.Background(), 1)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 1 || names[0] != "googleapis.com" {
		t.Fatalf("got %v, want [googleapis.com]", names)
	}
}

func TestCommonCrawlSeederDomains_Offset(t *testing.T) {
	srv := newCommonCrawlTestServer(t, gzipRanksFixture(t, ranksFixtureLines))
	defer srv.Close()

	seeder := &CommonCrawlSeeder{
		GraphInfoURL: srv.URL + "/graphinfo.json",
		DataURL:      srv.URL,
		Offset:       2,
	}
	names, err := seeder.Domains(context.Background(), 100)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	want := []string{"acme.github.io", "example.com"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestCommonCrawlSeederDomains_Cap(t *testing.T) {
	srv := newCommonCrawlTestServer(t, gzipRanksFixture(t, ranksFixtureLines))
	defer srv.Close()

	seeder := &CommonCrawlSeeder{
		GraphInfoURL: srv.URL + "/graphinfo.json",
		DataURL:      srv.URL,
	}
	names, err := seeder.Domains(context.Background(), 2)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 2 || names[0] != "googleapis.com" || names[1] != "wikipedia.org" {
		t.Fatalf("got %v, want [googleapis.com wikipedia.org]", names)
	}
}

// Early stop must not error when the server would serve far more data than
// the requested count needs: collection halts mid-stream after n domains.
func TestCommonCrawlSeederDomains_EarlyStopOnLargeStream(t *testing.T) {
	lines := []string{"#harmonicc_pos\tharmonicc_val\tpr_pos\tpr_val\thost_rev\tn_hosts"}
	for i := 0; i < 50000; i++ {
		lines = append(lines, fmt.Sprintf("%d\t0.5\t%d\t0.5\tcom.domain-%d\t1", i+1, i+1, i))
	}
	srv := newCommonCrawlTestServer(t, gzipRanksFixture(t, lines))
	defer srv.Close()

	seeder := &CommonCrawlSeeder{
		GraphInfoURL: srv.URL + "/graphinfo.json",
		DataURL:      srv.URL,
	}
	names, err := seeder.Domains(context.Background(), 3)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	want := []string{"domain-0.com", "domain-1.com", "domain-2.com"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestCommonCrawlSeederDomains_RejectsNonPositiveN(t *testing.T) {
	seeder := &CommonCrawlSeeder{GraphInfoURL: "http://example.invalid"}
	if _, err := seeder.Domains(context.Background(), 0); err == nil {
		t.Fatal("expected error for n=0")
	}
}

func TestCommonCrawlSeederDomains_EmptyGraphInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	seeder := &CommonCrawlSeeder{GraphInfoURL: srv.URL, DataURL: srv.URL}
	if _, err := seeder.Domains(context.Background(), 1); err == nil {
		t.Fatal("expected error for empty graphinfo release list")
	}
}
