package jsonout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/helgesverre/ardvark/internal/store"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 200
)

// SearchOptions selects one stable page of indexed catalog entries.
// Cursor is the last entry ID returned by the previous page.
type SearchOptions struct {
	Query     string `json:"query,omitempty" jsonschema:"case-insensitive text to find in URNs, names, descriptions, tags, representative queries, and reference URLs"`
	Host      string `json:"host,omitempty" jsonschema:"exact serving host to match"`
	Publisher string `json:"publisher,omitempty" jsonschema:"exact URN publisher to match"`
	MediaType string `json:"media_type,omitempty" jsonschema:"exact entry media type to match"`
	Tag       string `json:"tag,omitempty" jsonschema:"exact entry tag to match"`
	Verdict   string `json:"verdict,omitempty" jsonschema:"exact catalog verdict: valid, valid_with_warnings, or invalid"`
	Limit     int    `json:"limit,omitempty" jsonschema:"maximum results to return, from 1 to 200; 0 uses 50"`
	Cursor    uint   `json:"cursor,omitempty" jsonschema:"last entry ID from the previous page; 0 starts at the first result"`
}

// EntrySummary is the compact entry shape returned by search.
type EntrySummary struct {
	EntryID               uint     `json:"entry_id"`
	CatalogID             uint     `json:"catalog_id"`
	Host                  string   `json:"host"`
	CatalogSourceURL      string   `json:"catalog_source_url"`
	VerificationStatus    string   `json:"verification_status"`
	URN                   string   `json:"urn"`
	URNPublisher          string   `json:"urn_publisher"`
	DisplayName           string   `json:"display_name"`
	MediaType             string   `json:"media_type"`
	RefURL                string   `json:"ref_url,omitempty"`
	Description           string   `json:"description,omitempty"`
	Version               string   `json:"version,omitempty"`
	Source                string   `json:"source"`
	Tags                  []string `json:"tags"`
	RepresentativeQueries []string `json:"representative_queries"`
}

// SearchResult is one cursor-paginated page of indexed entries. NextCursor
// is absent when there is no later page.
type SearchResult struct {
	Entries    []EntrySummary `json:"entries"`
	NextCursor uint           `json:"next_cursor,omitempty"`
}

// GetCatalogOptions selects one catalog and one stable page of its entries.
type GetCatalogOptions struct {
	Selector    string `json:"selector" jsonschema:"numeric database ID or exact catalog source URL"`
	EntryLimit  int    `json:"entry_limit,omitempty" jsonschema:"maximum catalog entries to return, from 1 to 200; 0 uses 50"`
	EntryCursor uint   `json:"entry_cursor,omitempty" jsonschema:"last entry ID from the previous catalog page; 0 starts at the first entry"`
}

// StoredCheck is one verification check loaded from the database.
type StoredCheck struct {
	ID          uint      `json:"id"`
	SubjectType string    `json:"subject_type"`
	SubjectID   uint      `json:"subject_id"`
	CheckID     string    `json:"check_id"`
	Severity    string    `json:"severity"`
	Passed      bool      `json:"passed"`
	Message     string    `json:"message"`
	SpecRef     string    `json:"spec_ref,omitempty"`
	CheckedAt   time.Time `json:"checked_at"`
}

// EntryDetail is the complete stored representation of one catalog entry,
// with its catalog and serving-host context.
type EntryDetail struct {
	EntrySummary
	URNNamespace      string        `json:"urn_namespace,omitempty"`
	URNName           string        `json:"urn_name,omitempty"`
	HasEmbeddedData   bool          `json:"has_embedded_data"`
	EntryUpdatedAt    string        `json:"entry_updated_at,omitempty"`
	CapabilitiesJSON  string        `json:"capabilities_json,omitempty"`
	TrustManifestJSON string        `json:"trust_manifest_json,omitempty"`
	RawJSON           string        `json:"raw_json"`
	SourceRegistryID  *uint         `json:"source_registry_id,omitempty"`
	Checks            []StoredCheck `json:"checks"`
}

