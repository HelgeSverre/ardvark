package ard

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/helgesverre/ardvark/internal/mediatype"
)

func checkByID(checks []Check, id, subject string) (Check, bool) {
	for _, c := range checks {
		if c.CheckID == id && (subject == "" || c.Subject == subject) {
			return c, true
		}
	}
	return Check{}, false
}

func TestVerify_MalformedJSON(t *testing.T) {
	report := Verify([]byte(`{not json`), "example.com")

	if report.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %q, want %q", report.Verdict, VerdictInvalid)
	}
	if len(report.Checks) != 1 || report.Checks[0].CheckID != "schema.parse" {
		t.Fatalf("Checks = %+v, want single schema.parse check", report.Checks)
	}
	if report.Checks[0].Severity != SeverityError || report.Checks[0].Passed {
		t.Fatalf("schema.parse check = %+v, want failed error", report.Checks[0])
	}
}

func TestVerify_SchemaValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "missing required specVersion",
			raw:  `{"entries": []}`,
		},
		{
			name: "wrong specVersion enum value",
			raw:  `{"specVersion": "2.0", "entries": []}`,
		},
		{
			name: "entry missing required fields",
			raw:  `{"specVersion": "1.0", "entries": [{"url": "https://example.com/a.json"}]}`,
		},
		{
			name: "entry has both url and data",
			raw: `{"specVersion": "1.0", "entries": [{
				"identifier": "urn:air:example.com:agents:a",
				"displayName": "A",
				"type": "application/a2a-agent-card+json",
				"url": "https://example.com/a.json",
				"data": {}
			}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := Verify([]byte(tt.raw), "example.com")
			if _, ok := checkByID(report.Checks, "schema.validation", ""); !ok {
				t.Fatalf("expected a schema.validation check, got %+v", report.Checks)
			}
			if report.Verdict != VerdictInvalid {
				t.Fatalf("Verdict = %q, want %q", report.Verdict, VerdictInvalid)
			}
		})
	}
}

func TestVerify_SchemaValidationPasses(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:air:example.com:agents:a",
			"displayName": "A",
			"type": "application/a2a-agent-card+json",
			"url": "https://example.com/a.json",
			"representativeQueries": ["one", "two"]
		}]
	}`
	report := Verify([]byte(raw), "example.com")
	if _, ok := checkByID(report.Checks, "schema.validation", ""); ok {
		t.Fatalf("did not expect schema.validation failures, got %+v", report.Checks)
	}
}

func TestCheckSpecVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{"correct version", "1.0", true},
		{"wrong version", "2.0", false},
		{"empty version", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := checkSpecVersion(Catalog{SpecVersion: tt.version})
			if c.CheckID != "catalog.spec_version" || c.Severity != SeverityError || c.Subject != "catalog" {
				t.Fatalf("check = %+v, want catalog.spec_version/error/catalog", c)
			}
			if c.Passed != tt.want {
				t.Fatalf("Passed = %v, want %v", c.Passed, tt.want)
			}
		})
	}
}

func TestCheckIdentifierUnique(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want bool
	}{
		{"all unique", []string{"a", "b", "c"}, true},
		{"one duplicate", []string{"a", "b", "a"}, false},
		{"no entries", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entries []Entry
			for _, id := range tt.ids {
				entries = append(entries, Entry{Identifier: id})
			}
			c := checkIdentifierUnique(Catalog{Entries: entries})
			if c.CheckID != "identifier.unique" || c.Severity != SeverityError || c.Subject != "catalog" {
				t.Fatalf("check = %+v, want identifier.unique/error/catalog", c)
			}
			if c.Passed != tt.want {
				t.Fatalf("Passed = %v, want %v", c.Passed, tt.want)
			}
		})
	}
}

func TestCheckValueOrReference(t *testing.T) {
	tests := []struct {
		name string
		url  string
		data []byte
		want bool
	}{
		{"url only", "https://example.com/a.json", nil, true},
		{"data only", "", []byte(`{"a":1}`), true},
		{"neither", "", nil, false},
		{"null data treated as absent, url present", "https://example.com/a.json", []byte(`null`), true},
		{"both present", "https://example.com/a.json", []byte(`{"a":1}`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Entry{URL: tt.url, Data: tt.data}
			c := checkValueOrReference(e, "urn:air:example.com:name")
			if c.CheckID != "entry.value_or_reference" || c.Severity != SeverityError {
				t.Fatalf("check = %+v, want entry.value_or_reference/error", c)
			}
			if c.Passed != tt.want {
				t.Fatalf("Passed = %v, want %v", c.Passed, tt.want)
			}
		})
	}
}

func TestCheckURNFormat(t *testing.T) {
	_, badErr := ParseURN("not-a-urn")
	c := checkURNFormat(badErr, "not-a-urn")
	if c.CheckID != "urn.format" || c.Severity != SeverityError || c.Passed {
		t.Fatalf("check = %+v, want failed urn.format/error", c)
	}

	c = checkURNFormat(nil, "urn:air:example.com:name")
	if !c.Passed {
		t.Fatalf("check = %+v, want passed", c)
	}
}

