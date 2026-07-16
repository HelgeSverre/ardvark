// Command fixture is the synthetic web server for the distributed-crawling
// smoke test (tools/smoketest). One container serves ALL ~20 synthetic hosts
// (site1.test .. site20.test) over TLS, dispatching on the request Host header.
// It is attached to the compose network with every hostname as a network alias,
// so the ardvark worker containers resolve site1.test..site20.test to this one
// server.
//
// Per-host content (see routing in handler):
//   - /.well-known/ai-catalog.json — a valid ARD catalog for hosts 1..15;
//     404 for hosts 16..20 so "miss" outcomes are represented.
//   - /robots.txt — allow-all, no Agentmap directive.
//   - /artifacts/*.json — artifact documents referenced by catalog entries.
//   - / and /pageN — HTML link pages, used only on site1.test to exercise the
//     page_fetch fan-out and the maxPagesPerDomain page budget.
//
// Catalog shapes are derived from the repo's own valid testdata
// (internal/ard/testdata/solo-dev-catalog.json): specVersion "1.0", a host
// object, and entries whose URN publisher equals the serving host so
// ard.Verify returns the "valid" verdict (publisher-vs-serving-domain is an
// exact match under the .test TLD, which is not a public suffix). Two catalogs
// carry a FOREIGN-host artifact entry (site1 -> site8, site3 -> site11) whose
// url points at a different host than the catalog's own: this exercises the
// host-affinity sharding fix, where the artifact_fetch item must be owned by
// the worker that owns the artifact URL's host, not the catalog's host.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// responseDelay is an artificial per-response latency (env FIXTURE_DELAY_MS).
// It is applied to content responses (not health/robots) so the whole crawl
// takes long enough that the kill-and-reclaim scenario can reliably SIGKILL a
// worker while it still has items in flight. 0 disables it.
var responseDelay time.Duration

func init() {
	if ms, err := strconv.Atoi(os.Getenv("FIXTURE_DELAY_MS")); err == nil && ms > 0 {
		responseDelay = time.Duration(ms) * time.Millisecond
	}
}

// catalogHosts is the set of hosts (1..15) that serve a valid catalog.
// Hosts 16..20 deliberately 404 their well-known path (misses).
const catalogHostCount = 15

// foreignArtifacts maps a catalog host to an extra entry whose artifact URL is
// served by a DIFFERENT host (chosen to land in a different worker shard):
//
//	site1.test  (shard %10 = 9) -> https://site8.test/artifacts/foreign-agent.json (shard %10 = 0)
//	site3.test  (shard %10 = 3) -> https://site11.test/artifacts/foreign-tool.json (shard %10 = 2)
var foreignArtifacts = map[string]foreignEntry{
	"site1.test": {url: "https://site8.test/artifacts/foreign-agent.json", ns: "agents", name: "foreign-agent", mediaType: "application/a2a-agent-card+json"},
	"site3.test": {url: "https://site11.test/artifacts/foreign-tool.json", ns: "tools", name: "foreign-tool", mediaType: "application/mcp-server-card+json"},
}

type foreignEntry struct {
	url       string
	ns        string
	name      string
	mediaType string
}

// budgetHost is the single host whose HTML index links to more sub-pages than
// the maxPagesPerDomain budget, so the page budget binds and can be re-tested
// on re-crawl.
const budgetHost = "site1.test"