// CatalogDetail is the stored representation of one catalog, its catalog
// checks, and one cursor-paginated page of its entries.
type CatalogDetail struct {
	ID                 uint          `json:"id"`
	DomainID           uint          `json:"domain_id"`
	Host               string        `json:"host"`
	SourceURL          string        `json:"source_url"`
	ParentCatalogID    *uint         `json:"parent_catalog_id,omitempty"`
	SpecVersion        string        `json:"spec_version"`
	HostDisplayName    string        `json:"host_display_name,omitempty"`
	HostIdentifier     string        `json:"host_identifier,omitempty"`
	RawJSON            string        `json:"raw_json"`
	ContentHash        string        `json:"content_hash"`
	FetchedAt          time.Time     `json:"fetched_at"`
	VerificationStatus string        `json:"verification_status"`
	Entries            []EntryDetail `json:"entries"`
	NextEntryCursor    uint          `json:"next_entry_cursor,omitempty"`
	Checks             []StoredCheck `json:"checks"`
}

type searchRow struct {
	EntryID               uint
	CatalogID             uint
	Host                  string
	CatalogSourceURL      string
	VerificationStatus    string
	URN                   string
	URNPublisher          string
	DisplayName           string
	MediaType             string
	RefURL                string
	Description           string
	Version               string
	Source                string
	Tags                  string
	RepresentativeQueries string
}

// Search returns a stable, cursor-paginated page of indexed entries.
func Search(ctx context.Context, st *store.Store, opts SearchOptions) (SearchResult, error) {
	limit, err := searchLimit(opts.Limit)
	if err != nil {
		return SearchResult{}, err
	}
	if err := validateVerdict(opts.Verdict); err != nil {
		return SearchResult{}, err
	}

	q := st.DB.WithContext(ctx).Table("catalog_entries").
		Select(`catalog_entries.id AS entry_id, catalog_entries.catalog_id,
			domains.host, catalogs.source_url AS catalog_source_url,
			catalogs.verification_status, catalog_entries.urn,
			catalog_entries.urn_publisher, catalog_entries.display_name,
			catalog_entries.media_type, catalog_entries.ref_url,
			catalog_entries.description, catalog_entries.version,
			catalog_entries.source, catalog_entries.tags,
			catalog_entries.representative_queries`).
		Joins("JOIN catalogs ON catalogs.id = catalog_entries.catalog_id").
		Joins("JOIN domains ON domains.id = catalogs.domain_id")

	if opts.Cursor > 0 {
		q = q.Where("catalog_entries.id > ?", opts.Cursor)
	}
	if needle := strings.TrimSpace(opts.Query); needle != "" {
		pattern := likeContains(needle)
		q = q.Where(`LOWER(COALESCE(catalog_entries.urn, '')) LIKE ? ESCAPE '!'
			OR LOWER(COALESCE(catalog_entries.display_name, '')) LIKE ? ESCAPE '!'
			OR LOWER(COALESCE(catalog_entries.description, '')) LIKE ? ESCAPE '!'
			OR LOWER(COALESCE(catalog_entries.tags, '')) LIKE ? ESCAPE '!'
			OR LOWER(COALESCE(catalog_entries.representative_queries, '')) LIKE ? ESCAPE '!'
			OR LOWER(COALESCE(catalog_entries.ref_url, '')) LIKE ? ESCAPE '!'`,
			pattern, pattern, pattern, pattern, pattern, pattern)
	}
	if opts.Host != "" {
		q = q.Where("LOWER(domains.host) = ?", strings.ToLower(opts.Host))
	}
	if opts.Publisher != "" {
		q = q.Where("LOWER(catalog_entries.urn_publisher) = ?", strings.ToLower(opts.Publisher))
	}
	if opts.MediaType != "" {
		q = q.Where("LOWER(catalog_entries.media_type) = ?", strings.ToLower(opts.MediaType))
	}
	if opts.Tag != "" {
		encodedTag, _ := json.Marshal(opts.Tag) // encoding a string cannot fail
		q = q.Where("LOWER(COALESCE(catalog_entries.tags, '')) LIKE ? ESCAPE '!'", likeContains(string(encodedTag)))
	}
	if opts.Verdict != "" {
		q = q.Where("catalogs.verification_status = ?", opts.Verdict)
	}

	var rows []searchRow
	if err := q.Order("catalog_entries.id ASC").Limit(limit + 1).Scan(&rows).Error; err != nil {
		return SearchResult{}, fmt.Errorf("search: querying entries: %w", err)
	}

	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	res := SearchResult{Entries: make([]EntrySummary, len(rows))}
	for i := range rows {
		res.Entries[i] = rows[i].summary()
	}
	if more {
		res.NextCursor = res.Entries[len(res.Entries)-1].EntryID
	}
	return res, nil
}