func TestCheckPublisherMatches(t *testing.T) {
	tests := []struct {
		name          string
		publisher     string
		servingDomain string
		want          bool
	}{
		{"exact match", "example.com", "example.com", true},
		{"case insensitive match", "Example.com", "example.com", true},
		{"mismatch (aggregator)", "example.com", "aggregator.net", false},
		{"apex publisher vs www serving domain", "example.com", "www.example.com", true},
		{"www publisher vs apex serving domain", "www.example.com", "example.com", true},
		{"different subdomains of the same registrable domain", "api.example.com", "www.example.com", true},
		{"different registrable domains, both with subdomains, still warn", "www.example.com", "www.other.com", false},
		{"unresolvable eTLD falls back to exact match: single-label host", "example.com", "localhost", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := URN{Publisher: tt.publisher, Name: "x"}
			c := checkPublisherMatches(u, tt.servingDomain, "urn:air:"+tt.publisher+":x")
			if c.CheckID != "urn.publisher_matches" || c.Severity != SeverityWarning {
				t.Fatalf("check = %+v, want urn.publisher_matches/warning", c)
			}
			if c.Passed != tt.want {
				t.Fatalf("Passed = %v, want %v", c.Passed, tt.want)
			}
		})
	}
}

func TestCheckQueriesCount(t *testing.T) {
	tests := []struct {
		name    string
		queries []string
		want    bool
	}{
		{"zero queries", nil, false},
		{"one query", []string{"a"}, false},
		{"two queries", []string{"a", "b"}, true},
		{"five queries", []string{"a", "b", "c", "d", "e"}, true},
		{"six queries", []string{"a", "b", "c", "d", "e", "f"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := checkQueriesCount(Entry{RepresentativeQueries: tt.queries}, "subject")
			if c.CheckID != "queries.count" || c.Severity != SeverityWarning {
				t.Fatalf("check = %+v, want queries.count/warning", c)
			}
			if c.Passed != tt.want {
				t.Fatalf("Passed = %v, want %v", c.Passed, tt.want)
			}
		})
	}
}

func TestCheckMediaType(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		want bool
	}{
		{"a2a agent card", "application/a2a-agent-card+json", true},
		{"mcp server card", "application/mcp-server-card+json", true},
		{"ai catalog", "application/ai-catalog+json", true},
		{"ai registry", "application/ai-registry+json", true},
		{"ai skill", "application/ai-skill", true},
		{"ai skill markdown", "application/ai-skill+md", true},
		{"ai skill json wild form", "application/ai-skill+json", true},
		{"unknown type", "application/x-custom+json", false},
		{"empty type", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := checkMediaType(Entry{Type: tt.typ}, mediatype.Parse(tt.typ), "subject")
			if c.CheckID != "entry.media_type" || c.Severity != SeverityWarning {
				t.Fatalf("check = %+v, want entry.media_type/warning", c)
			}
			if c.Passed != tt.want {
				t.Fatalf("Passed = %v, want %v", c.Passed, tt.want)
			}
		})
	}
}

