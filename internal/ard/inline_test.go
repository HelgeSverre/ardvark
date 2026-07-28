package ard

import (
	"strings"
	"testing"
)

func TestInlineCatalogs(t *testing.T) {
	const catalogNode = `{"specVersion":"1.0","host":{"displayName":"X"},"entries":[{"identifier":"urn:air:example.com:agents:a","displayName":"A","type":"application/mcp-server-card","url":"https://example.com/a"}]}`

	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"single catalog node", catalogNode, 1},
		{"array containing a catalog", `[{"@type":"Organization","name":"X"}, ` + catalogNode + `]`, 1},
		{"graph containing a catalog", `{"@context":"https://schema.org","@graph":[{"@type":"WebSite"}, ` + catalogNode + `]}`, 1},
		{"two catalogs in one array", `[` + catalogNode + `,` + catalogNode + `]`, 2},

		// Everything below must produce no candidates: an ARD ingest that
		// fires on ordinary web markup would flood the index with junk.
		{"schema.org organization", `{"@context":"https://schema.org","@type":"Organization","name":"Example"}`, 0},
		{"schema.org graph", `{"@graph":[{"@type":"WebSite","url":"https://example.com"},{"@type":"Person","name":"A"}]}`, 0},
		{"entries without specVersion", `{"entries":[{"identifier":"urn:air:example.com:agents:a"}]}`, 0},
		{"specVersion without entries", `{"specVersion":"1.0"}`, 0},
		{"entries not an array", `{"specVersion":"1.0","entries":"nope"}`, 0},
		{"malformed json", `{ not json at all`, 0},
		{"empty", ``, 0},
		{"json null", `null`, 0},
		{"bare string", `"hello"`, 0},
		{"array of scalars", `[1,2,3]`, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InlineCatalogs([]byte(tt.raw))
			if len(got) != tt.want {
				t.Fatalf("InlineCatalogs() returned %d candidates, want %d: %q", len(got), tt.want, got)
			}
		})
	}
}

// A candidate must be returned verbatim so the entry-level JSON-LD and
// extension terms inside it survive into storage.
func TestInlineCatalogs_ReturnsNodeVerbatim(t *testing.T) {
	const raw = `{"@graph":[{"specVersion":"1.0","@id":"https://example.com/cat","entries":[{"identifier":"urn:air:example.com:agents:a","displayName":"A","type":"t","url":"https://example.com/a","acme:tier":"gold"}]}]}`

	got := InlineCatalogs([]byte(raw))
	if len(got) != 1 {
		t.Fatalf("returned %d candidates, want 1", len(got))
	}
	for _, term := range []string{`"@id"`, `acme:tier`} {
		if !strings.Contains(string(got[0]), term) {
			t.Errorf("candidate lost %s: %s", term, got[0])
		}
	}
	if strings.Contains(string(got[0]), "@graph") {
		t.Errorf("candidate should be the inner node, not the wrapper: %s", got[0])
	}
}

// Deeply nested or self-referential structures must not cause unbounded
// recursion.
func TestInlineCatalogs_BoundsNesting(t *testing.T) {
	raw := strings.Repeat(`{"@graph":[`, 200) + `{"specVersion":"1.0","entries":[]}` + strings.Repeat(`]}`, 200)
	// Must return rather than overflow the stack; the exact count is not the
	// point, only that it terminates.
	_ = InlineCatalogs([]byte(raw))
}
