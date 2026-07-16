// Command assert runs the SQL-and-log assertions for the distributed-crawling
// smoke test (tools/smoketest). It connects to the shared frontier (MySQL or
// Postgres, per -driver) and reads the per-worker event logs, then verifies one
// scenario's invariants, printing every observed number and a PASS/FAIL per
// check. Exit code is non-zero if any check in the scenario failed, so run.sh
// can gate on it. All SQL it issues is portable across both backends.
//
// Usage:
//
//	assert <a|b|c|d> [-driver mysql|postgres] -dsn <dsn> -logs <dir> [-workers 10]
//
// It is part of the ardvark module purely so it can reuse store models and the
// same gorm drivers; it writes nothing to the database.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/helgesverre/ardvark/internal/store"
)

// hostShardMod mirrors internal/frontier.hostShard composed with the worker
// partition function: fnv32a(host) % store.HostShardCount, then % workers. This
// is the owning worker index for a given fetch-target host. Reimplemented here
// (the frontier helper is unexported) and kept in lockstep with
// store.HostShardCount.
func ownerWorker(host string, workers int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(host))
	return int(h.Sum32()%store.HostShardCount) % workers
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: assert <a|b|c|d> [flags]")
	}
	scenario := os.Args[1]
	fs := flag.NewFlagSet("assert", flag.ExitOnError)
	driver := fs.String("driver", "mysql", "store driver: mysql or postgres")
	dsn := fs.String("dsn", "ardvark:ardvark@tcp(127.0.0.1:13399)/ardvark?charset=utf8mb4&parseTime=True&loc=UTC", "store DSN (mysql or postgres, matching -driver)")
	logsDir := fs.String("logs", "logs", "directory of per-worker JSONL logs")
	workers := fs.Int("workers", 10, "worker count")
	_ = fs.Parse(os.Args[2:])

	st, err := store.Open(*driver, *dsn)
	if err != nil {
		fatal("open %s: %v", *driver, err)
	}
	defer st.Close()

	r := &reporter{}
	switch scenario {
	case "a":
		assertA(r, st, *workers)
	case "b":
		assertB(r, st, *logsDir, *workers)
	case "c":
		assertC(r, st, *workers)
	case "d":
		assertD(r, st, *logsDir, *workers)
	default:
		fatal("unknown scenario %q", scenario)
	}

	fmt.Printf("\n== scenario %s: %d passed, %d failed ==\n", scenario, r.passed, r.failed)
	if r.failed > 0 {
		os.Exit(1)
	}
}

// -- expected fixture facts -------------------------------------------------

const (
	expectedDomains  = 20 // site1..site20 seeded, no extra domains discovered
	expectedCatalogs = 15 // hosts 1..15 serve a catalog; 16..20 are 404 misses
	expectedEntries  = 17 // 13 single-entry catalogs + site1(2) + site3(2)
	expectedArtifact = 17 // one artifact_fetch per entry url
	budgetHost       = "site1.test"
	budgetPageRows   = 4 // maxPagesPerDomain=4: index + 3 children
)

// -- scenario A: full cooperative drain -------------------------------------