func likeContains(value string) string {
	escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(value))
	return "%" + escaped + "%"
}

// GetCatalog loads one catalog by numeric ID or exact source URL. URL
// selectors return the newest matching stored version.
func GetCatalog(ctx context.Context, st *store.Store, opts GetCatalogOptions) (CatalogDetail, error) {
	selector := strings.TrimSpace(opts.Selector)
	if selector == "" {
		return CatalogDetail{}, fmt.Errorf("get catalog: selector is required")
	}
	entryLimit, err := catalogEntryLimit(opts.EntryLimit)
	if err != nil {
		return CatalogDetail{}, err
	}

	var cat store.Catalog
	q := st.DB.WithContext(ctx).Model(&store.Catalog{})
	if id, ok := numericSelector(selector); ok {
		q = q.Where("catalogs.id = ?", id)
	} else {
		q = q.Where("catalogs.source_url = ?", selector).Order("catalogs.fetched_at DESC, catalogs.id DESC")
	}
	if err := q.First(&cat).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return CatalogDetail{}, fmt.Errorf("get catalog %q: %w", selector, store.ErrNotFound)
		}
		return CatalogDetail{}, fmt.Errorf("get catalog %q: %w", selector, err)
	}

	domain, err := domainByID(ctx, st, cat.DomainID)
	if err != nil {
		return CatalogDetail{}, fmt.Errorf("get catalog %q: loading domain: %w", selector, err)
	}
	checks, err := catalogChecks(ctx, st, cat.ID)
	if err != nil {
		return CatalogDetail{}, fmt.Errorf("get catalog %q: %w", selector, err)
	}

	entryQuery := st.DB.WithContext(ctx).Where("catalog_id = ?", cat.ID)
	if opts.EntryCursor > 0 {
		entryQuery = entryQuery.Where("id > ?", opts.EntryCursor)
	}
	var entryRows []store.CatalogEntry
	if err := entryQuery.Order("id ASC").Limit(entryLimit + 1).Find(&entryRows).Error; err != nil {
		return CatalogDetail{}, fmt.Errorf("get catalog %q: loading entries: %w", selector, err)
	}
	more := len(entryRows) > entryLimit
	if more {
		entryRows = entryRows[:entryLimit]
	}
	entryIDs := make([]uint, len(entryRows))
	for i := range entryRows {
		entryIDs[i] = entryRows[i].ID
	}
	checksByEntry, err := entryChecksByID(ctx, st, entryIDs)
	if err != nil {
		return CatalogDetail{}, fmt.Errorf("get catalog %q: %w", selector, err)
	}
	entries := make([]EntryDetail, len(entryRows))
	for i := range entryRows {
		entries[i] = entryDetail(entryRows[i], cat, *domain, checksByEntry[entryRows[i].ID])
	}
	nextEntryCursor := uint(0)
	if more {
		nextEntryCursor = entryRows[len(entryRows)-1].ID
	}

	return CatalogDetail{
		ID:                 cat.ID,
		DomainID:           cat.DomainID,
		Host:               domain.Host,
		SourceURL:          cat.SourceURL,
		ParentCatalogID:    cat.ParentCatalogID,
		SpecVersion:        cat.SpecVersion,
		HostDisplayName:    cat.HostDisplayName,
		HostIdentifier:     cat.HostIdentifier,
		RawJSON:            string(cat.RawJSON),
		ContentHash:        cat.ContentHash,
		FetchedAt:          cat.FetchedAt,
		VerificationStatus: cat.VerificationStatus,
		Entries:            entries,
		NextEntryCursor:    nextEntryCursor,
		Checks:             checks,
	}, nil
}

