package jsonout

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/store"
)

func newSearchStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedSearchData(t *testing.T, st *store.Store) (first, second store.Catalog) {
	t.Helper()

	alpha, err := st.UpsertDomain("alpha.example", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain alpha: %v", err)
	}
	first = store.Catalog{
		DomainID:           alpha.ID,
		SourceURL:          "https://alpha.example/.well-known/ard.json",
		SpecVersion:        "1.0",
		RawJSON:            `{"specVersion":"1.0"}`,
		ContentHash:        "alpha-old",
		FetchedAt:          time.Now().Add(-time.Hour),
		VerificationStatus: store.VerificationStatusValid,
		Entries: []store.CatalogEntry{
			{
				URN:                   "urn:air:alpha.example:tools:search",
				URNPublisher:          "alpha.example",
				URNNamespace:          "tools",
				URNName:               "search",
				DisplayName:           "Search Service",
				MediaType:             "application/mcp-server-card+json",
				RefURL:                "https://alpha.example/mcp.json",
				Description:           "Search a private knowledge base",
				Tags:                  `["search","database","quoted\"tag"]`,
				RepresentativeQueries: `["find a document"]`,
				RawJSON:               `{"identifier":"urn:air:alpha.example:tools:search"}`,
				Source:                store.EntrySourceCatalog,
			},
			{
				URN:          "urn:air:alpha.example:agents:writer",
				URNPublisher: "alpha.example",
				DisplayName:  "Writer",
				MediaType:    "application/a2a-agent-card+json",
				Description:  "Draft technical text",
				RawJSON:      `{"identifier":"urn:air:alpha.example:agents:writer"}`,
				Source:       store.EntrySourceCatalog,
			},
		},
	}
	if err := st.SaveCatalog(&first,
		[]*store.VerificationCheck{{CheckID: "schema.validation", Severity: store.SeverityError, Passed: true}},
		map[int][]*store.VerificationCheck{0: {{CheckID: "urn.format", Severity: store.SeverityError, Passed: true}}},
	); err != nil {
		t.Fatalf("SaveCatalog first: %v", err)
	}

	beta, err := st.UpsertDomain("beta.example", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain beta: %v", err)
	}
	second = store.Catalog{
		DomainID:           beta.ID,
		SourceURL:          "https://beta.example/.well-known/ard.json",
		SpecVersion:        "1.0",
		RawJSON:            `not valid json`,
		ContentHash:        "beta",
		FetchedAt:          time.Now(),
		VerificationStatus: store.VerificationStatusInvalid,
		Entries: []store.CatalogEntry{{
			URN:                   "urn:air:beta.example:skills:review",
			URNPublisher:          "beta.example",
			DisplayName:           "Review Skill",
			MediaType:             "application/ai-skill+md",
			Description:           "Review source code",
			Tags:                  `["review"]`,
			RepresentativeQueries: `["review this code"]`,
			RawJSON:               `{"identifier":"urn:air:beta.example:skills:review"}`,
			Source:                store.EntrySourceCatalog,
		}},
	}
	if err := st.SaveCatalog(&second, nil, nil); err != nil {
		t.Fatalf("SaveCatalog second: %v", err)
	}
	return first, second
}