func assertA(r *reporter, st *store.Store, workers int) {
	// 1. Frontier fully drained.
	var pending, inFlight int64
	st.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusPending).Count(&pending)
	st.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusInFlight).Count(&inFlight)
	r.check("frontier drained (pending==0 && in_flight==0)", pending == 0 && inFlight == 0,
		"pending=%d in_flight=%d", pending, inFlight)

	// 2. No leftover leases anywhere.
	var leased int64
	st.DB.Model(&store.FrontierItem{}).Where("leased_until IS NOT NULL").Count(&leased)
	r.check("all leases cleared (leased_until IS NULL)", leased == 0, "rows with leased_until set=%d", leased)
	var workerIDSet int64
	st.DB.Model(&store.FrontierItem{}).Where("worker_id <> ''").Count(&workerIDSet)
	r.check("all worker_id cleared", workerIDSet == 0, "rows with worker_id set=%d", workerIDSet)

	// 3. Every seeded host has at least one probe recorded.
	missingProbes := hostsMissingProbes(st)
	r.check("every seeded host probed", len(missingProbes) == 0, "hosts without probes: %v", missingProbes)

	// 4. Domain / catalog / entry counts exact (no duplication).
	var domains, catalogs, entries, artifacts int64
	st.DB.Model(&store.Domain{}).Count(&domains)
	st.DB.Model(&store.Catalog{}).Count(&catalogs)
	st.DB.Model(&store.CatalogEntry{}).Count(&entries)
	st.DB.Model(&store.Artifact{}).Count(&artifacts)
	r.check("domain count", domains == expectedDomains, "domains=%d want=%d", domains, expectedDomains)
	r.check("catalog count (no duplication)", catalogs == expectedCatalogs, "catalogs=%d want=%d", catalogs, expectedCatalogs)
	r.check("catalog_entries count (no duplication)", entries == expectedEntries, "entries=%d want=%d", entries, expectedEntries)
	r.check("artifact count (one per entry url)", artifacts == expectedArtifact, "artifacts=%d want=%d", artifacts, expectedArtifact)

	// 5. No duplicate catalog per (domain_id, source_url).
	dupCatalogs := duplicateCatalogGroups(st)
	r.check("no duplicate catalog per (domain_id, source_url)", dupCatalogs == 0, "duplicate groups=%d", dupCatalogs)

	// 6. Probe hit accounting: 15 well_known hits, 5 misses.
	var wkHits int64
	st.DB.Model(&store.Probe{}).Where("method = ? AND outcome = ?", store.ProbeMethodWellKnown, store.ProbeOutcomeHit).Count(&wkHits)
	r.check("well_known hits == 15", wkHits == 15, "well_known hits=%d", wkHits)

	// 7. Catalogs all verified valid.
	var validCatalogs int64
	st.DB.Model(&store.Catalog{}).Where("verification_status = ?", store.VerificationStatusValid).Count(&validCatalogs)
	r.check("all catalogs verified valid", validCatalogs == expectedCatalogs, "valid=%d of %d", validCatalogs, catalogs)
}

// -- scenario B: shard partition (per-worker log disjointness) ---------------

func assertB(r *reporter, st *store.Store, logsDir string, workers int) {
	// For each worker N, gather the set of fetch-target hosts it completed
	// (from "crawler: item complete" events), and assert every one hashes to
	// worker N's shard partition. Pairwise disjointness then follows for free.
	byWorker, err := completedHostsByWorker(logsDir, workers)
	if err != nil {
		r.check("read per-worker logs", false, "%v", err)
		return
	}

	misowned := map[int][]string{}
	total := 0
	for w := 0; w < workers; w++ {
		for host := range byWorker[w] {
			total++
			if ownerWorker(host, workers) != w {
				misowned[w] = append(misowned[w], host)
			}
		}
	}
	r.check("every completed fetch-host owned by the completing worker's shard",
		len(misowned) == 0, "misowned by worker: %v", misowned)
	r.check("at least some work observed in logs", total > 0, "total completed host observations=%d", total)

	// Pairwise disjointness of host sets (a host is fetched by exactly one worker).
	overlap := pairwiseOverlap(byWorker, workers)
	r.check("worker host sets pairwise disjoint", len(overlap) == 0, "overlaps: %v", overlap)

	// Foreign-host artifact fix: the artifact at site8.test must be completed
	// by site8's owner (worker %10 of its shard), NOT by site1's worker.
	assertForeignArtifact(r, logsDir, workers, "site8.test", "site1.test")
	assertForeignArtifact(r, logsDir, workers, "site11.test", "site3.test")
}

// assertForeignArtifact verifies the artifact hosted on artifactHost (referenced
// by a catalog on catalogHost) was completed by artifactHost's owning worker and
// not by catalogHost's owning worker.
func assertForeignArtifact(r *reporter, logsDir string, workers int, artifactHost, catalogHost string) {
	owner := ownerWorker(artifactHost, workers)
	other := ownerWorker(catalogHost, workers)
	ownerHosts := completedFetchHostsInWorker(logsDir, owner)
	label := fmt.Sprintf("foreign artifact %s (catalog on %s): fetched by owner worker %d not %d", artifactHost, catalogHost, owner, other)
	if owner == other {
		r.check(label, false, "artifact host and catalog host map to same worker %d; pick different fixture hosts", owner)
		return
	}
	_, ownedByOwner := ownerHosts[artifactHost]
	otherHosts := completedFetchHostsInWorker(logsDir, other)
	_, ownedByOther := otherHosts[artifactHost]
	r.check(label, ownedByOwner && !ownedByOther,
		"completedByOwner(w%d)=%v completedByCatalogWorker(w%d)=%v", owner, ownedByOwner, other, ownedByOther)
}

