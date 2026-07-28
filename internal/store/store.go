package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/glebarez/sqlite"
)

// ErrNotFound is the store's driver-agnostic not-found sentinel. Lookup
// methods translate gorm.ErrRecordNotFound into an error wrapping
// ErrNotFound at the store boundary, so callers never need a gorm import
// just to distinguish "missing" from a real failure.
var ErrNotFound = errors.New("store: not found")

// Store wraps a *gorm.DB with focused, crawler-facing methods over the nine
// ardvark tables.
type Store struct {
	DB *gorm.DB
}

// allModels lists every table AutoMigrate must create/update, in an order
// safe for foreign key creation.
var allModels = []any{
	&CrawlRun{},
	&FrontierItem{},
	&Domain{},
	&Probe{},
	&Catalog{},
	&CatalogEntry{},
	&Artifact{},
	&Registry{},
	&VerificationCheck{},
}

// Open opens a database connection for the given driver ("sqlite", "mysql",
// or "postgres") and DSN, and runs AutoMigrate for all nine ardvark tables.
func Open(driver, dsn string) (*Store, error) {
	var dialector gorm.Dialector
	switch driver {
	case "sqlite", "sqlite3":
		dialector = sqlite.Open(dsn)
	case "mysql":
		dialector = mysql.Open(dsn)
	case "postgres", "postgresql":
		dialector = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("store: unsupported driver %q (want sqlite, mysql, or postgres)", driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("store: opening %s database: %w", driver, err)
	}

	if err := withMigrationLock(db, driver, func() error {
		if err := migrateFrontierDedupKey(db); err != nil {
			return fmt.Errorf("store: reconciling frontier_items before migration: %w", err)
		}
		if err := db.AutoMigrate(allModels...); err != nil {
			return fmt.Errorf("store: migrating schema: %w", err)
		}
		if err := migrateFrontierDedupVersion(db); err != nil {
			return fmt.Errorf("store: reconciling frontier dedup semantics: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if driver == "sqlite" || driver == "sqlite3" {
		// sqlite allows only one writer at a time; serialize connections so
		// concurrent callers get clean transaction retries via the driver's
		// busy handling instead of "database is locked" errors surfacing
		// from parallel connections.
		sqlDB, err := db.DB()
		if err != nil {
			return nil, fmt.Errorf("store: getting underlying sql.DB: %w", err)
		}
		sqlDB.SetMaxOpenConns(1)
	}

	return &Store{DB: db}, nil
}

// Schema-migration mutex identifiers. The key/name are arbitrary but must
// stay stable across versions, since older and newer ardvark processes
// starting against the same database have to contend for the same lock.
const (
	// migrationLockKey is postgres's advisory-lock key: ASCII "ARDV".
	migrationLockKey int64 = 0x41524456
	// migrationLockName is mysql's GET_LOCK name (max 64 chars).
	migrationLockName = "ardvark:schema-migration"
	// migrationLockTimeout bounds the wait so a stuck peer surfaces as a
	// clear error rather than a process that hangs at startup forever.
	migrationLockTimeout = 2 * time.Minute
)

// withMigrationLock runs migrate while holding a database-wide advisory lock,
// so that processes starting at the same time apply schema changes one at a
// time.
//
// This is load-bearing for any worker fleet. Open runs AutoMigrate on every
// open, and AutoMigrate issues a CREATE TABLE/INDEX per model. Those are not
// atomic against a concurrent identical create: on a server engine the loser
// gets a hard error rather than a benign no-op — postgres reports a system
// catalog unique violation (pg_class_relname_nsp_index /
// pg_type_typname_nsp_index), mysql reports "table already exists" (1050).
// Because Open runs before a command has opened its event log, such a worker
// dies at startup with nothing written to its log, which is exceptionally
// hard to diagnose from the outside.
//
// sqlite takes no lock: it serializes writers at the file level, and it has
// no advisory-lock primitive to use here.
func withMigrationLock(db *gorm.DB, driver string, migrate func() error) error {
	switch driver {
	case "postgres", "postgresql", "mysql":
	default:
		return migrate()
	}

	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("store: getting underlying sql.DB: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), migrationLockTimeout)
	defer cancel()

	// Both engines scope these locks to a session, so acquire and release
	// must run on the same pooled connection.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: opening migration-lock connection: %w", err)
	}
	// Returns the connection to the pool; the unlock deferred below runs
	// first, so the lock is always released before that happens.
	defer func() { _ = conn.Close() }()

	if driver == "mysql" {
		var acquired sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)",
			migrationLockName, int(migrationLockTimeout.Seconds())).Scan(&acquired); err != nil {
			return fmt.Errorf("store: acquiring schema migration lock: %w", err)
		}
		if !acquired.Valid || acquired.Int64 != 1 {
			return fmt.Errorf("store: timed out waiting for the schema migration lock")
		}
		// Released on a fresh context: ctx may already be done if migration
		// used its full budget, and the lock must be released regardless.
		defer func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT RELEASE_LOCK(?)", migrationLockName)
		}()
	} else {
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
			return fmt.Errorf("store: acquiring schema migration lock: %w", err)
		}
		defer func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey)
		}()
	}

	return migrate()
}

