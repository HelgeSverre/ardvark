package jsonout

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/helgesverre/ardvark/internal/store"
)

// ExportRow is one flattened catalog_entries row joined with its owning
// catalog and domain, ready for JSONL/CSV serialization.
type ExportRow struct {
	Host                  string `json:"host" csv:"host"`
	CatalogSourceURL      string `json:"catalog_source_url" csv:"catalog_source_url"`
	VerificationStatus    string `json:"verification_status" csv:"verification_status"`
	URN                   string `json:"urn" csv:"urn"`
	URNPublisher          string `json:"urn_publisher" csv:"urn_publisher"`
	DisplayName           string `json:"display_name" csv:"display_name"`
	MediaType             string `json:"media_type" csv:"media_type"`
	RefURL                string `json:"ref_url" csv:"ref_url"`
	Description           string `json:"description" csv:"description"`
	Version               string `json:"version" csv:"version"`
	Source                string `json:"source" csv:"source"`
	Tags                  string `json:"tags" csv:"tags"`
	RepresentativeQueries string `json:"representative_queries" csv:"representative_queries"`
}

// ExportResult summarizes a completed export: what format, where it was
// written (empty for stdout), and how many rows.
type ExportResult struct {
	Format string `json:"format"`
	Out    string `json:"out,omitempty"`
	Rows   int    `json:"rows"`
}

// exportQuery is the flattened catalog_entries/catalogs/domains join in
// stable entry order. Plain SQL against database/sql rather than a GORM
// Scan: exports run over millions of rows, and per-row reflection costs
// about 2s per million rows on top of this query.
const exportQuery = `SELECT domains.host, catalogs.source_url, catalogs.verification_status,
	catalog_entries.urn, catalog_entries.urn_publisher, catalog_entries.display_name,
	catalog_entries.media_type, catalog_entries.ref_url, catalog_entries.description,
	catalog_entries.version, catalog_entries.source, catalog_entries.tags,
	catalog_entries.representative_queries
	FROM catalog_entries
	JOIN catalogs ON catalogs.id = catalog_entries.catalog_id
	JOIN domains ON domains.id = catalogs.domain_id
	ORDER BY catalog_entries.id`

// exportWriteBufferSize sits at the top of the sequential-write plateau
// (64–256 KB); larger buffers stop paying.
const exportWriteBufferSize = 256 * 1024

