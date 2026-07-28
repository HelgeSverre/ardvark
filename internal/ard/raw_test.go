package ard

import (
	"encoding/json"
	"strings"
	"testing"
)

// An ARD entry is a JSON-LD node: it may carry @context, @id, and terms from
// namespaces ardvark knows nothing about. Decoding into the fixed Go struct
// must not be the point at which that data is destroyed, because the raw
// entry is what gets persisted and exported.
func TestEntry_UnmarshalPreservesUnknownTerms(t *testing.T) {
	const raw = `{
	  "@context": "https://ards.dev/ns/v1",
	  "@id": "https://example.com/agents/a",
	  "identifier": "urn:air:example.com:agents:a",
	  "displayName": "A",
	  "type": "application/mcp-server-card",
	  "url": "https://example.com/a",
	  "acme:serviceTier": {"level": "gold", "regions": ["eu", "us"]}
	}`

	var e Entry
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if e.Identifier != "urn:air:example.com:agents:a" {
		t.Errorf("Identifier = %q, want the parsed known field", e.Identifier)
	}
	if len(e.Raw) == 0 {
		t.Fatal("Entry.Raw is empty; the original entry JSON was not captured")
	}

	var got map[string]any
	if err := json.Unmarshal(e.Raw, &got); err != nil {
		t.Fatalf("decoding Entry.Raw: %v", err)
	}
	for _, term := range []string{"@context", "@id", "acme:serviceTier"} {
		if _, ok := got[term]; !ok {
			t.Errorf("Entry.Raw lost %q; have keys %v", term, keysOf(got))
		}
	}
	tier, ok := got["acme:serviceTier"].(map[string]any)
	if !ok {
		t.Fatalf("acme:serviceTier = %T, want a nested object preserved verbatim", got["acme:serviceTier"])
	}
	if tier["level"] != "gold" {
		t.Errorf("acme:serviceTier.level = %v, want gold", tier["level"])
	}
}

// Entries decoded as part of a catalog must each keep their own raw JSON.
func TestCatalog_UnmarshalPreservesPerEntryRaw(t *testing.T) {
	const raw = `{
	  "specVersion": "1.0",
	  "host": {"displayName": "Example"},
	  "entries": [
	    {"identifier": "urn:air:example.com:agents:a", "displayName": "A", "type": "application/mcp-server-card", "url": "https://example.com/a", "ext:one": 1},
	    {"identifier": "urn:air:example.com:agents:b", "displayName": "B", "type": "application/mcp-server-card", "url": "https://example.com/b", "ext:two": 2}
	  ]
	}`

	var c Catalog
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(c.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(c.Entries))
	}
	if !strings.Contains(string(c.Entries[0].Raw), "ext:one") {
		t.Errorf("entry 0 Raw lost ext:one: %s", c.Entries[0].Raw)
	}
	if !strings.Contains(string(c.Entries[1].Raw), "ext:two") {
		t.Errorf("entry 1 Raw lost ext:two: %s", c.Entries[1].Raw)
	}
	if strings.Contains(string(c.Entries[0].Raw), "ext:two") {
		t.Errorf("entry 0 Raw absorbed entry 1's terms: %s", c.Entries[0].Raw)
	}
}

// Raw is capture-only: it must not leak back out as a property when an entry
// is re-marshaled.
func TestEntry_MarshalDoesNotEmitRaw(t *testing.T) {
	var e Entry
	if err := json.Unmarshal([]byte(`{"identifier":"urn:air:example.com:agents:a","displayName":"A","type":"t"}`), &e); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	out, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "Raw") || strings.Contains(string(out), `"raw"`) {
		t.Errorf("marshaled entry exposes Raw: %s", out)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