func TestRollUp(t *testing.T) {
	tests := []struct {
		name   string
		checks []Check
		want   string
	}{
		{"all passed", []Check{{Severity: SeverityError, Passed: true}, {Severity: SeverityWarning, Passed: true}}, VerdictValid},
		{"warning failed only", []Check{{Severity: SeverityError, Passed: true}, {Severity: SeverityWarning, Passed: false}}, VerdictValidWithWarnings},
		{"error failed", []Check{{Severity: SeverityError, Passed: false}, {Severity: SeverityWarning, Passed: true}}, VerdictInvalid},
		{"both failed", []Check{{Severity: SeverityError, Passed: false}, {Severity: SeverityWarning, Passed: false}}, VerdictInvalid},
		{"no checks", nil, VerdictValid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollUp(tt.checks); got != tt.want {
				t.Fatalf("rollUp() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTransportChecks(t *testing.T) {
	tests := []struct {
		name         string
		contentType  string
		body         []byte
		maxBodyBytes int64
		wantPassed   map[string]bool
	}{
		{
			name:         "all pass",
			contentType:  "application/ai-catalog+json",
			body:         []byte(`{"ok":true}`),
			maxBodyBytes: 1000,
			wantPassed:   map[string]bool{"transport.content_type": true, "transport.size": true, "transport.utf8": true},
		},
		{
			name:         "wrong content type",
			contentType:  "text/plain",
			body:         []byte(`{"ok":true}`),
			maxBodyBytes: 1000,
			wantPassed:   map[string]bool{"transport.content_type": false, "transport.size": true, "transport.utf8": true},
		},
		{
			name:         "oversized body",
			contentType:  "application/json",
			body:         []byte(`{"ok":true}`),
			maxBodyBytes: 5,
			wantPassed:   map[string]bool{"transport.content_type": true, "transport.size": false, "transport.utf8": true},
		},
		{
			name:         "invalid utf8",
			contentType:  "application/json",
			body:         []byte{0xff, 0xfe, 0xfd},
			maxBodyBytes: 1000,
			wantPassed:   map[string]bool{"transport.content_type": true, "transport.size": true, "transport.utf8": false},
		},
		{
			name:         "non-positive maxBodyBytes disables the size check",
			contentType:  "application/json",
			body:         []byte(`{"ok":true}`),
			maxBodyBytes: 0,
			wantPassed:   map[string]bool{"transport.content_type": true, "transport.size": true, "transport.utf8": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := TransportChecks(tt.contentType, tt.body, tt.maxBodyBytes)
			if len(checks) != 3 {
				t.Fatalf("expected 3 transport checks, got %d", len(checks))
			}
			for _, c := range checks {
				if c.Severity != SeverityWarning {
					t.Errorf("check %s: expected warning severity, got %q", c.CheckID, c.Severity)
				}
				if want, ok := tt.wantPassed[c.CheckID]; ok && c.Passed != want {
					t.Errorf("check %s: passed = %v, want %v", c.CheckID, c.Passed, want)
				}
			}
		})
	}
}

func TestReport_MergeChecks(t *testing.T) {
	report := Report{
		Checks:  []Check{{Severity: SeverityError, Passed: true}},
		Verdict: VerdictValid,
	}
	merged := report.MergeChecks([]Check{{Severity: SeverityWarning, Passed: false}})
	if len(merged.Checks) != 2 {
		t.Fatalf("expected 2 checks after merge, got %d", len(merged.Checks))
	}
	if merged.Verdict != VerdictValidWithWarnings {
		t.Fatalf("expected verdict to be recomputed to valid_with_warnings, got %q", merged.Verdict)
	}
}

func TestVerify_DuplicateIdentifiers(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [
			{
				"identifier": "urn:air:example.com:agents:a",
				"displayName": "A",
				"type": "application/a2a-agent-card+json",
				"url": "https://example.com/a.json",
				"representativeQueries": ["one", "two"]
			},
			{
				"identifier": "urn:air:example.com:agents:a",
				"displayName": "A dup",
				"type": "application/a2a-agent-card+json",
				"url": "https://example.com/a2.json",
				"representativeQueries": ["one", "two"]
			}
		]
	}`
	report := Verify([]byte(raw), "example.com")
	c, ok := checkByID(report.Checks, "identifier.unique", "catalog")
	if !ok {
		t.Fatalf("expected identifier.unique check, got %+v", report.Checks)
	}
	if c.Passed {
		t.Fatalf("identifier.unique check = %+v, want failed", c)
	}
	if report.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %q, want %q", report.Verdict, VerdictInvalid)
	}
}

// identifier.unique must compare on the normalized parsed URN, not the raw
// string, so case-insensitive-equivalent URNs are caught as duplicates.
func TestVerify_DuplicateIdentifiers_CaseInsensitiveNormalization(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [
			{
				"identifier": "urn:air:Example.com:agents:a",
				"displayName": "A",
				"type": "application/a2a-agent-card+json",
				"url": "https://example.com/a.json",
				"representativeQueries": ["one", "two"]
			},
			{
				"identifier": "URN:AIR:example.COM:agents:a",
				"displayName": "A dup, different casing",
				"type": "application/a2a-agent-card+json",
				"url": "https://example.com/a2.json",
				"representativeQueries": ["one", "two"]
			}
		]
	}`
	report := Verify([]byte(raw), "example.com")
	c, ok := checkByID(report.Checks, "identifier.unique", "catalog")
	if !ok {
		t.Fatalf("expected identifier.unique check, got %+v", report.Checks)
	}
	if c.Passed {
		t.Fatalf("identifier.unique check = %+v, want failed (case-insensitive duplicate)", c)
	}
}

// A publisher domain differing only by case must still be treated as a
// duplicate via checkIdentifierUnique directly (belt-and-suspenders for the
// Verify-level test above).
func TestCheckIdentifierUnique_CaseInsensitiveNormalization(t *testing.T) {
	entries := []Entry{
		{Identifier: "urn:air:Example.com:agents:a"},
		{Identifier: "urn:AIR:example.com:AGENTS:a"}, // different namespace case: NOT a duplicate
		{Identifier: "URN:air:example.COM:agents:a"}, // same as entry 1 modulo case: duplicate
	}
	c := checkIdentifierUnique(Catalog{Entries: entries})
	if c.Passed {
		t.Fatalf("checkIdentifierUnique = %+v, want failed", c)
	}
}

// urn.publisher_matches must compare on registrable domain (eTLD+1), so a
// catalog served from "www.example.com" whose entries declare publisher
// "example.com" (or vice versa) does not warn.
func TestVerify_PublisherMatches_WWWvsApex(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:air:example.com:agents:a",
			"displayName": "A",
			"type": "application/a2a-agent-card+json",
			"url": "https://www.example.com/a.json",
			"representativeQueries": ["one", "two"]
		}]
	}`
	report := Verify([]byte(raw), "www.example.com")
	c, ok := checkByID(report.Checks, "urn.publisher_matches", "urn:air:example.com:agents:a")
	if !ok {
		t.Fatalf("expected urn.publisher_matches check, got %+v", report.Checks)
	}
	if !c.Passed {
		t.Fatalf("urn.publisher_matches = %+v, want passed (www vs apex is the same registrable domain)", c)
	}
	if report.Verdict != VerdictValid {
		t.Fatalf("Verdict = %q, want %q; checks: %+v", report.Verdict, VerdictValid, report.Checks)
	}
}

// With no serving domain (local-file verification), urn.publisher_matches
// has nothing to compare against and must be skipped, not warned, so a
// spec-clean catalog verified from disk still rolls up to "valid".
func TestVerify_PublisherMatches_SkippedWithoutServingDomain(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:air:example.com:agents:a",
			"displayName": "A",
			"type": "application/a2a-agent-card+json",
			"url": "https://www.example.com/a.json",
			"representativeQueries": ["one", "two"]
		}]
	}`
	report := Verify([]byte(raw), "")
	if c, ok := checkByID(report.Checks, "urn.publisher_matches", "urn:air:example.com:agents:a"); ok {
		t.Fatalf("urn.publisher_matches = %+v, want skipped when serving domain is empty", c)
	}
	if report.Verdict != VerdictValid {
		t.Fatalf("Verdict = %q, want %q; checks: %+v", report.Verdict, VerdictValid, report.Checks)
	}
}

