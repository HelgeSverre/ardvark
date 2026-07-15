package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var (
	exportFormat string
	exportOut    string
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Dump catalog entries, joined with domain and verification status, as JSONL or CSV",
	RunE:  runExport,
}

func init() {
	exportCmd.Flags().StringVar(&exportFormat, "format", "jsonl", "output format: jsonl or csv")
	exportCmd.Flags().StringVar(&exportOut, "out", "", "output file path (default: stdout)")
	rootCmd.AddCommand(exportCmd)
}

// exportRow is one flattened catalog_entries row joined with its owning
// catalog and domain, ready for JSONL/CSV serialization.
type exportRow struct {
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

func runExport(cmd *cobra.Command, args []string) error {
	if exportFormat != "jsonl" && exportFormat != "csv" {
		return fmt.Errorf("export: --format must be \"jsonl\" or \"csv\", got %q", exportFormat)
	}

	_, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	var rows []exportRow
	err = st.DB.Table("catalog_entries").
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
		return fmt.Errorf("export: querying catalog entries: %w", err)
	}

	var w io.Writer = cmd.OutOrStdout()
	if exportOut != "" {
		f, cerr := os.Create(exportOut)
		if cerr != nil {
			return fmt.Errorf("export: creating %s: %w", exportOut, cerr)
		}
		defer f.Close()
		w = f
	}

	switch exportFormat {
	case "jsonl":
		err = writeJSONL(w, rows)
	case "csv":
		err = writeCSV(w, rows)
	}
	if err != nil {
		return err
	}

	if exportOut != "" {
		printer(cmd).Mutedf("exported %d rows to %s", len(rows), exportOut)
	}
	return nil
}

func writeJSONL(w io.Writer, rows []exportRow) error {
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("export: encoding jsonl row: %w", err)
		}
	}
	return nil
}

func writeCSV(w io.Writer, rows []exportRow) error {
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
