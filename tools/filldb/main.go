// Command filldb fills an ardvark sqlite database with synthetic data for
// performance testing: N domains, one catalog per domain, and M total
// catalog entries spread across them. Not part of the product; not shipped.
package main

import (
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/helgesverre/ardvark/internal/store"
)

func main() {
	dbPath := flag.String("db", "fill.db", "sqlite database path")
	domains := flag.Int("domains", 50_000, "number of domains (and catalogs)")
	entries := flag.Int("entries", 10_000_000, "total catalog entries")
	flag.Parse()

	db, err := gorm.Open(sqlite.Open(*dbPath), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range []string{
		"PRAGMA journal_mode=WAL", "PRAGMA synchronous=OFF",
		"PRAGMA cache_size=-200000", "PRAGMA temp_store=MEMORY",
	} {
		db.Exec(p)
	}
	if err := db.AutoMigrate(
		&store.CrawlRun{}, &store.FrontierItem{}, &store.Domain{}, &store.Probe{},
		&store.Catalog{}, &store.CatalogEntry{}, &store.Artifact{}, &store.Registry{},
		&store.VerificationCheck{},
	); err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Domains + catalogs via multi-row VALUES inserts, ids 1..N aligned.
	const chunk = 500
	for base := 0; base < *domains; base += chunk {
		n := min(chunk, *domains-base)
		var dv, cv strings.Builder
		for i := 0; i < n; i++ {
			id := base + i + 1
			if i > 0 {
				dv.WriteByte(',')
				cv.WriteByte(',')
			}
			fmt.Fprintf(&dv, "(%d,'host-%d.example','%s','seed','found_valid','%s','%s')", id, id, now, now, now)
			fmt.Fprintf(&cv, "(%d,%d,'https://host-%d.example/.well-known/ai-catalog.json','1.0','{}','h%d','%s','valid','%s','%s')",
				id, id, id, id, now, now, now)
		}
		if err := db.Exec("INSERT INTO domains (id,host,first_seen_at,discovery_source,ard_status,created_at,updated_at) VALUES " + dv.String()).Error; err != nil {
			log.Fatal(err)
		}
		if err := db.Exec("INSERT INTO catalogs (id,domain_id,source_url,spec_version,raw_json,content_hash,fetched_at,verification_status,created_at,updated_at) VALUES " + cv.String()).Error; err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("domains+catalogs: %d in %s\n", *domains, time.Since(start).Round(time.Millisecond))

	// Entries: realistic-ish field sizes, spread round-robin over catalogs.
	mediaTypes := []string{
		"application/mcp-server-card+json", "application/a2a-agent-card+json",
		"application/ai-skill+md", "text/markdown", "application/json",
	}
	estart := time.Now()
	const echunk = 400
	var b strings.Builder
	for base := 0; base < *entries; base += echunk {
		n := min(echunk, *entries-base)
		b.Reset()
		for i := 0; i < n; i++ {
			id := base + i + 1
			catID := id%*domains + 1
			mt := mediaTypes[id%len(mediaTypes)]
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b,
				"(%d,%d,'urn:air:host-%d.example:tool:thing-%d','host-%d.example','tool','thing-%d','Thing %d','%s','https://host-%d.example/artifacts/%d.json',0,'A synthetic catalog entry used for export performance testing, number %d.','1.0.0','[\"a\",\"b\"]','[\"what is thing %d\",\"how do I use thing %d\"]','{\"identifier\":\"urn:air:host-%d.example:tool:thing-%d\"}','catalog','%s','%s')",
				id, catID, catID, id, catID, id, id, mt, catID, id, id, id, id, catID, id, now, now)
		}
		if err := db.Exec("INSERT INTO catalog_entries (id,catalog_id,urn,urn_publisher,urn_namespace,urn_name,display_name,media_type,ref_url,has_embedded_data,description,version,tags,representative_queries,raw_json,source,created_at,updated_at) VALUES " + b.String()).Error; err != nil {
			log.Fatal(err)
		}
		if base%1_000_000 < echunk && base > 0 {
			fmt.Printf("  %dM entries, %s elapsed\n", base/1_000_000, time.Since(estart).Round(time.Second))
		}
	}
	fmt.Printf("entries: %d in %s\n", *entries, time.Since(estart).Round(time.Second))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