// IDN publishers in Unicode (U-label) form must not be marked schema-invalid
// purely because of encoding, and the parsed URN's ASCII-normalized
// publisher must match a serving domain given in either Unicode or punycode
// form.
func TestVerify_IDNPublisherUnicodeForm(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:air:café.example:agents:a",
			"displayName": "A",
			"type": "application/a2a-agent-card+json",
			"url": "https://café.example/a.json",
			"representativeQueries": ["one", "two"]
		}]
	}`

	for _, servingDomain := range []string{"café.example", "xn--caf-dma.example"} {
		t.Run("serving domain "+servingDomain, func(t *testing.T) {
			report := Verify([]byte(raw), servingDomain)

			if c, ok := checkByID(report.Checks, "schema.validation", ""); ok {
				t.Fatalf("did not expect schema.validation failures for a Unicode-form IDN publisher, got %+v", c)
			}
			if c, ok := checkByID(report.Checks, "urn.format", "urn:air:café.example:agents:a"); ok && !c.Passed {
				t.Fatalf("urn.format = %+v, want passed", c)
			}
			if c, ok := checkByID(report.Checks, "urn.publisher_matches", "urn:air:café.example:agents:a"); !ok || !c.Passed {
				t.Fatalf("urn.publisher_matches = %+v, want passed", c)
			}
			if report.Verdict != VerdictValid {
				t.Fatalf("Verdict = %q, want %q; checks: %+v", report.Verdict, VerdictValid, report.Checks)
			}
		})
	}
}

// Once JSON Schema validation has already failed for a catalog, semantic
// checks that duplicate a schema-expressible constraint (spec_version,
// value_or_reference, urn.format) must not also emit a row for the same
// defect, while checks that find genuinely additional problems (duplicate
// identifiers, publisher mismatch, queries count, media type) still run.
func TestVerify_SemanticChecksDoNotDoubleReportSchemaFailures(t *testing.T) {
	raw := `{
		"specVersion": "2.0",
		"entries": [{
			"identifier": "urn:air:example.com:agents:a",
			"displayName": "A",
			"type": "application/x-custom+json",
			"url": "https://example.com/a.json"
		}]
	}`
	report := Verify([]byte(raw), "aggregator.net")

	if report.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %q, want %q", report.Verdict, VerdictInvalid)
	}
	if _, ok := checkByID(report.Checks, "schema.validation", ""); !ok {
		t.Fatalf("expected a schema.validation check for the bad specVersion, got %+v", report.Checks)
	}
	// catalog.spec_version, entry.value_or_reference, and urn.format
	// duplicate what schema.validation already reported; they must be
	// skipped once schema validation has failed.
	if c, ok := checkByID(report.Checks, "catalog.spec_version", ""); ok {
		t.Fatalf("catalog.spec_version should be skipped once schema validation fails, got %+v", c)
	}
	if c, ok := checkByID(report.Checks, "entry.value_or_reference", ""); ok {
		t.Fatalf("entry.value_or_reference should be skipped once schema validation fails, got %+v", c)
	}
	if c, ok := checkByID(report.Checks, "urn.format", ""); ok {
		t.Fatalf("urn.format should be skipped once schema validation fails, got %+v", c)
	}
	// identifier.unique, urn.publisher_matches, and entry.media_type find
	// genuinely additional problems the schema can't express and must still
	// run.
	if _, ok := checkByID(report.Checks, "identifier.unique", "catalog"); !ok {
		t.Fatalf("identifier.unique should still run even after a schema failure, got %+v", report.Checks)
	}
	if c, ok := checkByID(report.Checks, "urn.publisher_matches", "urn:air:example.com:agents:a"); !ok || c.Passed {
		t.Fatalf("urn.publisher_matches should still run and warn, got %+v", c)
	}
	if c, ok := checkByID(report.Checks, "entry.media_type", "urn:air:example.com:agents:a"); !ok || c.Passed {
		t.Fatalf("entry.media_type should still run and warn, got %+v", c)
	}
}

// unlimit.website-style catalogs (the "ai" URN NID and the
// "application/mcp-server+json" media type predating the spec's "-card"
// suffix, both seen in the wild) must still verify valid after the
// identifier.unique/publisher_matches/IDN/short-circuit changes above.
func TestVerify_UnlimitWebsiteStyleCatalogStillValid(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"host": {"displayName": "Unlimit"},
		"entries": [{
			"identifier": "urn:ai:unlimit.website:tool:pagenode-cms",
			"displayName": "PageNode CMS",
			"type": "application/mcp-server+json",
			"url": "https://unlimit.website/mcp/pagenode-cms.json",
			"representativeQueries": ["create a page", "publish a draft"]
		}]
	}`
	report := Verify([]byte(raw), "unlimit.website")
	if report.Verdict != VerdictValid {
		t.Fatalf("Verdict = %q, want %q; checks: %+v", report.Verdict, VerdictValid, report.Checks)
	}
	for _, c := range report.Checks {
		if !c.Passed {
			t.Fatalf("expected all checks to pass, got failing check: %+v", c)
		}
	}

	// Same catalog, served from "www.unlimit.website": publisher_matches
	// must not warn since it's the same registrable domain.
	reportWWW := Verify([]byte(raw), "www.unlimit.website")
	if reportWWW.Verdict != VerdictValid {
		t.Fatalf("Verdict (www serving domain) = %q, want %q; checks: %+v", reportWWW.Verdict, VerdictValid, reportWWW.Checks)
	}
}