// migrateFrontierDedupVersion removes work rows whose keys were generated
// under older URL-only semantics. Relationship-aware keys cannot match or
// safely merge those rows. The frontier is derived crawl state: operators
// repopulate it by re-running their seed/crawl inputs after upgrading.
func migrateFrontierDedupVersion(db *gorm.DB) error {
	if err := db.Where("dedup_version IS NULL OR dedup_version <> ?", FrontierDedupVersion).
		Delete(&FrontierItem{}).Error; err != nil {
		return fmt.Errorf("discarding stale frontier rows: %w", err)
	}
	return nil
}

// migrateFrontierDedupKey reconciles a pre-0.4.1 frontier_items table so that
// the subsequent AutoMigrate can succeed, and must run before it.
//
// Versions <=0.4.0 declared FrontierItem.DedupKey as varchar(512) and stored
// the raw "kind:natural" string there. 0.4.1 hashes that key to a fixed-width
// 64-char SHA-256 digest and narrows the column to varchar(64) (see
// FrontierItem.DedupKey and internal/crawler.dedupKey). On mysql/postgres,
// AutoMigrate reconciles that difference by issuing ALTER ... varchar(512) ->
// varchar(64), which HARD-FAILS on any table that already holds a key longer
// than 64 chars — the near-universal case for a populated 0.4.0 frontier,
// since even the shortest "page_fetch:https://..." prefix is 22 chars and any
// real URL pushes the raw key well past 64. Strict sql_mode (the default)
// aborts the ALTER with "Data truncated for column 'dedup_key'" and store.Open
// returns an error, so ardvark cannot start; non-strict mode would instead
// truncate every long key to 64 chars, collapsing distinct pending URLs onto
// the unique index — the exact silent data loss the 0.4.1 change exists to
// prevent.
//
// The old rows cannot be preserved under the narrower, re-keyed column anyway:
// new enqueues hash their keys, so pre-existing raw-key rows would never match
// a lookup again even if the ALTER somehow kept them. The only correct
// reconciliation is therefore to discard the stale frontier: we drop the table
// and let AutoMigrate recreate it with the 0.4.1 schema. Discarding pending
// frontier work is benign for a crawler — those URLs are re-discovered from
// the seed/domain tables on the next crawl, and status guards prevent
// duplicating anything already recorded — which is precisely why no data
// migration tries to rewrite the keys in place (see dedupKey's doc comment).
//
// sqlite is exempt on two counts: it treats VARCHAR(n) as unlimited TEXT so
// the narrowing never truncates and AutoMigrate never fails, and its single
// in-memory/on-disk frontier has no cross-version upgrade path worth eroding.
// The drop is gated on the column actually being wider than 64, so a fresh
// database (no table yet) and any frontier already on the 0.4.1 schema are
// both left untouched — this runs on every Open and must be a no-op once
// reconciled.
func migrateFrontierDedupKey(db *gorm.DB) error {
	switch db.Dialector.Name() {
	case "mysql", "postgres":
		// Only these dialects enforce varchar length; see doc comment.
	default:
		return nil
	}

	m := db.Migrator()
	// A fresh database has no frontier_items yet: AutoMigrate will create it
	// correctly, so there is nothing to reconcile.
	if !m.HasTable(&FrontierItem{}) || !m.HasColumn(&FrontierItem{}, "DedupKey") {
		return nil
	}

	cols, err := m.ColumnTypes(&FrontierItem{})
	if err != nil {
		return fmt.Errorf("inspecting frontier_items columns: %w", err)
	}
	for _, c := range cols {
		if c.Name() != "dedup_key" {
			continue
		}
		// Length reports the declared varchar width on mysql/postgres. A width
		// > 64 means this is a pre-0.4.1 (raw-key) column that AutoMigrate
		// could not safely narrow in place, so drop the whole table and let
		// AutoMigrate rebuild it. length==64 (already reconciled) or an
		// unreported length leaves the table as-is.
		if length, ok := c.Length(); ok && length > 64 {
			if err := m.DropTable(&FrontierItem{}); err != nil {
				return fmt.Errorf("dropping pre-0.4.1 frontier_items for re-key: %w", err)
			}
		}
		break
	}
	return nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return fmt.Errorf("store: getting underlying sql.DB: %w", err)
	}
	return sqlDB.Close()
}

