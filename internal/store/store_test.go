package store

import (
	"errors"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenMigratesAllTables(t *testing.T) {
	s := newTestStore(t)
	for _, m := range allModels {
		if !s.DB.Migrator().HasTable(m) {
			t.Errorf("table for %T not migrated", m)
		}
	}
}

func TestOpenUnsupportedDriver(t *testing.T) {
	_, err := Open("oracle", "dsn")
	if err == nil {
		t.Fatal("expected error for unsupported driver")
	}
}

func TestCreateAndFinishRun(t *testing.T) {
	s := newTestStore(t)

	run, err := s.CreateRun(`{"foo":"bar"}`)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.ID == 0 {
		t.Fatal("expected non-zero run ID")
	}
	if run.FinishedAt != nil {
		t.Fatal("expected FinishedAt to be nil initially")
	}

	if err := s.FinishRun(run.ID, 10, 5, 2, 1, 0); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	var reloaded CrawlRun
	if err := s.DB.First(&reloaded, run.ID).Error; err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if reloaded.FinishedAt == nil {
		t.Fatal("expected FinishedAt to be set")
	}
	if reloaded.PagesFetched != 10 || reloaded.CatalogsValid != 1 {
		t.Fatalf("unexpected counters: %+v", reloaded)
	}
}

func TestFinishRunNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.FinishRun(9999, 0, 0, 0, 0, 0); err == nil {
		t.Fatal("expected error for missing run")
	}
}

func TestUpsertDomainCreatesThenReturnsExisting(t *testing.T) {
	s := newTestStore(t)

	d1, err := s.UpsertDomain("example.com", DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain (create): %v", err)
	}
	if d1.ID == 0 {
		t.Fatal("expected non-zero ID")
	}
	if d1.ARDStatus != ARDStatusUnprobed {
		t.Fatalf("expected unprobed status, got %q", d1.ARDStatus)
	}

	d2, err := s.UpsertDomain("example.com", DiscoverySourceAnchor)
	if err != nil {
		t.Fatalf("UpsertDomain (existing): %v", err)
	}
	if d2.ID != d1.ID {
		t.Fatalf("expected same row, got IDs %d and %d", d1.ID, d2.ID)
	}
	// Discovery source is not overwritten on subsequent upserts.
	if d2.DiscoverySource != DiscoverySourceSeed {
		t.Fatalf("expected original discovery source preserved, got %q", d2.DiscoverySource)
	}

	var count int64
	s.DB.Model(&Domain{}).Where("host = ?", "example.com").Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 domain row, got %d", count)
	}
}

func TestDomainByHostNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.DomainByHost("missing.example.com")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRecentlyProbed(t *testing.T) {
	s := newTestStore(t)

	ok, err := s.RecentlyProbed("never-probed.example.com", time.Hour)
	if err != nil {
		t.Fatalf("RecentlyProbed (unknown host): %v", err)
	}
	if ok {
		t.Fatal("expected false for unknown host")
	}

	d, err := s.UpsertDomain("stale.example.com", DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}

	ok, err = s.RecentlyProbed("stale.example.com", time.Hour)
	if err != nil {
		t.Fatalf("RecentlyProbed (never probed): %v", err)
	}
	if ok {
		t.Fatal("expected false, domain has never been probed")
	}

	if err := s.RecordProbe(&Probe{
		DomainID: d.ID,
		Method:   ProbeMethodWellKnown,
		URL:      "https://stale.example.com/.well-known/ai-catalog.json",
		Outcome:  ProbeOutcomeMiss,
	}); err != nil {
		t.Fatalf("RecordProbe: %v", err)
	}

	ok, err = s.RecentlyProbed("stale.example.com", time.Hour)
	if err != nil {
		t.Fatalf("RecentlyProbed (just probed): %v", err)
	}
	if !ok {
		t.Fatal("expected true, domain was just probed")
	}

	ok, err = s.RecentlyProbed("stale.example.com", -time.Hour)
	if err != nil {
		t.Fatalf("RecentlyProbed (negative window): %v", err)
	}
	if ok {
		t.Fatal("expected false for a negative freshness window")
	}
}