func TestVerify_WarningsOnlyIsValidWithWarnings(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:air:aggregator.net:agents:a",
			"displayName": "A",
			"type": "application/x-custom+json",
			"url": "https://example.com/a.json",
			"representativeQueries": ["one", "two"]
		}]
	}`
	report := Verify([]byte(raw), "example.com")

	if report.Verdict != VerdictValidWithWarnings {
		t.Fatalf("Verdict = %q, want %q (checks: %+v)", report.Verdict, VerdictValidWithWarnings, report.Checks)
	}

	if c, ok := checkByID(report.Checks, "urn.publisher_matches", "urn:air:aggregator.net:agents:a"); !ok || c.Passed {
		t.Fatalf("urn.publisher_matches = %+v, want failed warning", c)
	}
	if c, ok := checkByID(report.Checks, "queries.count", "urn:air:aggregator.net:agents:a"); !ok || !c.Passed {
		t.Fatalf("queries.count = %+v, want passed", c)
	}
	if c, ok := checkByID(report.Checks, "entry.media_type", "urn:air:aggregator.net:agents:a"); !ok || c.Passed {
		t.Fatalf("entry.media_type = %+v, want failed warning", c)
	}
}

func TestVerify_GoldenCorpus(t *testing.T) {
	t.Run("enterprise catalog is valid", func(t *testing.T) {
		raw, err := os.ReadFile("testdata/enterprise-catalog.json")
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		report := Verify(raw, "acme.example")
		if report.Verdict != VerdictValid {
			t.Fatalf("Verdict = %q, want %q; checks: %+v", report.Verdict, VerdictValid, report.Checks)
		}
		for _, c := range report.Checks {
			if !c.Passed {
				t.Fatalf("expected all checks to pass, got failing check: %+v", c)
			}
		}
	})

	t.Run("solo dev catalog verifies", func(t *testing.T) {
		raw, err := os.ReadFile("testdata/solo-dev-catalog.json")
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		report := Verify(raw, "jane.dev")
		if report.Verdict != VerdictValid {
			t.Fatalf("Verdict = %q, want %q; checks: %+v", report.Verdict, VerdictValid, report.Checks)
		}
		for _, c := range report.Checks {
			if !c.Passed {
				t.Fatalf("expected all checks to pass, got failing check: %+v", c)
			}
		}
	})
}

func TestVerify_MediaType_KnownFamiliesNoWarning(t *testing.T) {
	raw := []byte(`{"specVersion":"1.0","host":{"displayName":"H"},"entries":[
		{"identifier":"urn:air:example.com:skills:s","displayName":"S","type":"application/agent-skills+md","url":"https://example.com/s.md"},
		{"identifier":"urn:air:example.com:arch:a","displayName":"A","type":"application/ai-skill-archive+gzip","url":"https://example.com/a.tar.gz"},
		{"identifier":"urn:air:example.com:api:o","displayName":"O","type":"application/vnd.oai.openapi","url":"https://example.com/o.yaml"}
	]}`)
	report := Verify(raw, "example.com")
	for _, c := range report.Checks {
		if c.CheckID == "entry.media_type" && !c.Passed {
			t.Errorf("unexpected failed entry.media_type check: %+v", c)
		}
	}
}

func TestVerify_MediaType_UnsuffixedRegistryIsPointer(t *testing.T) {
	raw := []byte(`{"specVersion":"1.0","host":{"displayName":"H"},"entries":[
		{"identifier":"urn:air:example.com:registry:r","displayName":"R","type":"application/ai-registry","url":"https://example.com/api"}
	]}`)
	report := Verify(raw, "example.com")
	// Pointer entries must not be nagged about representativeQueries.
	if _, ok := checkByID(report.Checks, "queries.count", "urn:air:example.com:registry:r"); ok {
		t.Error("queries.count should be skipped for an unsuffixed application/ai-registry pointer")
	}
	c, ok := checkByID(report.Checks, "entry.media_type", "urn:air:example.com:registry:r")
	if !ok {
		t.Fatal("expected entry.media_type check for urn:air:example.com:registry:r")
	}
	if !c.Passed {
		t.Errorf("expected entry.media_type check to pass for unsuffixed application/ai-registry pointer, got %+v", c)
	}
}

func TestVerify_MediaType_MessageIncludesKindAndFormat(t *testing.T) {
	raw := []byte(`{"specVersion":"1.0","host":{"displayName":"H"},"entries":[
		{"identifier":"urn:air:example.com:mcp:m","displayName":"M","type":"application/mcp-server-card+json","url":"https://example.com/m.json"}
	]}`)
	report := Verify(raw, "example.com")
	c, ok := checkByID(report.Checks, "entry.media_type", "urn:air:example.com:mcp:m")
	if !ok {
		t.Fatal("expected entry.media_type check for urn:air:example.com:mcp:m")
	}
	if !c.Passed {
		t.Errorf("expected entry.media_type check to pass, got %+v", c)
	}
	if !strings.Contains(c.Message, "recognized as mcp-server (format: json)") {
		t.Errorf("Message = %q, want substring %q", c.Message, "recognized as mcp-server (format: json)")
	}
}

func TestVerify_MediaType_UnknownStillWarns(t *testing.T) {
	raw := []byte(`{"specVersion":"1.0","host":{"displayName":"H"},"entries":[
		{"identifier":"urn:air:example.com:x:y","displayName":"Y","type":"application/octet-stream","url":"https://example.com/y.bin"}
	]}`)
	report := Verify(raw, "example.com")
	c, ok := checkByID(report.Checks, "entry.media_type", "urn:air:example.com:x:y")
	if !ok || c.Passed {
		t.Errorf("expected failed entry.media_type warning for unknown type, got ok=%v check=%+v", ok, c)
	}
}

// --- strict/lenient verification mode tests ---

// A legacy "urn:ai:" identifier is a deliberate lenient-mode acceptance:
// Verify must validate it against the schema, while VerifyStrict — the raw
// vendored schema, which only accepts the "air" NID — must reject it.
func TestVerify_LenientAcceptsLegacyAiNID_StrictRejects(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:ai:example.com:tool:x",
			"displayName": "X",
			"type": "application/mcp-server-card+json",
			"url": "https://example.com/x.json",
			"representativeQueries": ["one", "two"]
		}]
	}`

	lenient := Verify([]byte(raw), "example.com")
	if _, ok := checkByID(lenient.Checks, "schema.validation", ""); ok {
		t.Fatalf("lenient Verify: expected no schema.validation failures for urn:ai:, got %+v", lenient.Checks)
	}
	if lenient.Verdict != VerdictValid {
		t.Fatalf("lenient Verdict = %q, want %q; checks: %+v", lenient.Verdict, VerdictValid, lenient.Checks)
	}

	strict := VerifyStrict([]byte(raw), "example.com")
	if strict.Verdict != VerdictInvalid {
		t.Fatalf("strict Verdict = %q, want %q; checks: %+v", strict.Verdict, VerdictInvalid, strict.Checks)
	}
	if _, ok := checkByID(strict.Checks, "schema.validation", ""); !ok {
		t.Fatalf("strict VerifyStrict: expected a schema.validation failure for urn:ai:, got %+v", strict.Checks)
	}
}