// -- Crawl run bookkeeping --------------------------------------------------

// CreateRun inserts a new crawl_runs row with StartedAt set to now and
// returns it.
func (s *Store) CreateRun(configSnapshot string) (*CrawlRun, error) {
	run := &CrawlRun{
		StartedAt:      time.Now(),
		ConfigSnapshot: configSnapshot,
	}
	if err := s.DB.Create(run).Error; err != nil {
		return nil, fmt.Errorf("store: creating run: %w", err)
	}
	return run, nil
}

// FinishRun sets FinishedAt and final counters on a crawl run.
func (s *Store) FinishRun(runID uint, pagesFetched, hostsProbed, catalogsFound, catalogsValid, errorCount int) error {
	now := time.Now()
	res := s.DB.Model(&CrawlRun{}).Where("id = ?", runID).Updates(map[string]any{
		"finished_at":    &now,
		"pages_fetched":  pagesFetched,
		"hosts_probed":   hostsProbed,
		"catalogs_found": catalogsFound,
		"catalogs_valid": catalogsValid,
		"errors":         errorCount,
	})
	if res.Error != nil {
		return fmt.Errorf("store: finishing run %d: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("store: finishing run %d: run not found", runID)
	}
	return nil
}

// -- Domains ------------------------------------------------------------

// UpsertDomain inserts a domain row for host if it does not already exist,
// or returns the existing row unchanged when it does (discoverySource is
// only set on first insert, and is ignored for an existing row).
// LastProbedAt/ARDStatus are updated separately by RecordProbe and
// UpdateDomainARDStatus, not by UpsertDomain.
func (s *Store) UpsertDomain(host, discoverySource string) (*Domain, error) {
	var existing Domain
	err := s.DB.Where("host = ?", host).First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("store: looking up domain %q: %w", host, err)
	}

	d := &Domain{
		Host:            host,
		FirstSeenAt:     time.Now(),
		DiscoverySource: discoverySource,
		ARDStatus:       ARDStatusUnprobed,
	}
	if err := s.DB.Create(d).Error; err != nil {
		// Race: another writer inserted the same host between our lookup
		// and our insert. Re-fetch instead of failing.
		var retry Domain
		if lookupErr := s.DB.Where("host = ?", host).First(&retry).Error; lookupErr == nil {
			return &retry, nil
		}
		return nil, fmt.Errorf("store: creating domain %q: %w", host, err)
	}
	return d, nil
}

// DomainByHost returns the domain row for host, or an error wrapping
// ErrNotFound.
func (s *Store) DomainByHost(host string) (*Domain, error) {
	var d Domain
	if err := s.DB.Where("host = ?", host).First(&d).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("store: domain %q: %w", host, ErrNotFound)
		}
		return nil, err
	}
	return &d, nil
}

// DomainByID returns the domain row for id, or an error wrapping
// ErrNotFound.
func (s *Store) DomainByID(id uint) (*Domain, error) {
	var d Domain
	if err := s.DB.First(&d, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("store: domain %d: %w", id, ErrNotFound)
		}
		return nil, err
	}
	return &d, nil
}

// PromoteDomainAdvertised records that a host advertises an ARD document,
// without overwriting a verification verdict already attributed to it.
//
// "Advertised" is strictly weaker information than found_valid/found_invalid:
// it says a pointer exists, not that this host serves a catalog that
// verified. So it fills in only where nothing better is known, which is
// expressed as a single conditional UPDATE — atomic, and free of the
// read-then-write race a check-in-Go version would have.
func (s *Store) PromoteDomainAdvertised(domainID uint) error {
	if err := s.DB.Model(&Domain{}).
		Where("id = ? AND ard_status IN ?", domainID, []string{ARDStatusUnprobed, ARDStatusNotFound}).
		Update("ard_status", ARDStatusAdvertised).Error; err != nil {
		return fmt.Errorf("store: promoting domain %d to advertised: %w", domainID, err)
	}
	return nil
}