func TestRecordProbeUpdatesLastProbedAtButNotStatus(t *testing.T) {
	s := newTestStore(t)

	d, err := s.UpsertDomain("hit.example.com", DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}

	if err := s.RecordProbe(&Probe{
		DomainID: d.ID,
		Method:   ProbeMethodWellKnown,
		URL:      "https://hit.example.com/.well-known/ai-catalog.json",
		Outcome:  ProbeOutcomeHit,
	}); err != nil {
		t.Fatalf("RecordProbe: %v", err)
	}

	reloaded, err := s.DomainByHost("hit.example.com")
	if err != nil {
		t.Fatalf("DomainByHost: %v", err)
	}
	// A probe "hit" only means a catalog document was found, not that it
	// verified as valid; ard_status is decided separately (see
	// UpdateDomainARDStatus) once verification has run.
	if reloaded.ARDStatus != ARDStatusUnprobed {
		t.Fatalf("expected ard_status to remain unprobed after a bare probe hit, got %q", reloaded.ARDStatus)
	}
	if reloaded.LastProbedAt == nil {
		t.Fatalf("expected last_probed_at to be set")
	}

	var probeCount int64
	s.DB.Model(&Probe{}).Where("domain_id = ?", d.ID).Count(&probeCount)
	if probeCount != 1 {
		t.Fatalf("expected 1 probe row, got %d", probeCount)
	}
}

func TestUpdateDomainARDStatus(t *testing.T) {
	s := newTestStore(t)

	d, err := s.UpsertDomain("status.example.com", DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}

	if err := s.UpdateDomainARDStatus(d.ID, ARDStatusFoundInvalid); err != nil {
		t.Fatalf("UpdateDomainARDStatus: %v", err)
	}

	reloaded, err := s.DomainByHost("status.example.com")
	if err != nil {
		t.Fatalf("DomainByHost: %v", err)
	}
	if reloaded.ARDStatus != ARDStatusFoundInvalid {
		t.Fatalf("expected found_invalid, got %q", reloaded.ARDStatus)
	}
}

func TestSaveCatalogWithEntriesAndChecks(t *testing.T) {
	s := newTestStore(t)

	d, err := s.UpsertDomain("catalog.example.com", DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}

	cat := &Catalog{
		DomainID:           d.ID,
		SourceURL:          "https://catalog.example.com/.well-known/ai-catalog.json",
		SpecVersion:        "1.0",
		HostDisplayName:    "Catalog Example",
		RawJSON:            `{"specVersion":"1.0"}`,
		ContentHash:        "deadbeef",
		FetchedAt:          time.Now(),
		VerificationStatus: VerificationStatusValidWithWarnings,
		Entries: []CatalogEntry{
			{URN: "urn:air:catalog.example.com:agent-a", DisplayName: "Agent A", Source: EntrySourceCatalog},
			{URN: "urn:air:catalog.example.com:agent-b", DisplayName: "Agent B", Source: EntrySourceCatalog},
		},
	}

	catalogChecks := []*VerificationCheck{
		{CheckID: "catalog.spec_version", Severity: SeverityError, Passed: true},
	}
	entryChecks := map[int][]*VerificationCheck{
		0: {{CheckID: "queries.count", Severity: SeverityWarning, Passed: false, Message: "expected 2-5 queries"}},
	}

	if err := s.SaveCatalog(cat, catalogChecks, entryChecks); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}
	if cat.ID == 0 {
		t.Fatal("expected catalog ID to be set")
	}
	for i, e := range cat.Entries {
		if e.ID == 0 {
			t.Fatalf("entry %d: expected ID to be set", i)
		}
		if e.CatalogID != cat.ID {
			t.Fatalf("entry %d: expected catalog_id %d, got %d", i, cat.ID, e.CatalogID)
		}
	}

	var entryCount int64
	s.DB.Model(&CatalogEntry{}).Where("catalog_id = ?", cat.ID).Count(&entryCount)
	if entryCount != 2 {
		t.Fatalf("expected 2 entries, got %d", entryCount)
	}

	var catalogCheckCount int64
	s.DB.Model(&VerificationCheck{}).
		Where("subject_type = ? AND subject_id = ?", SubjectTypeCatalog, cat.ID).
		Count(&catalogCheckCount)
	if catalogCheckCount != 1 {
		t.Fatalf("expected 1 catalog check, got %d", catalogCheckCount)
	}

	var entryCheckCount int64
	s.DB.Model(&VerificationCheck{}).
		Where("subject_type = ? AND subject_id = ?", SubjectTypeEntry, cat.Entries[0].ID).
		Count(&entryCheckCount)
	if entryCheckCount != 1 {
		t.Fatalf("expected 1 entry check, got %d", entryCheckCount)
	}

	found, err := s.CatalogByHash("deadbeef")
	if err != nil {
		t.Fatalf("CatalogByHash: %v", err)
	}
	if found.ID != cat.ID {
		t.Fatalf("expected catalog ID %d, got %d", cat.ID, found.ID)
	}
}