// GetEntry loads one catalog entry by numeric ID or exact URN. URN
// selectors return the newest matching stored version.
func GetEntry(ctx context.Context, st *store.Store, selector string) (EntryDetail, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return EntryDetail{}, fmt.Errorf("get entry: selector is required")
	}

	var entry store.CatalogEntry
	q := st.DB.WithContext(ctx).Model(&store.CatalogEntry{})
	if id, ok := numericSelector(selector); ok {
		q = q.Where("catalog_entries.id = ?", id)
	} else {
		q = q.Joins("JOIN catalogs ON catalogs.id = catalog_entries.catalog_id").
			Where("catalog_entries.urn = ?", selector).
			Order("catalogs.fetched_at DESC, catalog_entries.id DESC")
	}
	if err := q.First(&entry).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return EntryDetail{}, fmt.Errorf("get entry %q: %w", selector, store.ErrNotFound)
		}
		return EntryDetail{}, fmt.Errorf("get entry %q: %w", selector, err)
	}

	var cat store.Catalog
	if err := st.DB.WithContext(ctx).First(&cat, entry.CatalogID).Error; err != nil {
		return EntryDetail{}, fmt.Errorf("get entry %q: loading catalog: %w", selector, err)
	}
	domain, err := domainByID(ctx, st, cat.DomainID)
	if err != nil {
		return EntryDetail{}, fmt.Errorf("get entry %q: loading domain: %w", selector, err)
	}
	checks, err := entryChecks(ctx, st, entry.ID)
	if err != nil {
		return EntryDetail{}, fmt.Errorf("get entry %q: %w", selector, err)
	}
	return entryDetail(entry, cat, *domain, checks), nil
}

func searchLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultSearchLimit, nil
	}
	if limit < 1 || limit > maxSearchLimit {
		return 0, fmt.Errorf("search: limit must be from 1 to %d", maxSearchLimit)
	}
	return limit, nil
}

func catalogEntryLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultSearchLimit, nil
	}
	if limit < 1 || limit > maxSearchLimit {
		return 0, fmt.Errorf("get catalog: entry limit must be from 1 to %d", maxSearchLimit)
	}
	return limit, nil
}

func validateVerdict(verdict string) error {
	switch verdict {
	case "", store.VerificationStatusValid, store.VerificationStatusValidWithWarnings, store.VerificationStatusInvalid:
		return nil
	default:
		return fmt.Errorf("search: verdict must be valid, valid_with_warnings, or invalid")
	}
}

func numericSelector(selector string) (uint, bool) {
	id, err := strconv.ParseUint(selector, 10, 64)
	return uint(id), err == nil && id > 0 && uint64(uint(id)) == id
}

func decodeStringList(raw string) []string {
	values := []string{}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &values)
	}
	return values
}

func (r searchRow) summary() EntrySummary {
	return EntrySummary{
		EntryID:               r.EntryID,
		CatalogID:             r.CatalogID,
		Host:                  r.Host,
		CatalogSourceURL:      r.CatalogSourceURL,
		VerificationStatus:    r.VerificationStatus,
		URN:                   r.URN,
		URNPublisher:          r.URNPublisher,
		DisplayName:           r.DisplayName,
		MediaType:             r.MediaType,
		RefURL:                r.RefURL,
		Description:           r.Description,
		Version:               r.Version,
		Source:                r.Source,
		Tags:                  decodeStringList(r.Tags),
		RepresentativeQueries: decodeStringList(r.RepresentativeQueries),
	}
}