// An uppercase "URN:AIR:" prefix must validate leniently (the schema pattern
// is case-sensitive; lenient mode lowercases the prefix before validating)
// but fail strict validation against the raw schema.
func TestVerify_LenientAcceptsUppercasePrefix_StrictRejects(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "URN:AIR:example.com:tool:x",
			"displayName": "X",
			"type": "application/mcp-server-card+json",
			"url": "https://example.com/x.json",
			"representativeQueries": ["one", "two"]
		}]
	}`

	lenient := Verify([]byte(raw), "example.com")
	if _, ok := checkByID(lenient.Checks, "schema.validation", ""); ok {
		t.Fatalf("lenient Verify: expected no schema.validation failures for URN:AIR:, got %+v", lenient.Checks)
	}

	strict := VerifyStrict([]byte(raw), "example.com")
	if strict.Verdict != VerdictInvalid {
		t.Fatalf("strict Verdict = %q, want %q; checks: %+v", strict.Verdict, VerdictInvalid, strict.Checks)
	}
}

// A "url" entry with an explicit "data": null is a deliberate lenient-mode
// acceptance (data: null is treated as absent, matching
// checkValueOrReference); strict mode sees both "url" and "data" present and
// rejects the entry against the raw schema's value-or-reference oneOf/not.
func TestVerify_LenientAcceptsExplicitNullData_StrictRejects(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:air:example.com:tool:x",
			"displayName": "X",
			"type": "application/mcp-server-card+json",
			"url": "https://example.com/x.json",
			"data": null,
			"representativeQueries": ["one", "two"]
		}]
	}`

	lenient := Verify([]byte(raw), "example.com")
	if _, ok := checkByID(lenient.Checks, "schema.validation", ""); ok {
		t.Fatalf("lenient Verify: expected no schema.validation failures for url + data:null, got %+v", lenient.Checks)
	}
	if lenient.Verdict != VerdictValid {
		t.Fatalf("lenient Verdict = %q, want %q; checks: %+v", lenient.Verdict, VerdictValid, lenient.Checks)
	}

	strict := VerifyStrict([]byte(raw), "example.com")
	if strict.Verdict != VerdictInvalid {
		t.Fatalf("strict Verdict = %q, want %q; checks: %+v", strict.Verdict, VerdictInvalid, strict.Checks)
	}
}

