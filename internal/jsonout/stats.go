package jsonout

import (
	"fmt"

	"github.com/helgesverre/ardvark/internal/store"
)

// StatsReport is the full dataset summary produced by `ardvark stats`.
type StatsReport struct {
	Domains  StatsDomains  `json:"domains"`
	Catalogs StatsCatalogs `json:"catalogs"`
	Entries  StatsEntries  `json:"entries"`
}

// StatsDomains summarizes the domains table, broken down by ard_status.
type StatsDomains struct {
	Total       int64      `json:"total"`
	ByARDStatus []KeyCount `json:"by_ard_status"`
}

// StatsCatalogs summarizes the catalogs table, broken down by verdict.
type StatsCatalogs struct {
	Total     int64      `json:"total"`
	ByVerdict []KeyCount `json:"by_verdict"`
}

// StatsEntries summarizes the catalog_entries table, broken down by media
// type.
type StatsEntries struct {
	Total       int64      `json:"total"`
	ByMediaType []KeyCount `json:"by_media_type"`
}

// Stats computes the dataset summary: hosts probed, catalogs by verdict,
// entries by media type.
func Stats(st *store.Store) (StatsReport, error) {
	var rep StatsReport

	if err := st.DB.Model(&store.Domain{}).Count(&rep.Domains.Total).Error; err != nil {
		return StatsReport{}, fmt.Errorf("stats: counting domains: %w", err)
	}
	byStatus, err := GroupCount(st, "domains", "ard_status")
	if err != nil {
		return StatsReport{}, err
	}
	rep.Domains.ByARDStatus = byStatus

	if err := st.DB.Model(&store.Catalog{}).Count(&rep.Catalogs.Total).Error; err != nil {
		return StatsReport{}, fmt.Errorf("stats: counting catalogs: %w", err)
	}
	byVerdict, err := GroupCount(st, "catalogs", "verification_status")
	if err != nil {
		return StatsReport{}, err
	}
	rep.Catalogs.ByVerdict = byVerdict

	if err := st.DB.Model(&store.CatalogEntry{}).Count(&rep.Entries.Total).Error; err != nil {
		return StatsReport{}, fmt.Errorf("stats: counting entries: %w", err)
	}
	byMediaType, err := GroupCount(st, "catalog_entries", "media_type")
	if err != nil {
		return StatsReport{}, err
	}
	rep.Entries.ByMediaType = byMediaType

	return rep, nil
}

// GroupCount runs SELECT <col>, COUNT(*) FROM <table> GROUP BY <col>. Both
// table and col are internal constants (never user input), so building the
// query string directly is safe.
func GroupCount(st *store.Store, table, col string) ([]KeyCount, error) {
	// The aliases avoid SQL reserved words: "key" is reserved on
	// MySQL/MariaDB and breaks the query there (SQLite/Postgres accept it).
	type row struct {
		Key   string `gorm:"column:k"`
		Count int64  `gorm:"column:n"`
	}
	var rows []row
	err := st.DB.Table(table).
		Select(col + " AS k, COUNT(*) AS n").
		Group(col).
		Order(col).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("stats: grouping %s.%s: %w", table, col, err)
	}
	out := make([]KeyCount, len(rows))
	for i, r := range rows {
		key := r.Key
		if key == "" {
			key = "(empty)"
		}
		out[i] = KeyCount{Key: key, Count: r.Count}
	}
	return out, nil
}