// -- scenario C: kill-and-reclaim -------------------------------------------

func assertC(r *reporter, st *store.Store, workers int) {
	// After a worker was SIGKILL'd mid-crawl and restarted, the frontier must
	// still fully drain with no duplicated rows: the reclaimed in-flight items
	// are completed exactly once.
	var pending, inFlight, leased int64
	st.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusPending).Count(&pending)
	st.DB.Model(&store.FrontierItem{}).Where("status = ?", store.FrontierStatusInFlight).Count(&inFlight)
	st.DB.Model(&store.FrontierItem{}).Where("leased_until IS NOT NULL").Count(&leased)
	r.check("frontier drained after kill+restart", pending == 0 && inFlight == 0, "pending=%d in_flight=%d", pending, inFlight)
	r.check("leases cleared after kill+restart", leased == 0, "leased rows=%d", leased)

	var catalogs, entries, artifacts int64
	st.DB.Model(&store.Catalog{}).Count(&catalogs)
	st.DB.Model(&store.CatalogEntry{}).Count(&entries)
	st.DB.Model(&store.Artifact{}).Count(&artifacts)
	r.check("catalogs exactly once (==15)", catalogs == expectedCatalogs, "catalogs=%d want=%d", catalogs, expectedCatalogs)
	r.check("catalog_entries exactly once (==17)", entries == expectedEntries, "entries=%d want=%d", entries, expectedEntries)
	// SaveArtifact has no dedup guard, so a kill in the window between
	// SaveArtifact and Complete could duplicate an artifact row on reclaim.
	// Report the observed count honestly rather than only pass/fail.
	r.check("artifacts exactly once (==17)", artifacts == expectedArtifact, "artifacts=%d want=%d (a>want would indicate at-least-once artifact reprocessing)", artifacts, expectedArtifact)

	dupCatalogs := duplicateCatalogGroups(st)
	r.check("no duplicate catalog per (domain_id, source_url)", dupCatalogs == 0, "duplicate groups=%d", dupCatalogs)

	missingProbes := hostsMissingProbes(st)
	r.check("every seeded host probed after kill+restart", len(missingProbes) == 0, "hosts without probes: %v", missingProbes)
}

// -- scenario D: re-crawl budget re-activation ------------------------------

func assertD(r *reporter, st *store.Store, logsDir string, workers int) {
	// Budget host page_fetch rows must not have grown beyond the budget and
	// must all be done: the re-crawl re-activated existing done rows rather
	// than being starved by the budget or creating overflow rows.
	var pageRows, donePageRows int64
	st.DB.Model(&store.FrontierItem{}).Where("host = ? AND kind = ?", budgetHost, store.KindPageFetch).Count(&pageRows)
	st.DB.Model(&store.FrontierItem{}).Where("host = ? AND kind = ? AND status = ?", budgetHost, store.KindPageFetch, store.FrontierStatusDone).Count(&donePageRows)
	r.check("budget host page_fetch rows == budget (no overflow, no starvation)", pageRows == budgetPageRows, "page_fetch rows=%d want=%d", pageRows, budgetPageRows)
	r.check("budget host page_fetch rows all done", donePageRows == pageRows, "done=%d of %d", donePageRows, pageRows)

	// Catalogs/entries unchanged by the (non-force) re-crawl: idempotent.
	var catalogs, entries int64
	st.DB.Model(&store.Catalog{}).Count(&catalogs)
	st.DB.Model(&store.CatalogEntry{}).Count(&entries)
	r.check("re-crawl did not duplicate catalogs (==15)", catalogs == expectedCatalogs, "catalogs=%d", catalogs)
	r.check("re-crawl did not duplicate entries (==17)", entries == expectedEntries, "entries=%d", entries)

	// Log evidence: the run-2 logs (cleared before re-crawl) must show the
	// budget host's owning worker RE-COMPLETING a budgeted child page (/pageN).
	// If the old budget bug froze capped pages out, no /pageN would re-fetch.
	owner := ownerWorker(budgetHost, workers)
	childReactivations := completedBudgetChildPages(logsDir, owner, budgetHost)
	r.check(fmt.Sprintf("budget host owner (worker %d) re-fetched a budgeted child page on re-crawl", owner),
		childReactivations > 0, "re-fetched child /pageN count in run-2 logs=%d", childReactivations)
}