func TestSearchTextFiltersAndPagination(t *testing.T) {
	st := newSearchStore(t)
	seedSearchData(t, st)

	res, err := Search(t.Context(), st, SearchOptions{Query: "PRIVATE KNOWLEDGE", Tag: "database"})
	if err != nil {
		t.Fatalf("Search text+tag: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].DisplayName != "Search Service" {
		t.Fatalf("Search text+tag = %+v, want Search Service", res)
	}
	if len(res.Entries[0].Tags) != 3 || res.Entries[0].Tags[1] != "database" {
		t.Errorf("Search tags = %v, want [search database quoted\"tag]", res.Entries[0].Tags)
	}
	quotedTag, err := Search(t.Context(), st, SearchOptions{Tag: `quoted"tag`})
	if err != nil {
		t.Fatalf("Search quoted tag: %v", err)
	}
	if len(quotedTag.Entries) != 1 || quotedTag.Entries[0].DisplayName != "Search Service" {
		t.Fatalf("Search quoted tag = %+v, want Search Service", quotedTag)
	}

	filtered, err := Search(t.Context(), st, SearchOptions{
		Host:      "BETA.EXAMPLE",
		Publisher: "beta.example",
		MediaType: "application/ai-skill+md",
		Verdict:   store.VerificationStatusInvalid,
	})
	if err != nil {
		t.Fatalf("Search filters: %v", err)
	}
	if len(filtered.Entries) != 1 || filtered.Entries[0].URNPublisher != "beta.example" {
		t.Fatalf("Search filters = %+v, want beta entry", filtered)
	}
	literalWildcard, err := Search(t.Context(), st, SearchOptions{Query: "%"})
	if err != nil {
		t.Fatalf("Search literal wildcard: %v", err)
	}
	if len(literalWildcard.Entries) != 0 {
		t.Fatalf("Search literal wildcard = %+v, want no entries", literalWildcard)
	}

	page1, err := Search(t.Context(), st, SearchOptions{Limit: 1})
	if err != nil {
		t.Fatalf("Search page 1: %v", err)
	}
	if len(page1.Entries) != 1 || page1.NextCursor == 0 {
		t.Fatalf("Search page 1 = %+v, want one entry and next cursor", page1)
	}
	page2, err := Search(t.Context(), st, SearchOptions{Limit: 1, Cursor: page1.NextCursor})
	if err != nil {
		t.Fatalf("Search page 2: %v", err)
	}
	if len(page2.Entries) != 1 || page2.Entries[0].EntryID <= page1.Entries[0].EntryID {
		t.Fatalf("Search page 2 = %+v, want later entry", page2)
	}
}

func TestSearchRejectsInvalidOptions(t *testing.T) {
	st := newSearchStore(t)
	for _, opts := range []SearchOptions{
		{Limit: -1},
		{Limit: maxSearchLimit + 1},
		{Verdict: "maybe"},
	} {
		if _, err := Search(t.Context(), st, opts); err == nil {
			t.Errorf("Search(%+v): want error", opts)
		}
	}
}

func TestSearchHonorsContextCancellation(t *testing.T) {
	st := newSearchStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Search(ctx, st, SearchOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search canceled error = %v, want context.Canceled", err)
	}
}

func TestGetCatalogAndEntry(t *testing.T) {
	st := newSearchStore(t)
	first, second := seedSearchData(t, st)

	byID, err := GetCatalog(t.Context(), st, GetCatalogOptions{Selector: stringID(first.ID)})
	if err != nil {
		t.Fatalf("GetCatalog by ID: %v", err)
	}
	if byID.Host != "alpha.example" || len(byID.Entries) != 2 || len(byID.Checks) != 1 {
		t.Fatalf("GetCatalog by ID = %+v", byID)
	}
	if len(byID.Entries[0].Checks) != 1 || byID.Entries[0].Checks[0].CheckID != "urn.format" {
		t.Errorf("entry checks = %+v, want urn.format", byID.Entries[0].Checks)
	}
	page1, err := GetCatalog(t.Context(), st, GetCatalogOptions{Selector: stringID(first.ID), EntryLimit: 1})
	if err != nil {
		t.Fatalf("GetCatalog page 1: %v", err)
	}
	if len(page1.Entries) != 1 || page1.NextEntryCursor == 0 {
		t.Fatalf("GetCatalog page 1 = %+v, want one entry and next cursor", page1)
	}
	page2, err := GetCatalog(t.Context(), st, GetCatalogOptions{
		Selector:    stringID(first.ID),
		EntryLimit:  1,
		EntryCursor: page1.NextEntryCursor,
	})
	if err != nil {
		t.Fatalf("GetCatalog page 2: %v", err)
	}
	if len(page2.Entries) != 1 || page2.Entries[0].EntryID <= page1.Entries[0].EntryID || page2.NextEntryCursor != 0 {
		t.Fatalf("GetCatalog page 2 = %+v, want final later entry", page2)
	}

	byURL, err := GetCatalog(t.Context(), st, GetCatalogOptions{Selector: second.SourceURL})
	if err != nil {
		t.Fatalf("GetCatalog by URL: %v", err)
	}
	if byURL.ID != second.ID || byURL.RawJSON != "not valid json" {
		t.Errorf("GetCatalog by URL = %+v, want second catalog and verbatim raw JSON", byURL)
	}

	entryID := first.Entries[0].ID
	entryByID, err := GetEntry(t.Context(), st, stringID(entryID))
	if err != nil {
		t.Fatalf("GetEntry by ID: %v", err)
	}
	entryByURN, err := GetEntry(t.Context(), st, first.Entries[0].URN)
	if err != nil {
		t.Fatalf("GetEntry by URN: %v", err)
	}
	if entryByID.EntryID != entryID || entryByURN.EntryID != entryID || len(entryByID.Checks) != 1 {
		t.Errorf("GetEntry results: by ID=%+v by URN=%+v", entryByID, entryByURN)
	}
}

func TestGetNotFound(t *testing.T) {
	st := newSearchStore(t)
	if _, err := GetCatalog(t.Context(), st, GetCatalogOptions{Selector: "1", EntryLimit: 201}); err == nil {
		t.Error("GetCatalog invalid entry limit: want error")
	}
	if _, err := GetCatalog(t.Context(), st, GetCatalogOptions{Selector: "999"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetCatalog missing error = %v, want store.ErrNotFound", err)
	}
	if _, err := GetEntry(t.Context(), st, "urn:air:none.example:x:y"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetEntry missing error = %v, want store.ErrNotFound", err)
	}
}

func TestGetExternalSelectorsChooseNewestFetch(t *testing.T) {
	st := newSearchStore(t)
	domain, err := st.UpsertDomain("versions.example", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	sourceURL := "https://versions.example/.well-known/ard.json"
	urn := "urn:air:versions.example:tools:x"
	newer := store.Catalog{
		DomainID: domain.ID, SourceURL: sourceURL, FetchedAt: time.Now(),
		VerificationStatus: store.VerificationStatusValid,
		Entries:            []store.CatalogEntry{{URN: urn, DisplayName: "newer"}},
	}
	older := store.Catalog{
		DomainID: domain.ID, SourceURL: sourceURL, FetchedAt: time.Now().Add(-time.Hour),
		VerificationStatus: store.VerificationStatusInvalid,
		Entries:            []store.CatalogEntry{{URN: urn, DisplayName: "older"}},
	}
	// Insert the newest fetch first so primary-key order disagrees with fetch
	// time. External selectors must use fetch time, not insertion order.
	if err := st.SaveCatalog(&newer, nil, nil); err != nil {
		t.Fatalf("SaveCatalog newer: %v", err)
	}
	if err := st.SaveCatalog(&older, nil, nil); err != nil {
		t.Fatalf("SaveCatalog older: %v", err)
	}

	catalog, err := GetCatalog(t.Context(), st, GetCatalogOptions{Selector: sourceURL})
	if err != nil {
		t.Fatalf("GetCatalog by URL: %v", err)
	}
	if catalog.ID != newer.ID {
		t.Errorf("GetCatalog by URL ID = %d, want newer ID %d", catalog.ID, newer.ID)
	}
	entry, err := GetEntry(t.Context(), st, urn)
	if err != nil {
		t.Fatalf("GetEntry by URN: %v", err)
	}
	if entry.DisplayName != "newer" {
		t.Errorf("GetEntry by URN display name = %q, want newer", entry.DisplayName)
	}
}

func stringID(id uint) string {
	return fmt.Sprint(id)
}
