package jsonout

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/store"
)

// VerifyTarget must use ardvark's lenient default verification, accepting a
// legacy "urn:ai:" identifier, while VerifyTargetStrict must validate
// against the exact published spec schema and reject it.
func TestVerifyTarget_LenientVsStrict(t *testing.T) {
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "catalog.json")
	catalog := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:ai:example.com:tool:x",
			"displayName": "X",
			"type": "application/mcp-server-card+json",
			"url": "https://example.com/x.json",
			"representativeQueries": ["one", "two"]
		}]
	}`
	if err := os.WriteFile(catalogPath, []byte(catalog), 0o644); err != nil {
		t.Fatalf("writing catalog: %v", err)
	}

	lenient, err := VerifyTarget(context.Background(), catalogPath)
	if err != nil {
		t.Fatalf("VerifyTarget: %v", err)
	}
	if lenient.Verdict != ard.VerdictValid {
		t.Fatalf("VerifyTarget Verdict = %q, want %q; checks: %+v", lenient.Verdict, ard.VerdictValid, lenient.Checks)
	}

	strict, err := VerifyTargetStrict(context.Background(), catalogPath)
	if err != nil {
		t.Fatalf("VerifyTargetStrict: %v", err)
	}
	if strict.Verdict != ard.VerdictInvalid {
		t.Fatalf("VerifyTargetStrict Verdict = %q, want %q; checks: %+v", strict.Verdict, ard.VerdictInvalid, strict.Checks)
	}
}

// VerifyTarget and VerifyTargetStrict must both return an error, rather than
// panic, for a target that doesn't exist.
func TestVerifyTarget_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")

	if _, err := VerifyTarget(context.Background(), missing); err == nil {
		t.Fatal("VerifyTarget: want error for missing file, got nil")
	}
	if _, err := VerifyTargetStrict(context.Background(), missing); err == nil {
		t.Fatal("VerifyTargetStrict: want error for missing file, got nil")
	}
}

func TestReverifyCatalog_DuplicateIdentifiersKeepPerEntryChecks(t *testing.T) {
	st := newTestStore(t)
	domain, err := st.UpsertDomain("example.com", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}

	const duplicateURN = "urn:air:example.com:agents:duplicate"
	cat := &store.Catalog{
		DomainID:           domain.ID,
		SourceURL:          "https://example.com/.well-known/ai-catalog.json",
		RawJSON:            `{"specVersion":"1.0","entries":[]}`,
		ContentHash:        "duplicate-check-test",
		FetchedAt:          time.Now(),
		VerificationStatus: store.VerificationStatusInvalid,
		Entries: []store.CatalogEntry{
			{URN: duplicateURN, DisplayName: "First", Source: store.EntrySourceCatalog},
			{URN: duplicateURN, DisplayName: "Second", Source: store.EntrySourceCatalog},
		},
	}
	if err := st.SaveCatalog(cat, nil, nil); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}

	report := ard.Report{
		Verdict: ard.VerdictInvalid,
		Checks: []ard.Check{
			{CheckID: "entry.check", Subject: duplicateURN, EntryIndex: 0, Message: "first occurrence"},
			{CheckID: "entry.check", Subject: duplicateURN, EntryIndex: 1, Message: "second occurrence"},
		},
	}
	if err := reverifyCatalog(st, *cat, report); err != nil {
		t.Fatalf("reverifyCatalog: %v", err)
	}

	for i, entry := range cat.Entries {
		var checks []store.VerificationCheck
		if err := st.DB.Where("subject_type = ? AND subject_id = ?", store.SubjectTypeEntry, entry.ID).
			Find(&checks).Error; err != nil {
			t.Fatalf("loading checks for entry %d: %v", i, err)
		}
		if len(checks) != 1 {
			t.Fatalf("entry %d checks = %d, want 1: %+v", i, len(checks), checks)
		}
	}
}