// -- shared query helpers ---------------------------------------------------

func hostsMissingProbes(st *store.Store) []string {
	var missing []string
	for i := 1; i <= 20; i++ {
		host := fmt.Sprintf("site%d.test", i)
		var d store.Domain
		if err := st.DB.Where("host = ?", host).First(&d).Error; err != nil {
			missing = append(missing, host+"(no-domain)")
			continue
		}
		var probes int64
		st.DB.Model(&store.Probe{}).Where("domain_id = ?", d.ID).Count(&probes)
		if probes == 0 {
			missing = append(missing, host)
		}
	}
	return missing
}

func duplicateCatalogGroups(st *store.Store) int64 {
	type row struct{ N int64 }
	var rows []row
	st.DB.Raw("SELECT COUNT(*) AS n FROM catalogs GROUP BY domain_id, source_url HAVING COUNT(*) > 1").Scan(&rows)
	return int64(len(rows))
}

// -- log parsing ------------------------------------------------------------

type logEvent struct {
	Msg  string `json:"msg"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
	Host string `json:"host"`
}

// completedHostsByWorker returns, per worker index, the set of fetch-target
// hosts that worker logged as "crawler: item complete".
func completedHostsByWorker(logsDir string, workers int) (map[int]map[string]struct{}, error) {
	out := map[int]map[string]struct{}{}
	for w := 0; w < workers; w++ {
		out[w] = completedFetchHostsInWorker(logsDir, w)
	}
	// Surface a hard error only if literally no log files were found.
	found := false
	for w := 0; w < workers; w++ {
		if _, err := os.Stat(filepath.Join(logsDir, fmt.Sprintf("worker-%d.jsonl", w))); err == nil {
			found = true
			break
		}
	}
	if !found {
		return out, fmt.Errorf("no worker-*.jsonl files in %s", logsDir)
	}
	return out, nil
}

// completedFetchHostsInWorker parses one worker's log and returns the set of
// fetch-target hosts (url's host, or event host when url is empty) it completed.
func completedFetchHostsInWorker(logsDir string, w int) map[string]struct{} {
	set := map[string]struct{}{}
	forEachItemComplete(logsDir, w, func(ev logEvent) {
		set[fetchHost(ev)] = struct{}{}
	})
	return set
}

// completedBudgetChildPages counts item-complete events for kind=page_fetch on
// the budget host whose url path is a /pageN child (not the index "/").
func completedBudgetChildPages(logsDir string, w int, host string) int {
	n := 0
	forEachItemComplete(logsDir, w, func(ev logEvent) {
		if ev.Kind != store.KindPageFetch {
			return
		}
		u, err := url.Parse(ev.URL)
		if err != nil || u.Hostname() != host {
			return
		}
		if strings.HasPrefix(u.Path, "/page") {
			n++
		}
	})
	return n
}

func forEachItemComplete(logsDir string, w int, fn func(logEvent)) {
	f, err := os.Open(filepath.Join(logsDir, fmt.Sprintf("worker-%d.jsonl", w)))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		var ev logEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Msg != "crawler: item complete" {
			continue
		}
		fn(ev)
	}
}

// fetchHost mirrors internal/frontier.fetchHost: url's hostname when present,
// else the event's host field (host_probe items have no url).
func fetchHost(ev logEvent) string {
	if ev.URL != "" {
		if u, err := url.Parse(ev.URL); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return ev.Host
}

func pairwiseOverlap(byWorker map[int]map[string]struct{}, workers int) map[string][]int {
	owners := map[string][]int{}
	for w := 0; w < workers; w++ {
		for host := range byWorker[w] {
			owners[host] = append(owners[host], w)
		}
	}
	overlap := map[string][]int{}
	for host, ws := range owners {
		if len(ws) > 1 {
			sort.Ints(ws)
			overlap[host] = ws
		}
	}
	return overlap
}

// -- reporting --------------------------------------------------------------

type reporter struct {
	passed, failed int
}

func (r *reporter) check(name string, ok bool, detailFmt string, args ...any) {
	detail := fmt.Sprintf(detailFmt, args...)
	if ok {
		r.passed++
		fmt.Printf("PASS  %s  [%s]\n", name, detail)
		return
	}
	r.failed++
	fmt.Printf("FAIL  %s  [%s]\n", name, detail)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