// A malformed "uri" (host.documentationUrl) and a malformed "date-time"
// (entry.updatedAt) are format-assertion failures: lenient mode downgrades
// them to warnings (valid_with_warnings), strict mode reports them as
// errors (invalid).
func TestVerify_MalformedFormats_LenientWarnsStrictInvalid(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"host": {"displayName": "H", "documentationUrl": "not a uri"},
		"entries": [{
			"identifier": "urn:air:example.com:tool:x",
			"displayName": "X",
			"type": "application/mcp-server-card+json",
			"url": "https://example.com/x.json",
			"updatedAt": "not-a-date-time",
			"representativeQueries": ["one", "two"]
		}]
	}`

	lenient := Verify([]byte(raw), "example.com")
	if lenient.Verdict != VerdictValidWithWarnings {
		t.Fatalf("lenient Verdict = %q, want %q; checks: %+v", lenient.Verdict, VerdictValidWithWarnings, lenient.Checks)
	}
	var sawWarning bool
	for _, c := range lenient.Checks {
		if c.CheckID == "schema.validation" && !c.Passed {
			if c.Severity != SeverityWarning {
				t.Fatalf("lenient schema.validation check = %+v, want warning severity for a format failure", c)
			}
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Fatalf("lenient Verify: expected at least one downgraded schema.validation warning, got %+v", lenient.Checks)
	}

	strict := VerifyStrict([]byte(raw), "example.com")
	if strict.Verdict != VerdictInvalid {
		t.Fatalf("strict Verdict = %q, want %q; checks: %+v", strict.Verdict, VerdictInvalid, strict.Checks)
	}
	for _, c := range strict.Checks {
		if c.CheckID == "schema.validation" && !c.Passed && c.Severity != SeverityError {
			t.Fatalf("strict schema.validation check = %+v, want error severity", c)
		}
	}
}

// representativeQueries with a single item is below the schema's minItems:2:
// lenient mode downgrades this to a warning (valid_with_warnings, via the
// semantic queries.count check, without also emitting a duplicate
// schema-level row), while strict mode reports it as a schema error.
func TestVerify_QueriesCountBelowMinimum_LenientWarnsStrictInvalid(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"entries": [{
			"identifier": "urn:air:example.com:tool:x",
			"displayName": "X",
			"type": "application/mcp-server-card+json",
			"url": "https://example.com/x.json",
			"representativeQueries": ["only one"]
		}]
	}`

	lenient := Verify([]byte(raw), "example.com")
	if lenient.Verdict != VerdictValidWithWarnings {
		t.Fatalf("lenient Verdict = %q, want %q; checks: %+v", lenient.Verdict, VerdictValidWithWarnings, lenient.Checks)
	}
	for _, c := range lenient.Checks {
		if c.CheckID == "schema.validation" && strings.Contains(c.Message, "representativeQueries") {
			t.Fatalf("lenient Verify: representativeQueries minItems failure should be dropped (queries.count already covers it), got %+v", c)
		}
	}
	if c, ok := checkByID(lenient.Checks, "queries.count", "urn:air:example.com:tool:x"); !ok || c.Passed {
		t.Fatalf("lenient Verify: expected a failed queries.count warning, got %+v", c)
	}

	strict := VerifyStrict([]byte(raw), "example.com")
	if strict.Verdict != VerdictInvalid {
		t.Fatalf("strict Verdict = %q, want %q; checks: %+v", strict.Verdict, VerdictInvalid, strict.Checks)
	}
}