// RestoreDomainLastProbedAt resets the freshness marker after an
// indeterminate probe attempt. Probe rows remain as history, but a transient
// failure must not make the host look freshly and conclusively probed.
func (s *Store) RestoreDomainLastProbedAt(domainID uint, previous *time.Time) error {
	if err := s.DB.Model(&Domain{}).Where("id = ?", domainID).
		Update("last_probed_at", previous).Error; err != nil {
		return fmt.Errorf("store: restoring domain %d last_probed_at: %w", domainID, err)
	}
	return nil
}

// RecentlyProbed reports whether host has a domains.last_probed_at within
// window of now.
func (s *Store) RecentlyProbed(host string, window time.Duration) (bool, error) {
	d, err := s.DomainByHost(host)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("store: checking recently probed for %q: %w", host, err)
	}
	if d.LastProbedAt == nil {
		return false, nil
	}
	return time.Since(*d.LastProbedAt) < window, nil
}

// RecordProbe inserts a probe row and updates the parent domain's
// LastProbedAt. ARDStatus is not set here: a probe "hit" only means a
// catalog document was found, not that it verified as valid, so the final
// ard_status is decided by the caller once the full set of probes for the
// host (and, on a hit, the catalog verification verdict) is known — see
// UpdateDomainARDStatus.
func (s *Store) RecordProbe(p *Probe) error {
	if p.ProbedAt.IsZero() {
		p.ProbedAt = time.Now()
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(p).Error; err != nil {
			return fmt.Errorf("store: recording probe: %w", err)
		}

		updates := map[string]any{"last_probed_at": p.ProbedAt}

		if err := tx.Model(&Domain{}).Where("id = ?", p.DomainID).Updates(updates).Error; err != nil {
			return fmt.Errorf("store: updating domain %d after probe: %w", p.DomainID, err)
		}
		return nil
	})
}

// UpdateDomainARDStatus sets domains.ard_status for domainID. Callers use
// this to mark a host not_found once all probe methods have missed, and to
// mark found_valid/found_invalid once a discovered catalog has been
// verified.
func (s *Store) UpdateDomainARDStatus(domainID uint, status string) error {
	if err := s.DB.Model(&Domain{}).Where("id = ?", domainID).Update("ard_status", status).Error; err != nil {
		return fmt.Errorf("store: updating ard_status for domain %d: %w", domainID, err)
	}
	return nil
}

// -- Catalogs -------------------------------------------------------------

// SaveCatalog persists a catalog, its entries, and its verification checks
// in a single transaction. entries and checks are given IDs by GORM on
// insert; checks referencing a catalog entry should have SubjectID left at
// zero and will be backfilled to the corresponding entry's ID by matching
// slice index via checksByEntryIndex, while catalogChecks apply directly to
// the catalog.
func (s *Store) SaveCatalog(cat *Catalog, catalogChecks []*VerificationCheck, entryChecks map[int][]*VerificationCheck) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		// Entries are created explicitly below (after CatalogID is known),
		// so skip GORM's automatic association save on Create.
		if err := tx.Omit("Entries").Create(cat).Error; err != nil {
			return fmt.Errorf("store: creating catalog: %w", err)
		}

		for i := range cat.Entries {
			cat.Entries[i].CatalogID = cat.ID
		}
		if len(cat.Entries) > 0 {
			if err := tx.Create(&cat.Entries).Error; err != nil {
				return fmt.Errorf("store: creating catalog entries: %w", err)
			}
		}

		for _, c := range catalogChecks {
			c.SubjectType = SubjectTypeCatalog
			c.SubjectID = cat.ID
			if c.CheckedAt.IsZero() {
				c.CheckedAt = time.Now()
			}
		}
		if len(catalogChecks) > 0 {
			if err := tx.Create(&catalogChecks).Error; err != nil {
				return fmt.Errorf("store: creating catalog checks: %w", err)
			}
		}

		for idx, checks := range entryChecks {
			if idx < 0 || idx >= len(cat.Entries) {
				return fmt.Errorf("store: entry check index %d out of range (entries=%d)", idx, len(cat.Entries))
			}
			entryID := cat.Entries[idx].ID
			for _, c := range checks {
				c.SubjectType = SubjectTypeEntry
				c.SubjectID = entryID
				if c.CheckedAt.IsZero() {
					c.CheckedAt = time.Now()
				}
			}
			if len(checks) == 0 {
				continue
			}
			if err := tx.Create(&checks).Error; err != nil {
				return fmt.Errorf("store: creating entry checks for entry %d: %w", entryID, err)
			}
		}

		return nil
	})
}