func TestSaveCatalogEntryCheckIndexOutOfRange(t *testing.T) {
	s := newTestStore(t)

	d, err := s.UpsertDomain("badindex.example.com", DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}

	cat := &Catalog{
		DomainID:    d.ID,
		SourceURL:   "https://badindex.example.com/.well-known/ai-catalog.json",
		SpecVersion: "1.0",
		RawJSON:     `{}`,
		ContentHash: "abc123",
		FetchedAt:   time.Now(),
	}
	entryChecks := map[int][]*VerificationCheck{
		0: {{CheckID: "x", Severity: SeverityError}},
	}

	if err := s.SaveCatalog(cat, nil, entryChecks); err == nil {
		t.Fatal("expected error for out-of-range entry check index")
	}

	var count int64
	s.DB.Model(&Catalog{}).Where("id = ?", cat.ID).Count(&count)
	if count != 0 {
		t.Fatal("expected transaction to roll back and not persist the catalog")
	}
}

func TestCatalogByHashNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.CatalogByHash("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLatestCatalogBySourceNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.LatestCatalogBySource(999, "https://nowhere.example/catalog.json")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSaveEntries(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveEntries(nil); err != nil {
		t.Fatalf("SaveEntries (empty): %v", err)
	}

	entries := []CatalogEntry{
		{CatalogID: 1, URN: "urn:air:reg.example:agents:a", Source: EntrySourceRegistry},
		{CatalogID: 1, URN: "urn:air:reg.example:agents:b", Source: EntrySourceRegistry},
	}
	if err := s.SaveEntries(entries); err != nil {
		t.Fatalf("SaveEntries: %v", err)
	}

	var count int64
	s.DB.Model(&CatalogEntry{}).Where("catalog_id = ?", 1).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 entry rows, got %d", count)
	}
}

func TestUpdateRegistryStatus(t *testing.T) {
	s := newTestStore(t)

	r := &Registry{EntryID: 1, BaseURL: "https://registry.example.com", HarvestStatus: HarvestStatusPending}
	if err := s.SaveRegistry(r); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}

	harvestedAt := time.Now()
	if err := s.UpdateRegistryStatus(r.ID, HarvestStatusOK, harvestedAt); err != nil {
		t.Fatalf("UpdateRegistryStatus: %v", err)
	}

	var reloaded Registry
	if err := s.DB.First(&reloaded, r.ID).Error; err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if reloaded.HarvestStatus != HarvestStatusOK {
		t.Fatalf("expected harvest_status ok, got %q", reloaded.HarvestStatus)
	}
	if reloaded.LastHarvestedAt == nil {
		t.Fatal("expected last_harvested_at to be set")
	}
}

func TestSaveArtifact(t *testing.T) {
	s := newTestStore(t)

	a := &Artifact{
		EntryID:     1,
		URL:         "https://example.com/agent-card.json",
		HTTPStatus:  200,
		ContentType: "application/json",
		RawBody:     `{"name":"agent"}`,
		ContentHash: "cafebabe",
		FetchStatus: FetchStatusOK,
	}
	if err := s.SaveArtifact(a); err != nil {
		t.Fatalf("SaveArtifact: %v", err)
	}
	if a.ID == 0 {
		t.Fatal("expected artifact ID to be set")
	}
	if a.FetchedAt.IsZero() {
		t.Fatal("expected FetchedAt to be defaulted")
	}
}

func TestSaveRegistry(t *testing.T) {
	s := newTestStore(t)

	r := &Registry{
		EntryID:       1,
		BaseURL:       "https://registry.example.com",
		HarvestStatus: HarvestStatusPending,
	}
	if err := s.SaveRegistry(r); err != nil {
		t.Fatalf("SaveRegistry: %v", err)
	}
	if r.ID == 0 {
		t.Fatal("expected registry ID to be set")
	}
}
