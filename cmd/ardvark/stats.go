package main

import (
	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/jsonout"
	"github.com/helgesverre/ardvark/internal/ui"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Summarize the dataset: hosts probed, catalogs by verdict, entries by media type",
	RunE:  runStats,
}

func init() {
	addJSONFlag(statsCmd)
	rootCmd.AddCommand(statsCmd)
}

func runStats(cmd *cobra.Command, args []string) error {
	_, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	report, err := jsonout.Stats(st)
	if err != nil {
		return err
	}

	if jsonOut {
		return printJSON(cmd, report)
	}

	p := printer(cmd)
	printStatsSection(p, "domains", report.Domains.Total, report.Domains.ByARDStatus)
	printStatsSection(p, "catalogs", report.Catalogs.Total, report.Catalogs.ByVerdict)
	printStatsSection(p, "entries", report.Entries.Total, report.Entries.ByMediaType)
	return nil
}

// printStatsSection prints one stats table: a header, the total, and the
// indented per-key breakdown.
func printStatsSection(p *ui.Printer, header string, total int64, groups []jsonout.KeyCount) {
	p.Header(header)
	p.KV("total", total)
	for _, g := range groups {
		p.KV("  "+g.Key, g.Count)
	}
}
