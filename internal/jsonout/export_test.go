package jsonout

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/store"
)

// seedExportRows creates one domain with one catalog holding two entries, so
// export tests have a stable, ordered, non-empty result set.
func seedExportRows(t *testing.T, st *store.Store) {
	t.Helper()

	dom, err := st.UpsertDomain("acme.test", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	cat := &store.Catalog{
		DomainID:           dom.ID,
		SourceURL:          "https://acme.test/.well-known/ai-catalog.json",
		SpecVersion:        "1.0",
		ContentHash:        "abc123",
		FetchedAt:          time.Now(),
		VerificationStatus: store.VerificationStatusValid,
		Entries: []store.CatalogEntry{
			{URN: "urn:air:acme.test:tool:alpha", URNPublisher: "acme.test", DisplayName: "Alpha", MediaType: "application/vnd.mcp.server+json", RefURL: "https://acme.test/alpha"},
			{URN: "urn:air:acme.test:tool:beta", URNPublisher: "acme.test", DisplayName: "Beta,Quoted", MediaType: "application/vnd.agent-card+json", RefURL: "https://acme.test/beta"},
		},
	}
	if err := st.DB.Create(cat).Error; err != nil {
		t.Fatalf("creating catalog: %v", err)
	}
}

func TestExport_RejectsUnknownFormat(t *testing.T) {
	st := newTestStore(t)

	if _, err := Export(st, "xml", "", &bytes.Buffer{}, false); err == nil {
		t.Fatal("Export(format=xml) error = nil, want a format error")
	}
}

func TestExport_JSONLToStdout(t *testing.T) {
	st := newTestStore(t)
	seedExportRows(t, st)

	var out bytes.Buffer
	res, err := Export(st, "jsonl", "", &out, false)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Rows != 2 {
		t.Errorf("Rows = %d, want 2", res.Rows)
	}
	if res.Out != "" {
		t.Errorf("Out = %q, want empty for stdout", res.Out)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2: %q", len(lines), out.String())
	}
	var first ExportRow
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("decoding first jsonl row: %v", err)
	}
	if first.Host != "acme.test" || first.URN != "urn:air:acme.test:tool:alpha" {
		t.Errorf("first row = %+v, want acme.test/alpha", first)
	}
}

// The file path is the case the buffered writer can silently lose: rows sit
// in the bufio buffer until Export flushes AND closes, so a reader that only
// sees a truncated file proves the close path regressed.
func TestExport_JSONLToFileIsFlushedAndClosed(t *testing.T) {
	st := newTestStore(t)
	seedExportRows(t, st)

	outPath := filepath.Join(t.TempDir(), "export.jsonl")
	res, err := Export(st, "jsonl", outPath, &bytes.Buffer{}, false)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if res.Rows != 2 {
		t.Errorf("Rows = %d, want 2", res.Rows)
	}
	if res.Out != outPath {
		t.Errorf("Out = %q, want %q", res.Out, outPath)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading export file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("file has %d lines, want 2: %q", len(lines), string(data))
	}
	for i, line := range lines {
		var row ExportRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("decoding file line %d: %v", i, err)
		}
	}
}

func TestExport_ExistingFileRequiresOverwrite(t *testing.T) {
	st := newTestStore(t)
	seedExportRows(t, st)

	outPath := filepath.Join(t.TempDir(), "existing.jsonl")
	if err := os.WriteFile(outPath, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("writing existing output: %v", err)
	}
	if _, err := Export(st, "jsonl", outPath, &bytes.Buffer{}, false); err == nil {
		t.Fatal("Export without overwrite: want error")
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading existing output: %v", err)
	}
	if string(data) != "keep me" {
		t.Fatalf("existing output changed to %q", data)
	}

	res, err := Export(st, "jsonl", outPath, &bytes.Buffer{}, true)
	if err != nil {
		t.Fatalf("Export with overwrite: %v", err)
	}
	if res.Rows != 2 {
		t.Errorf("Export with overwrite rows = %d, want 2", res.Rows)
	}
	data, err = os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading overwritten output: %v", err)
	}
	if string(data) == "keep me" {
		t.Fatal("Export with overwrite did not replace output")
	}
}

func TestExport_CSVToFileHasHeaderAndQuotedRows(t *testing.T) {
	st := newTestStore(t)
	seedExportRows(t, st)

	outPath := filepath.Join(t.TempDir(), "export.csv")
	if _, err := Export(st, "csv", outPath, &bytes.Buffer{}, false); err != nil {
		t.Fatalf("Export: %v", err)
	}

	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("opening export file: %v", err)
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parsing csv: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("csv has %d records, want header + 2 rows", len(records))
	}
	if records[0][0] != "host" {
		t.Errorf("header[0] = %q, want %q", records[0][0], "host")
	}
	// A comma inside a field must survive the round trip as one field.
	if got := records[2][5]; got != "Beta,Quoted" {
		t.Errorf("display_name = %q, want %q", got, "Beta,Quoted")
	}
}

// Export must not leave a partial file behind when the format is rejected.
func TestExport_InvalidFormatDoesNotCreateOutputFile(t *testing.T) {
	st := newTestStore(t)
	seedExportRows(t, st)

	outPath := filepath.Join(t.TempDir(), "never-written.jsonl")
	if _, err := Export(st, "tsv", outPath, &bytes.Buffer{}, false); err == nil {
		t.Fatal("Export(format=tsv) error = nil, want a format error")
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("output file exists after rejected format (stat err = %v)", err)
	}
}
