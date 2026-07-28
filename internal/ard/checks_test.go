package ard

import (
	"encoding/json"
	"testing"
)

// duplicateIdentifierCatalog is a catalog whose two entries share one
// identifier but have DIFFERENT defects, so a correct partition is
// observable: entry 0 is fully conformant, while entry 1 is missing the
// schema-required displayName and carries zero representativeQueries.
const duplicateIdentifierCatalog = `{
  "specVersion": "1.0",
  "host": {"displayName": "Example"},
  "entries": [
    {
      "identifier": "urn:air:example.com:agents:dup",
      "displayName": "First",
      "type": "application/mcp-server-card",
      "url": "https://example.com/a",
      "representativeQueries": ["find a thing", "do a thing"]
    },
    {
      "identifier": "urn:air:example.com:agents:dup",
      "type": "application/mcp-server-card",
      "url": "https://example.com/b"
    }
  ]
}`

// Subject alone cannot disambiguate duplicate identifiers, so checks must
// carry the entry index they were produced for. Entry 0 is conformant: if any
// failing check lands on it, the partition misattributed entry 1's defects.
func TestPartitionChecks_DuplicateIdentifiersAttributeDefectsToTheRightEntry(t *testing.T) {
	var cat Catalog
	if err := json.Unmarshal([]byte(duplicateIdentifierCatalog), &cat); err != nil {
		t.Fatalf("unmarshalling fixture: %v", err)
	}

	report := Verify([]byte(duplicateIdentifierCatalog), "example.com")
	_, entryChecks := PartitionChecks(report.Checks, cat.Entries)

	for _, c := range entryChecks[0] {
		if !c.Passed {
			t.Errorf("entry 0 is conformant but got failing check %s: %s", c.CheckID, c.Message)
		}
	}

	var sawSchemaFailure, sawQueriesFailure bool
	for _, c := range entryChecks[1] {
		if c.Passed {
			continue
		}
		switch c.CheckID {
		case "schema.validation":
			sawSchemaFailure = true
		case "queries.count":
			sawQueriesFailure = true
		}
	}
	if !sawSchemaFailure {
		t.Errorf("entry 1 is missing the required displayName; want a failing schema.validation check, got %+v", entryChecks[1])
	}
	if !sawQueriesFailure {
		t.Errorf("entry 1 has zero representativeQueries; want a failing queries.count check, got %+v", entryChecks[1])
	}
}

// Every entry check must be attributed to exactly one entry, and no check may
// be silently dropped or duplicated by the partition.
func TestPartitionChecks_PartitionIsTotalAndDisjoint(t *testing.T) {
	var cat Catalog
	if err := json.Unmarshal([]byte(duplicateIdentifierCatalog), &cat); err != nil {
		t.Fatalf("unmarshalling fixture: %v", err)
	}

	report := Verify([]byte(duplicateIdentifierCatalog), "example.com")
	catalogChecks, entryChecks := PartitionChecks(report.Checks, cat.Entries)

	total := len(catalogChecks)
	for idx, checks := range entryChecks {
		if idx < 0 || idx >= len(cat.Entries) {
			t.Errorf("entry check bucket %d is outside the entry range [0,%d)", idx, len(cat.Entries))
		}
		total += len(checks)
	}
	if total != len(report.Checks) {
		t.Errorf("partitioned %d checks, want all %d", total, len(report.Checks))
	}
}

// The common case must keep working: distinct identifiers route by index
// without relying on identifier lookup at all.
func TestPartitionChecks_DistinctIdentifiers(t *testing.T) {
	const raw = `{
	  "specVersion": "1.0",
	  "host": {"displayName": "Example"},
	  "entries": [
	    {"identifier": "urn:air:example.com:agents:a", "displayName": "A", "type": "application/mcp-server-card", "url": "https://example.com/a", "representativeQueries": ["one", "two"]},
	    {"identifier": "urn:air:example.com:agents:b", "displayName": "B", "type": "application/mcp-server-card", "url": "https://example.com/b", "representativeQueries": ["one", "two"]}
	  ]
	}`

	var cat Catalog
	if err := json.Unmarshal([]byte(raw), &cat); err != nil {
		t.Fatalf("unmarshalling fixture: %v", err)
	}

	report := Verify([]byte(raw), "example.com")
	_, entryChecks := PartitionChecks(report.Checks, cat.Entries)

	if len(entryChecks[0]) == 0 || len(entryChecks[1]) == 0 {
		t.Fatalf("both entries must receive checks, got %d and %d", len(entryChecks[0]), len(entryChecks[1]))
	}
	for idx, checks := range entryChecks {
		want := cat.Entries[idx].Identifier
		for _, c := range checks {
			if c.Subject != want {
				t.Errorf("entry %d got a check for subject %q, want %q", idx, c.Subject, want)
			}
		}
	}
}
