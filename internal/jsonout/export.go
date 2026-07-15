package jsonout

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"

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

// Export dumps every catalog entry, joined with domain and verification
// status, as JSONL or CSV — to the file at outPath when non-empty, else to
// stdout.
func Export(st *store.Store, format, outPath string, stdout io.Writer) (ExportResult, error) {
	if format != "jsonl" && format != "csv" {
		return ExportResult{}, fmt.Errorf("export: --format must be \"jsonl\" or \"csv\", got %q", format)
	}

	rows, err := ExportRows(st)
	if err != nil {
		return ExportResult{}, err
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

	switch format {
	case "jsonl":
		err = WriteJSONL(w, rows)
	case "csv":
		err = WriteCSV(w, rows)
	}
	if err != nil {
		return ExportResult{}, err
	}

	return ExportResult{Format: format, Out: outPath, Rows: len(rows)}, nil
}

// ExportRows queries the flattened catalog_entries/catalogs/domains join in
// stable entry order.
func ExportRows(st *store.Store) ([]ExportRow, error) {
	var rows []ExportRow
	err := st.DB.Table("catalog_entries").
		Select(`domains.host AS host,
			catalogs.source_url AS catalog_source_url,
			catalogs.verification_status AS verification_status,
			catalog_entries.urn AS urn,
			catalog_entries.urn_publisher AS urn_publisher,
			catalog_entries.display_name AS display_name,
			catalog_entries.media_type AS media_type,
			catalog_entries.ref_url AS ref_url,
			catalog_entries.description AS description,
			catalog_entries.version AS version,
			catalog_entries.source AS source,
			catalog_entries.tags AS tags,
			catalog_entries.representative_queries AS representative_queries`).
		Joins("JOIN catalogs ON catalogs.id = catalog_entries.catalog_id").
		Joins("JOIN domains ON domains.id = catalogs.domain_id").
		Order("catalog_entries.id").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("export: querying catalog entries: %w", err)
	}
	return rows, nil
}

// WriteJSONL writes rows as newline-delimited JSON objects.
func WriteJSONL(w io.Writer, rows []ExportRow) error {
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("export: encoding jsonl row: %w", err)
		}
	}
	return nil
}

// WriteCSV writes rows as CSV with a header row.
func WriteCSV(w io.Writer, rows []ExportRow) error {
	cw := csv.NewWriter(w)
	header := []string{
		"host", "catalog_source_url", "verification_status", "urn", "urn_publisher",
		"display_name", "media_type", "ref_url", "description", "version", "source",
		"tags", "representative_queries",
	}
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("export: writing csv header: %w", err)
	}
	for _, r := range rows {
		record := []string{
			r.Host, r.CatalogSourceURL, r.VerificationStatus, r.URN, r.URNPublisher,
			r.DisplayName, r.MediaType, r.RefURL, r.Description, r.Version, r.Source,
			r.Tags, r.RepresentativeQueries,
		}
		if err := cw.Write(record); err != nil {
			return fmt.Errorf("export: writing csv row: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}
