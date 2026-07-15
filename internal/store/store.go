package store

import (
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

	if err := db.AutoMigrate(allModels...); err != nil {
		return nil, fmt.Errorf("store: migrating schema: %w", err)
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
	var c Catalog
	if err := s.DB.Where("domain_id = ? AND source_url = ?", domainID, sourceURL).
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

// SaveRegistry inserts a registry row.
func (s *Store) SaveRegistry(r *Registry) error {
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
