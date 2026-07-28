package ard

import "encoding/json"

// maxInlineNestingDepth bounds how deep InlineCatalogs will descend through
// @graph wrappers and arrays. JSON-LD nests legitimately, but only shallowly;
// a deeper structure is malformed or hostile, and either way is not worth
// unbounded stack.
const maxInlineNestingDepth = 16

// InlineCatalogs extracts ARD catalog documents embedded in one
// <script type="application/ld+json"> block.
//
// A block may hold a single node, an array of nodes, or a node wrapping an
// @graph array, and those forms nest. Each candidate is returned verbatim so
// its @context, @id, and namespaced extension terms survive into storage.
//
// Recognition is deliberately narrow: a node counts only when it has both a
// specVersion string and an entries array — the shape of an ARD catalog
// document. Most JSON-LD on the web is Schema.org markup (Organization,
// WebSite, BreadcrumbList), and treating "is JSON-LD" as "is ARD" would fill
// the index with pages that never published a catalog at all.
//
// Malformed JSON yields no candidates rather than an error: a broken script
// block on an otherwise fine page is not a crawl failure.
func InlineCatalogs(raw []byte) []json.RawMessage {
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil
	}
	var out []json.RawMessage
	collectInlineCatalogs(node, 0, &out)
	return out
}

// collectInlineCatalogs walks a decoded JSON-LD value, appending every node
// that looks like an ARD catalog document.
func collectInlineCatalogs(node any, depth int, out *[]json.RawMessage) {
	if depth > maxInlineNestingDepth {
		return
	}

	switch v := node.(type) {
	case []any:
		for _, item := range v {
			collectInlineCatalogs(item, depth+1, out)
		}
	case map[string]any:
		if isCatalogDocument(v) {
			// Re-marshal rather than slicing the original bytes: the decoded
			// map is the only handle we have on this node's extent, and JSON
			// object key order is not semantically meaningful.
			if encoded, err := json.Marshal(v); err == nil {
				*out = append(*out, encoded)
			}
			return
		}
		if graph, ok := v["@graph"]; ok {
			collectInlineCatalogs(graph, depth+1, out)
		}
	}
}

// isCatalogDocument reports whether a decoded JSON-LD node has the shape of
// an ARD catalog document: a specVersion string alongside an entries array.
func isCatalogDocument(node map[string]any) bool {
	if _, ok := node["specVersion"].(string); !ok {
		return false
	}
	_, ok := node["entries"].([]any)
	return ok
}
