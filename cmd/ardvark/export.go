package main

import (
	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/jsonout"
)

var (
	exportFormat string
	exportOut    string
)

// export deliberately has no --json flag: its default output is already
// machine-readable (JSONL/CSV) and stdout is occupied by the data itself.
// The MCP ardvark_export tool returns the typed jsonout.ExportResult
// summary instead.
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

func runExport(cmd *cobra.Command, args []string) error {
	_, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	res, err := jsonout.Export(st, exportFormat, exportOut, cmd.OutOrStdout())
	if err != nil {
		return err
	}

	if exportOut != "" {
		printer(cmd).Mutedf("exported %d rows to %s", res.Rows, exportOut)
	}
	return nil
}