// budgetPageLinks is how many /pageN links the budget host's index advertises.
// The smoke test config sets maxPagesPerDomain=4, so only a subset become
// page_fetch rows.
const budgetPageLinks = 8

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)

	srv := &http.Server{Addr: ":443", Handler: mux}
	log.Printf("fixture: serving TLS on :443 for site1.test..site20.test")
	if err := srv.ListenAndServeTLS("/certs/server.crt", "/certs/server.key"); err != nil {
		log.Fatalf("fixture: %v", err)
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	host := hostOnly(r.Host)
	path := r.URL.Path

	switch {
	case path == "/robots.txt":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "User-agent: *\nAllow: /\n")
		return

	case path == "/healthz":
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
		return

	case path == "/.well-known/ai-catalog.json":
		delay()
		serveCatalog(w, host)
		return

	case strings.HasPrefix(path, "/artifacts/"):
		// Artifact documents are simple JSON payloads; content only needs to be
		// fetchable and 200 for the artifact_fetch handler to record FetchStatusOK.
		delay()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"artifact":true,"host":%q,"path":%q}`, host, path)
		return

	case host == budgetHost && (path == "/" || strings.HasPrefix(path, "/page")):
		delay()
		serveBudgetPage(w, path)
		return

	case path == "/":
		// Non-budget hosts serve a trivial page so a URL seed (if any) still
		// resolves; the smoke test only seeds the budget host as a URL.
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html><body><h1>%s</h1></body></html>", host)
		return

	default:
		http.NotFound(w, r)
	}
}

// serveCatalog writes a valid ARD catalog for hosts 1..15, or 404 for 16..20.
func serveCatalog(w http.ResponseWriter, host string) {
	n, ok := hostIndex(host)
	if !ok || n > catalogHostCount {
		// Hosts 16..20 (and anything unrecognized) have no catalog: a 404 here
		// is what makes the probe record a "miss" rather than a "hit".
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "no catalog\n")
		return
	}

	entries := []map[string]any{
		{
			"identifier":            fmt.Sprintf("urn:air:%s:skills:skill-a", host),
			"displayName":           "Skill A",
			"type":                  "application/ai-skill+json",
			"url":                   fmt.Sprintf("https://%s/artifacts/skill-a.json", host),
			"description":           "A synthetic skill artifact for smoke testing.",
			"representativeQueries": []string{"How do I use skill A?", "What can skill A do?"},
		},
	}

	if fe, has := foreignArtifacts[host]; has {
		entries = append(entries, map[string]any{
			"identifier":            fmt.Sprintf("urn:air:%s:%s:%s", host, fe.ns, fe.name),
			"displayName":           "Foreign " + fe.name,
			"type":                  fe.mediaType,
			"url":                   fe.url,
			"description":           "An entry whose artifact is served by a different host.",
			"representativeQueries": []string{"Call the foreign artifact", "Use the cross-host resource"},
		})
	}

	doc := map[string]any{
		"specVersion": "1.0",
		"host":        map[string]any{"displayName": host + " synthetic host"},
		"entries":     entries,
	}
	w.Header().Set("Content-Type", "application/ai-catalog+json")
	_ = json.NewEncoder(w).Encode(doc)
}

// serveBudgetPage serves the budget host's index (linking to budgetPageLinks
// sub-pages, all same-host) or a leaf sub-page with no further links.
func serveBudgetPage(w http.ResponseWriter, path string) {
	w.Header().Set("Content-Type", "text/html")
	if path == "/" {
		var b strings.Builder
		b.WriteString("<html><body><h1>budget host index</h1><ul>")
		for i := 1; i <= budgetPageLinks; i++ {
			fmt.Fprintf(&b, `<li><a href="/page%d">page %d</a></li>`, i, i)
		}
		b.WriteString("</ul></body></html>")
		fmt.Fprint(w, b.String())
		return
	}
	fmt.Fprintf(w, "<html><body><p>leaf %s</p></body></html>", path)
}

// delay sleeps for the configured artificial response latency, if any.
func delay() {
	if responseDelay > 0 {
		time.Sleep(responseDelay)
	}
}

// hostOnly strips an optional :port suffix from a Host header value.
func hostOnly(h string) string {
	if i := strings.IndexByte(h, ':'); i >= 0 {
		return h[:i]
	}
	return h
}

// hostIndex parses "siteN.test" and returns N.
func hostIndex(host string) (int, bool) {
	if !strings.HasPrefix(host, "site") || !strings.HasSuffix(host, ".test") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(host, "site"), ".test")
	var n int
	if _, err := fmt.Sscanf(mid, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}