// A fully spec-clean catalog must verify valid under both modes.
func TestVerify_FullyValidCatalog_ValidInBothModes(t *testing.T) {
	raw, err := os.ReadFile("testdata/enterprise-catalog.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	lenient := Verify(raw, "acme.example")
	if lenient.Verdict != VerdictValid {
		t.Fatalf("lenient Verdict = %q, want %q; checks: %+v", lenient.Verdict, VerdictValid, lenient.Checks)
	}

	strict := VerifyStrict(raw, "acme.example")
	if strict.Verdict != VerdictValid {
		t.Fatalf("strict Verdict = %q, want %q; checks: %+v", strict.Verdict, VerdictValid, strict.Checks)
	}
}

// --- schema drift test ---

// jsonFieldNames reflects the json struct tags of t's exported fields,
// returning each field's tag name (the part before any ",omitempty" etc.),
// or its Go field name when untagged.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name := f.Name
		if tag != "" {
			if comma := strings.Index(tag, ","); comma >= 0 {
				tag = tag[:comma]
			}
			if tag == "-" {
				continue
			}
			if tag != "" {
				name = tag
			}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// schemaPropertyNames returns the sorted key set of obj["properties"], for
// obj a JSON Schema object node (unmarshaled into map[string]any).
func schemaPropertyNames(t *testing.T, obj map[string]any) []string {
	t.Helper()
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		t.Fatalf("node has no \"properties\" object: %+v", obj)
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func assertPropertyNamesMatch(t *testing.T, label string, schemaProps []string, goType reflect.Type) {
	t.Helper()
	want := jsonFieldNames(goType)
	if !reflect.DeepEqual(schemaProps, want) {
		t.Fatalf("%s: vendored schema properties %v do not match %s's json struct tags %v — the schema was "+
			"re-vendored with a shape change; update the corresponding Go type", label, schemaProps, goType.Name(), want)
	}
}

// TestSchema_MatchesGoTypes walks the embedded vendored schema's root and
// $defs property sets and asserts they exactly match the json struct tags
// reflected off the corresponding ard Go types. A re-vendored schema whose
// shape has drifted from the Go types turns this test red instead of
// silently producing a mismatch at runtime.
func TestSchema_MatchesGoTypes(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(catalogSchemaJSON, &schema); err != nil {
		t.Fatalf("unmarshal embedded schema: %v", err)
	}

	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no $defs object: %+v", schema)
	}
	def := func(name string) map[string]any {
		d, ok := defs[name].(map[string]any)
		if !ok {
			t.Fatalf("$defs has no %q object", name)
		}
		return d
	}
	nodeProp := func(obj map[string]any, name string) map[string]any {
		props, ok := obj["properties"].(map[string]any)
		if !ok {
			t.Fatalf("node has no \"properties\" object: %+v", obj)
		}
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("properties has no %q object", name)
		}
		return p
	}
	itemsOf := func(arr map[string]any) map[string]any {
		items, ok := arr["items"].(map[string]any)
		if !ok {
			t.Fatalf("array node has no \"items\" object: %+v", arr)
		}
		return items
	}

	assertPropertyNamesMatch(t, "root (Catalog)", schemaPropertyNames(t, schema), reflect.TypeOf(Catalog{}))
	assertPropertyNamesMatch(t, "root.host (HostInfo)", schemaPropertyNames(t, nodeProp(schema, "host")), reflect.TypeOf(HostInfo{}))

	catalogEntry := def("catalogEntry")
	assertPropertyNamesMatch(t, "$defs.catalogEntry (Entry)", schemaPropertyNames(t, catalogEntry), reflect.TypeOf(Entry{}))

	trustManifest := def("trustManifest")
	assertPropertyNamesMatch(t, "$defs.trustManifest (TrustManifest)", schemaPropertyNames(t, trustManifest), reflect.TypeOf(TrustManifest{}))

	trustSchema := def("trustSchema")
	assertPropertyNamesMatch(t, "$defs.trustSchema (TrustSchema)", schemaPropertyNames(t, trustSchema), reflect.TypeOf(TrustSchema{}))

	attestationItems := itemsOf(nodeProp(trustManifest, "attestations"))
	assertPropertyNamesMatch(t, "$defs.trustManifest.attestations items (Attestation)", schemaPropertyNames(t, attestationItems), reflect.TypeOf(Attestation{}))

	provenanceItems := itemsOf(nodeProp(trustManifest, "provenance"))
	assertPropertyNamesMatch(t, "$defs.trustManifest.provenance items (ProvenanceLink)", schemaPropertyNames(t, provenanceItems), reflect.TypeOf(ProvenanceLink{}))
}

// queries.count must not fire for container/pointer entries (nested catalogs
// and registry endpoints), which legitimately have no representativeQueries.
func TestVerify_QueriesCountSkipsPointerEntries(t *testing.T) {
	doc := []byte(`{
		"specVersion": "1.0",
		"host": {"displayName": "T"},
		"entries": [
			{"identifier": "urn:air:example.com:registry:main", "displayName": "R", "type": "application/ai-registry+json", "url": "https://example.com/api"},
			{"identifier": "urn:air:example.com:bundle:x", "displayName": "B", "type": "application/ai-catalog+json", "url": "https://example.com/nested.json"}
		]
	}`)
	report := Verify(doc, "example.com")
	for _, c := range report.Checks {
		if c.CheckID == "queries.count" {
			t.Fatalf("queries.count should be skipped for pointer entries, got check on %q", c.Subject)
		}
	}
	if report.Verdict == VerdictInvalid {
		t.Fatalf("pointer-only catalog should not be invalid, got %s", report.Verdict)
	}
}
