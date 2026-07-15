package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/store"
)

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Summarize the dataset: hosts probed, catalogs by verdict, entries by media type",
	RunE:  runStats,
}

func init() {
	rootCmd.AddCommand(statsCmd)
}

func runStats(cmd *cobra.Command, args []string) error {
	_, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	p := printer(cmd)

	var domainCount int64
	if err := st.DB.Model(&store.Domain{}).Count(&domainCount).Error; err != nil {
		return fmt.Errorf("stats: counting domains: %w", err)
	}
	p.Header("domains")
	p.KV("total", domainCount)
	byStatus, err := groupCount(st, "domains", "ard_status")
	if err != nil {
		return err
	}
	for _, g := range byStatus {
		p.KV("  "+g.key, g.count)
	}

	var catalogCount int64
	if err := st.DB.Model(&store.Catalog{}).Count(&catalogCount).Error; err != nil {
		return fmt.Errorf("stats: counting catalogs: %w", err)
	}
	p.Header("catalogs")
	p.KV("total", catalogCount)
	byVerdict, err := groupCount(st, "catalogs", "verification_status")
	if err != nil {
		return err
	}
	for _, g := range byVerdict {
		p.KV("  "+g.key, g.count)
	}

	var entryCount int64
	if err := st.DB.Model(&store.CatalogEntry{}).Count(&entryCount).Error; err != nil {
		return fmt.Errorf("stats: counting entries: %w", err)
	}
	p.Header("entries")
	p.KV("total", entryCount)
	byMediaType, err := groupCount(st, "catalog_entries", "media_type")
	if err != nil {
		return err
	}
	for _, g := range byMediaType {
		p.KV("  "+g.key, g.count)
	}

	return nil
}

type groupRow struct {
	key   string
	count int64
}

// groupCount runs SELECT <col>, COUNT(*) FROM <table> GROUP BY <col>. Both
// table and col are internal constants (never user input), so building the
// query string directly is safe.
func groupCount(st *store.Store, table, col string) ([]groupRow, error) {
	type row struct {
		Key   string
		Count int64
	}
	var rows []row
	err := st.DB.Table(table).
		Select(col + " AS key, COUNT(*) AS count").
		Group(col).
		Order(col).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("stats: grouping %s.%s: %w", table, col, err)
	}
	out := make([]groupRow, len(rows))
	for i, r := range rows {
		key := r.Key
		if key == "" {
			key = "(empty)"
		}
		out[i] = groupRow{key: key, count: r.Count}
	}
	return out, nil
}