// Export dumps every catalog entry, joined with domain and verification
// status, as JSONL or CSV — to the file at outPath when non-empty, else to
// stdout. Rows stream from the database cursor straight to the writer, so
// memory use is constant regardless of dataset size and output starts
// immediately.
func Export(st *store.Store, format, outPath string, stdout io.Writer) (ExportResult, error) {
	if format != "jsonl" && format != "csv" {
		return ExportResult{}, fmt.Errorf("export: --format must be \"jsonl\" or \"csv\", got %q", format)
	}

	w := stdout
	if outPath != "" {
		f, cerr := os.Create(outPath)
		if cerr != nil {
			return ExportResult{}, fmt.Errorf("export: creating %s: %w", outPath, cerr)
		}
		defer f.Close()
		w = f
	}
	bw := bufio.NewWriterSize(w, exportWriteBufferSize)

	sqlDB, err := st.DB.DB()
	if err != nil {
		return ExportResult{}, fmt.Errorf("export: unwrapping database handle: %w", err)
	}
	rows, err := sqlDB.Query(exportQuery)
	if err != nil {
		return ExportResult{}, fmt.Errorf("export: querying catalog entries: %w", err)
	}
	defer rows.Close()

	var (
		r       ExportRow
		n       int
		scratch = make([]byte, 0, 4096)
		cw      *csv.Writer
	)
	if format == "csv" {
		cw = csv.NewWriter(bw)
		if err := cw.Write(csvHeader); err != nil {
			return ExportResult{}, fmt.Errorf("export: writing csv header: %w", err)
		}
	}

	for rows.Next() {
		if err := rows.Scan(&r.Host, &r.CatalogSourceURL, &r.VerificationStatus,
			&r.URN, &r.URNPublisher, &r.DisplayName, &r.MediaType, &r.RefURL,
			&r.Description, &r.Version, &r.Source, &r.Tags, &r.RepresentativeQueries); err != nil {
			return ExportResult{}, fmt.Errorf("export: scanning row %d: %w", n+1, err)
		}
		switch format {
		case "jsonl":
			scratch = appendRowJSON(scratch[:0], &r)
			if _, err := bw.Write(scratch); err != nil {
				return ExportResult{}, fmt.Errorf("export: writing jsonl row: %w", err)
			}
		case "csv":
			if err := cw.Write(r.record()); err != nil {
				return ExportResult{}, fmt.Errorf("export: writing csv row: %w", err)
			}
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return ExportResult{}, fmt.Errorf("export: iterating catalog entries: %w", err)
	}

	if cw != nil {
		cw.Flush()
		if err := cw.Error(); err != nil {
			return ExportResult{}, fmt.Errorf("export: flushing csv: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return ExportResult{}, fmt.Errorf("export: flushing output: %w", err)
	}

	return ExportResult{Format: format, Out: outPath, Rows: n}, nil
}

var csvHeader = []string{
	"host", "catalog_source_url", "verification_status", "urn", "urn_publisher",
	"display_name", "media_type", "ref_url", "description", "version", "source",
	"tags", "representative_queries",
}

func (r *ExportRow) record() []string {
	return []string{
		r.Host, r.CatalogSourceURL, r.VerificationStatus, r.URN, r.URNPublisher,
		r.DisplayName, r.MediaType, r.RefURL, r.Description, r.Version, r.Source,
		r.Tags, r.RepresentativeQueries,
	}
}

// WriteJSONL writes rows as newline-delimited JSON objects. Kept for
// callers that already hold rows in memory; Export streams instead.
func WriteJSONL(w io.Writer, rows []ExportRow) error {
	scratch := make([]byte, 0, 4096)
	for i := range rows {
		scratch = appendRowJSON(scratch[:0], &rows[i])
		if _, err := w.Write(scratch); err != nil {
			return fmt.Errorf("export: encoding jsonl row: %w", err)
		}
	}
	return nil
}

// WriteCSV writes rows as CSV with a header row. Kept for callers that
// already hold rows in memory; Export streams instead.
func WriteCSV(w io.Writer, rows []ExportRow) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return fmt.Errorf("export: writing csv header: %w", err)
	}
	for i := range rows {
		if err := cw.Write(rows[i].record()); err != nil {
			return fmt.Errorf("export: writing csv row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}

// appendRowJSON serializes one row as a JSON object plus trailing newline,
// byte-identical to encoding/json's output for ExportRow. Hand-rolled
// because the export hot loop runs millions of times: reusing the caller's
// scratch buffer avoids the per-row allocations of json.Marshal and is
// ~1.5x faster end-to-end (see the escaping equivalence test).
func appendRowJSON(b []byte, r *ExportRow) []byte {
	b = append(b, '{')
	b = appendField(b, "host", r.Host, false)
	b = appendField(b, "catalog_source_url", r.CatalogSourceURL, true)
	b = appendField(b, "verification_status", r.VerificationStatus, true)
	b = appendField(b, "urn", r.URN, true)
	b = appendField(b, "urn_publisher", r.URNPublisher, true)
	b = appendField(b, "display_name", r.DisplayName, true)
	b = appendField(b, "media_type", r.MediaType, true)
	b = appendField(b, "ref_url", r.RefURL, true)
	b = appendField(b, "description", r.Description, true)
	b = appendField(b, "version", r.Version, true)
	b = appendField(b, "source", r.Source, true)
	b = appendField(b, "tags", r.Tags, true)
	b = appendField(b, "representative_queries", r.RepresentativeQueries, true)
	return append(b, '}', '\n')
}

func appendField(b []byte, key, val string, comma bool) []byte {
	if comma {
		b = append(b, ',')
	}
	b = append(b, '"')
	b = append(b, key...) // keys are fixed identifiers, nothing to escape
	b = append(b, '"', ':')
	return appendJSONString(b, val)
}

const hexDigits = "0123456789abcdef"

// appendJSONString appends s as a JSON string, mirroring encoding/json's
// escaping exactly: ", \, control characters, the HTML-unsafe <, >, &, the
// U+2028/U+2029 line separators, and invalid UTF-8 replaced with the
// \ufffd escape. TestAppendJSONString_MatchesEncodingJSON pins the
// equivalence.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' && c != '<' && c != '>' && c != '&' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"':
				b = append(b, '\\', '"')
			case '\\':
				b = append(b, '\\', '\\')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			case '\b':
				b = append(b, '\\', 'b')
			case '\f':
				b = append(b, '\\', 'f')
			default:
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, '\\', 'u', 'f', 'f', 'f', 'd')
			i += size
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			b = append(b, s[start:i]...)
			b = append(b, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}