// CatalogByHash returns the most recent catalog row matching contentHash,
// or an error wrapping ErrNotFound if none exists. Used for change
// detection on re-crawls.
func (s *Store) CatalogByHash(contentHash string) (*Catalog, error) {
	var c Catalog
	if err := s.DB.Where("content_hash = ?", contentHash).Order("fetched_at desc").First(&c).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("store: catalog with hash %q: %w", contentHash, ErrNotFound)
		}
		return nil, err
	}
	return &c, nil
}

// LatestCatalogBySource returns the most recently fetched catalog row for
// the given (domainID, sourceURL) pair, or an error wrapping ErrNotFound if
// none exists. Content hash alone does not identify "the same document"
// (two different URLs could coincidentally hash the same, e.g. both empty),
// so change detection keys on source_url/domain first and content_hash
// second.
func (s *Store) LatestCatalogBySource(domainID uint, sourceURL string) (*Catalog, error) {
	return s.LatestCatalogBySourceAndParent(domainID, sourceURL, nil)
}

// LatestCatalogBySourceAndParent returns the most recently fetched catalog
// for one serving domain, final source URL, and parent relationship. The
// parent is part of document identity because the current schema represents
// each nested-catalog edge with its own Catalog row.
func (s *Store) LatestCatalogBySourceAndParent(domainID uint, sourceURL string, parentCatalogID *uint) (*Catalog, error) {
	var c Catalog
	query := s.DB.Where("domain_id = ? AND source_url = ?", domainID, sourceURL)
	if parentCatalogID == nil {
		query = query.Where("parent_catalog_id IS NULL")
	} else {
		query = query.Where("parent_catalog_id = ?", *parentCatalogID)
	}
	if err := query.
		Order("fetched_at desc").First(&c).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("store: catalog for domain %d source %q: %w", domainID, sourceURL, ErrNotFound)
		}
		return nil, err
	}
	return &c, nil
}

// SaveEntries inserts catalog entry rows in one batch, e.g. entries
// harvested from a registry and attributed to an existing catalog. Each
// row's CatalogID (and Source/SourceRegistryID provenance) must already be
// set by the caller.
func (s *Store) SaveEntries(entries []CatalogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	if err := s.DB.Create(&entries).Error; err != nil {
		return fmt.Errorf("store: creating catalog entries: %w", err)
	}
	return nil
}

// -- Artifacts & registries ------------------------------------------------

// SaveArtifact inserts an artifact row.
func (s *Store) SaveArtifact(a *Artifact) error {
	if a.FetchedAt.IsZero() {
		a.FetchedAt = time.Now()
	}
	if err := s.DB.Create(a).Error; err != nil {
		return fmt.Errorf("store: creating artifact: %w", err)
	}
	return nil
}

// SaveRegistry inserts a registry reference, or returns the existing row for
// the same declaring entry/base URL/referral parent. A Registry row models a
// relationship, not a harvest attempt; repeat fan-out must therefore reuse it.
func (s *Store) SaveRegistry(r *Registry) error {
	query := s.DB.Where("entry_id = ? AND base_url = ?", r.EntryID, r.BaseURL)
	if r.ReferralSourceID == nil {
		query = query.Where("referral_source_id IS NULL")
	} else {
		query = query.Where("referral_source_id = ?", *r.ReferralSourceID)
	}

	var existing Registry
	err := query.First(&existing).Error
	if err == nil {
		*r = existing
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("store: looking up registry reference: %w", err)
	}

	if err := s.DB.Create(r).Error; err != nil {
		return fmt.Errorf("store: creating registry: %w", err)
	}
	return nil
}

// UpdateRegistryStatus sets a registries row's harvest_status and
// last_harvested_at after a harvest attempt.
func (s *Store) UpdateRegistryStatus(registryID uint, status string, harvestedAt time.Time) error {
	res := s.DB.Model(&Registry{}).Where("id = ?", registryID).
		Updates(map[string]any{"harvest_status": status, "last_harvested_at": &harvestedAt})
	if res.Error != nil {
		return fmt.Errorf("store: updating registry %d status: %w", registryID, res.Error)
	}
	return nil
}