func entryDetail(entry store.CatalogEntry, cat store.Catalog, domain store.Domain, checks []StoredCheck) EntryDetail {
	return EntryDetail{
		EntrySummary: EntrySummary{
			EntryID:               entry.ID,
			CatalogID:             entry.CatalogID,
			Host:                  domain.Host,
			CatalogSourceURL:      cat.SourceURL,
			VerificationStatus:    cat.VerificationStatus,
			URN:                   entry.URN,
			URNPublisher:          entry.URNPublisher,
			DisplayName:           entry.DisplayName,
			MediaType:             entry.MediaType,
			RefURL:                entry.RefURL,
			Description:           entry.Description,
			Version:               entry.Version,
			Source:                entry.Source,
			Tags:                  decodeStringList(entry.Tags),
			RepresentativeQueries: decodeStringList(entry.RepresentativeQueries),
		},
		URNNamespace:      entry.URNNamespace,
		URNName:           entry.URNName,
		HasEmbeddedData:   entry.HasEmbeddedData,
		EntryUpdatedAt:    entry.EntryUpdatedAt,
		CapabilitiesJSON:  entry.Capabilities,
		TrustManifestJSON: string(entry.TrustManifest),
		RawJSON:           string(entry.RawJSON),
		SourceRegistryID:  entry.SourceRegistryID,
		Checks:            checks,
	}
}

func catalogChecks(ctx context.Context, st *store.Store, catalogID uint) ([]StoredCheck, error) {
	return loadChecks(st.DB.WithContext(ctx).Model(&store.VerificationCheck{}).
		Where("subject_type = ? AND subject_id = ?", store.SubjectTypeCatalog, catalogID))
}

func entryChecks(ctx context.Context, st *store.Store, entryID uint) ([]StoredCheck, error) {
	return loadChecks(st.DB.WithContext(ctx).Model(&store.VerificationCheck{}).
		Where("subject_type = ? AND subject_id = ?", store.SubjectTypeEntry, entryID))
}

func entryChecksByID(ctx context.Context, st *store.Store, entryIDs []uint) (map[uint][]StoredCheck, error) {
	checksByEntry := make(map[uint][]StoredCheck)
	if len(entryIDs) == 0 {
		return checksByEntry, nil
	}
	checks, err := loadChecks(st.DB.WithContext(ctx).Model(&store.VerificationCheck{}).
		Where("subject_type = ? AND subject_id IN ?", store.SubjectTypeEntry, entryIDs))
	if err != nil {
		return nil, err
	}
	for _, check := range checks {
		checksByEntry[check.SubjectID] = append(checksByEntry[check.SubjectID], check)
	}
	return checksByEntry, nil
}

func domainByID(ctx context.Context, st *store.Store, id uint) (*store.Domain, error) {
	var domain store.Domain
	if err := st.DB.WithContext(ctx).First(&domain, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("store: domain %d: %w", id, store.ErrNotFound)
		}
		return nil, err
	}
	return &domain, nil
}

func loadChecks(q *gorm.DB) ([]StoredCheck, error) {
	var rows []store.VerificationCheck
	if err := q.Order("subject_type ASC, subject_id ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("loading verification checks: %w", err)
	}
	checks := make([]StoredCheck, len(rows))
	for i, row := range rows {
		checks[i] = StoredCheck{
			ID:          row.ID,
			SubjectType: row.SubjectType,
			SubjectID:   row.SubjectID,
			CheckID:     row.CheckID,
			Severity:    row.Severity,
			Passed:      row.Passed,
			Message:     row.Message,
			SpecRef:     row.SpecRef,
			CheckedAt:   row.CheckedAt,
		}
	}
	return checks, nil
}
