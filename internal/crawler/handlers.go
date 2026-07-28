package crawler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/harvest"
	"github.com/helgesverre/ardvark/internal/mediatype"
	"github.com/helgesverre/ardvark/internal/probe"
	"github.com/helgesverre/ardvark/internal/registry"
	"github.com/helgesverre/ardvark/internal/store"
)

// hostOf extracts the host component from rawURL.
func hostOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", errors.New("crawler: url has no host: " + rawURL)
	}
	return u.Host, nil
}

// -- page_fetch -------------------------------------------------------------

// handlePageFetch fetches an HTML page (depth-bounded), extracts anchor
// hosts and links, and enqueues host_probe for newly-seen hosts and
// page_fetch for in-budget links, per the design doc.
func (e *Engine) handlePageFetch(ctx context.Context, item store.FrontierItem) error {
	fetched, skip, err := e.get(ctx, item.URL)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	if !strings.Contains(strings.ToLower(fetched.ContentType), "html") {
		// Not HTML: nothing to harvest, but not a failure either.
		return nil
	}

	result, err := harvest.ExtractLinks(fetched.URL, bytes.NewReader(fetched.Body))
	if err != nil {
		// A malformed page is not a crawl failure; just nothing to harvest.
		e.logger.Warn("crawler: failed to parse page for link harvesting", "url", item.URL, "error", err)
		return nil
	}

	for _, host := range result.Hosts {
		if _, err := e.store.UpsertDomain(host, store.DiscoverySourceAnchor); err != nil {
			e.logger.Warn("crawler: failed to upsert domain from anchor", "host", host, "error", err)
			continue
		}
		if _, err := e.enqueue(store.KindHostProbe, "", host, 0, provenance{}); err != nil {
			return err
		}
	}

	for _, hintURL := range result.AICatalogHints {
		hintHost, err := hostOf(hintURL)
		if err != nil {
			continue
		}
		domain, err := e.store.UpsertDomain(hintHost, store.DiscoverySourceAnchor)
		if err != nil {
			e.logger.Warn("crawler: failed to upsert domain for ai-catalog link hint", "host", hintHost, "error", err)
			continue
		}
		if err := e.store.RecordProbe(&store.Probe{
			DomainID: domain.ID,
			Method:   store.ProbeMethodLinkTag,
			URL:      hintURL,
			Outcome:  probe.OutcomeHit,
			ProbedAt: time.Now(),
		}); err != nil {
			e.logger.Warn("crawler: failed to record link_tag probe", "url", hintURL, "error", err)
		}
		if _, err := e.enqueue(store.KindCatalogFetch, hintURL, hintHost, 0, provenance{ProbeMethod: store.ProbeMethodLinkTag}); err != nil {
			return err
		}
	}

	if err := e.processInlineCatalogs(ctx, result.JSONLD, fetched.URL, item.Host); err != nil {
		return err
	}

	if item.Depth < e.maxDepth() {
		for _, link := range result.Links {
			linkHost, err := hostOf(link)
			if err != nil {
				continue
			}
			// enqueuePageFetch applies the maxPagesPerDomain budget per call,
			// so it both refuses NEW URLs once linkHost is capped and lets an
			// already-known URL be re-activated for free (see its doc comment).
			// Checked here inside the fan-out loop, the budget reflects every
			// page_fetch row the earlier iterations added, so one page cannot
			// overshoot.
			if _, err := e.enqueuePageFetch(link, linkHost, item.Depth+1); err != nil {
				continue
			}
		}
	}

	return nil
}

// processInlineCatalogs ingests ARD catalog documents embedded in a page's
// <script type="application/ld+json"> blocks, which the spec treats as a
// first-class publication mechanism alongside the well-known paths.
//
// Each candidate is persisted through the same processCatalog path as a
// hosted document, so verification, storage, and follow-up fan-out behave
// identically no matter how the catalog was published. Provenance is the page
// URL plus a stable per-block/per-node fragment, which keeps two catalogs on
// one page distinct and stable across re-crawls.
//
// This runs synchronously rather than through its own frontier item, and that
// is safe here: the page body is the only source of these entries, and the
// page_fetch item is not marked done until this returns. A crash part-way
// re-fetches the page and redoes the work, so nothing is permanently lost —
// unlike a probe hit, which is durably recorded before its follow-up work
// exists and therefore needs a persistent item to recover.
// An inline catalog enters at catalog depth 0: it is a top-level document
// that happens to be embedded in a page, not a catalog nested inside another
// catalog. The page's own crawl depth is a different budget (crawler.maxDepth
// governs how far anchors are followed) and must not be spent here, or a
// catalog published on a deep page would be silently ignored.
func (e *Engine) processInlineCatalogs(ctx context.Context, blocks [][]byte, pageURL, host string) error {
	for blockIdx, block := range blocks {
		for nodeIdx, candidate := range ard.InlineCatalogs(block) {
			sum := sha256.Sum256(candidate)
			sourceURL := fmt.Sprintf("%s#ld+json=%d.%d", pageURL, blockIdx, nodeIdx)
			if err := e.processCatalog(ctx, candidate, hex.EncodeToString(sum[:]), host, sourceURL, nil, store.ProbeMethodInPage, 0, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// -- host_probe ---------------------------------------------------------

// handleHostProbe probes item.Host for ARD documents via internal/probe,
// persists the domain and probe rows, and on a hit enqueues catalog_fetch
// for every discovered catalog URL. It skips hosts probed within the
// freshness window unless Options.Force is set.
func (e *Engine) handleHostProbe(ctx context.Context, item store.FrontierItem) error {
	if !e.opts.Force && item.Attempts == 0 {
		recently, err := e.store.RecentlyProbed(item.Host, e.refreshWindow())
		if err != nil {
			// Fail open: the freshness check is an optimization, so a broken
			// lookup must not block the probe — but it shouldn't be silent.
			e.logger.Warn("crawler: recently-probed check failed", "host", item.Host, "error", err)
		} else if recently {
			e.logger.Debug("crawler: skipping recently-probed host", "host", item.Host)

			// A recently-recorded hit doesn't prove THIS item's
			// own catalog_fetch fan-out ever completed. A prior process may
			// have crashed after store.RecordProbe durably persisted a hit
			// (which is what RecentlyProbed keys on) but before the
			// catalog_fetch enqueue loop below ran for it (or ran for all of
			// it: robots_agentmap can discover more than one catalog URL per
			// probe attempt, so a crash partway through that loop can lose
			// some but not all of them), permanently losing that catalog on
			// every future resume, since this branch used to return
			// unconditionally. Re-deriving every durably recorded hit through
			// Frontier.Resume fills missing work, retries failed work, and
			// leaves completed catalog fetches untouched.
			return e.reenqueueKnownCatalogHits(item.Host)
		}
	}

	domain, err := e.store.UpsertDomain(item.Host, store.DiscoverySourceSeed)
	if err != nil {
		return err
	}
	previousLastProbedAt := domain.LastProbedAt

	results := e.probeHost(ctx, e.fetch, item.Host)
	hadHit := false
	var retryErr error
	for _, r := range results {
		if r.Outcome != probe.OutcomeHit {
			if err := e.store.RecordProbe(&store.Probe{
				DomainID:    domain.ID,
				Method:      r.Method,
				URL:         r.URL,
				HTTPStatus:  r.HTTPStatus,
				ContentType: r.ContentType,
				Outcome:     r.Outcome,
				ErrorDetail: r.ErrorDetail,
				ProbedAt:    time.Now(),
			}); err != nil {
				return err
			}
			e.emit(ProbeEvent{Host: item.Host, Method: r.Method, Outcome: r.Outcome, Detail: probeDetail(r)})
			if retryErr == nil && r.RetryError != nil {
				retryErr = r.RetryError
			}
			continue
		}

		hadHit = true
		// One Probe row per discovered catalog URL, not one per method/
		// result: recording (and enqueuing) per-URL is what makes a crash
		// mid-fan-out recoverable for robots_agentmap too, not just
		// well_known's inherently-single-URL case (see BUG 2 /
		// reenqueueKnownCatalogHits). Each row's URL is itself a genuine
		// catalog document URL, so "outcome = hit" alone identifies every
		// recoverable follow-up for this host, uniformly across methods.
		for _, catalogURL := range r.CatalogURLs {
			if err := e.store.RecordProbe(&store.Probe{
				DomainID:    domain.ID,
				Method:      r.Method,
				URL:         catalogURL,
				HTTPStatus:  r.HTTPStatus,
				ContentType: r.ContentType,
				Outcome:     r.Outcome,
				ProbedAt:    time.Now(),
			}); err != nil {
				return err
			}
			if _, err := e.resume(store.KindCatalogFetch, catalogURL, item.Host, 0, provenance{ProbeMethod: r.Method}); err != nil {
				return err
			}
		}
	}

	switch {
	case hadHit:
		// This host advertises ARD. The verification verdict is written
		// against whichever host actually serves the catalog, which may be a
		// different one, so record what we know about THIS host rather than
		// leaving it looking as though it was never probed. Promotion never
		// demotes a verdict this host has already earned.
		if err := e.store.PromoteDomainAdvertised(domain.ID); err != nil {
			e.logger.Warn("crawler: failed to mark domain advertised", "host", item.Host, "error", err)
		}
	case retryErr == nil:
		if err := e.store.UpdateDomainARDStatus(domain.ID, store.ARDStatusNotFound); err != nil {
			e.logger.Warn("crawler: failed to mark domain not_found", "host", item.Host, "error", err)
		}
	}
	if retryErr != nil {
		if err := e.store.RestoreDomainLastProbedAt(domain.ID, previousLastProbedAt); err != nil {
			return err
		}
		return fmt.Errorf("crawler: probing host %s: %w", item.Host, retryErr)
	}

	e.logger.Info("crawler: host probed", "host", item.Host, "domain_id", domain.ID)
	return nil
}

// reenqueueKnownCatalogHits ensures catalog_fetch exists for every durably
// recorded probe hit on host, closing the gap documented at handleHostProbe's
// freshness-skip branch without reactivating completed work.
//
// This works uniformly across both probe methods because handleHostProbe
// records one Probe row per discovered catalog URL (not one per method
// attempt): every row with Outcome = hit therefore IS itself a catalog
// document URL, whether it came from well_known's single URL or one of
// robots_agentmap's possibly-several Agentmap directives. So "every hit row
// for this domain" is exactly "every catalog_fetch this host_probe ever
// durably promised", and re-enqueuing all of them recovers a crash at any
// point in the original fan-out loop, not just after its first entry.
// Anything discovered but not yet durably recorded before the crash is, by
// construction, not recoverable here — it was never promised — and will be
// rediscovered on the next non-skipped probe (window expiry or --force).
func (e *Engine) reenqueueKnownCatalogHits(host string) error {
	domain, err := e.store.DomainByHost(host)
	if err != nil {
		return fmt.Errorf("crawler: looking up known hits for %s: %w", host, err)
	}
	var hits []store.Probe
	if err := e.store.DB.
		Where("domain_id = ? AND outcome = ?", domain.ID, probe.OutcomeHit).
		Order("id ASC").
		Find(&hits).Error; err != nil {
		return fmt.Errorf("crawler: loading known hits for %s: %w", host, err)
	}
	for _, hit := range hits {
		if _, err := e.resume(store.KindCatalogFetch, hit.URL, host, 0, provenance{ProbeMethod: hit.Method}); err != nil {
			return err
		}
	}
	return nil
}

// -- catalog_fetch --------------------------------------------------------

// handleCatalogFetch fetches and verifies an ai-catalog.json document, and
// delegates the rest to processCatalog. Like well-known probing, fetching an
// already-discovered ARD catalog document bypasses the robots.txt gate: per
// the design doc, robots.txt governs page crawling, not ARD document
// discovery/retrieval, and a host that discovers a catalog via a
// robots-exempt mechanism (well-known, Agentmap) must not then have that
// same document blocked from being fetched.
func (e *Engine) handleCatalogFetch(ctx context.Context, item store.FrontierItem) error {
	fetched, err := e.fetch.GetWellKnown(ctx, item.URL)
	if err != nil {
		// LOW fix: a host_probe hit that leads to a catalog_fetch which then
		// permanently fails (e.g. a 404 on a document that briefly existed)
		// otherwise emits zero ProbeEvents for that host, breaking
		// ProbeEvent's documented "one row per host" contract. Only
		// non-transient failures are reported here: a transient failure will
		// be retried (see fetch.Transient/Options.MaxAttempts), so emitting
		// on every attempt would double- or triple-report the same host.
		if !fetch.Transient(err) {
			e.emit(ProbeEvent{Host: item.Host, Method: item.ProbeMethod, Outcome: probe.OutcomeError, Detail: fetchErrorDetail(err)})
		}
		return err
	}

	transportChecks := ard.TransportChecks(fetched.ContentType, fetched.Body, e.cfg.Crawler.MaxBodyBytes)

	servingHost, err := hostOf(fetched.URL)
	if err != nil {
		return fmt.Errorf("crawler: resolving fetched catalog host: %w", err)
	}
	return e.processCatalog(ctx, fetched.Body, fetched.SHA256, servingHost, fetched.URL, item.ParentCatalogID, item.ProbeMethod, item.Depth, transportChecks)
}

// fetchErrorDetail summarizes a catalog_fetch failure for ProbeEvent.Detail:
// the HTTP status if one was received, or the error text otherwise. Mirrors
// probeDetail's precedence, minus the ErrorDetail field host_probe results
// carry that a bare fetch error does not.
func fetchErrorDetail(err error) string {
	var fe *fetch.Error
	if errors.As(err, &fe) && fe.Status > 0 {
		return strconv.Itoa(fe.Status)
	}
	return err.Error()
}

// processCatalog verifies raw catalog bytes, persists the catalog, its
// entries, and its verification checks, and (within ard.maxCatalogDepth)
// enqueues follow-on work per entry: catalog_fetch for url-referenced
// nested catalogs, a direct recursive call for data-embedded nested
// catalogs, registry_harvest for registry entries, and artifact_fetch for
// everything else. transportChecks (step 1 of the verification pipeline)
// are supplied by the caller since they depend on HTTP transport metadata;
// pass nil for data-embedded nested catalogs, which were never
// independently fetched over HTTP. probeMethod is which probe method
// discovered sourceURL (empty for data-embedded nested catalogs, which were
// never independently discovered), reported on ProbeEvents.
func (e *Engine) processCatalog(ctx context.Context, raw []byte, contentHash, host, sourceURL string, parentCatalogID *uint, probeMethod string, depth int, transportChecks []ard.Check) error {
	report := ard.Verify(raw, host).MergeChecks(transportChecks)

	var parsed ard.Catalog
	_ = json.Unmarshal(raw, &parsed) // best-effort; parse failures are already captured in report.Checks

	domain, err := e.store.UpsertDomain(host, store.DiscoverySourceCatalogRef)
	if err != nil {
		return err
	}

	if !e.opts.Force {
		prior, err := e.store.LatestCatalogBySourceAndParent(domain.ID, sourceURL, parentCatalogID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && prior.ContentHash == contentHash {
			e.logger.Debug("crawler: catalog unchanged since last fetch, skipping re-save", "url", sourceURL, "host", host)
			e.emit(ProbeEvent{
				Host:    host,
				Method:  probeMethod,
				Outcome: probe.OutcomeHit,
				Verdict: prior.VerificationStatus,
				Detail:  "unchanged",
			})

			// "Unchanged" only means the save is redundant, not that this
			// item's follow-on work was ever enqueued. A prior
			// process may have crashed after store.SaveCatalog committed but
			// before enqueueEntryFollowups ran (or before this branch even
			// existed to run it), permanently losing that catalog's nested
			// fetches/harvests/artifacts on every future resume, since this
			// short-circuit used to return before fan-out. Re-running fan-out
			// is safe because relationship-specific work uses Frontier.Ensure:
			// missing rows are inserted while completed rows remain completed.
			if depth < e.maxCatalogDepth() {
				if err := e.store.DB.Where("catalog_id = ?", prior.ID).Order("id ASC").Find(&prior.Entries).Error; err != nil {
					return fmt.Errorf("crawler: loading prior catalog entries for %s: %w", sourceURL, err)
				}
				if len(prior.Entries) != len(parsed.Entries) {
					// Should not happen (identical content hash implies
					// identical entries), but a stale/foreign prior row must
					// not be zipped against the wrong entries by index.
					e.logger.Warn("crawler: prior catalog entry count mismatch, skipping follow-up re-enqueue", "url", sourceURL, "prior_entries", len(prior.Entries), "parsed_entries", len(parsed.Entries))
					return nil
				}
				if err := e.enqueueEntryFollowups(ctx, prior, parsed.Entries, host, sourceURL, depth); err != nil {
					return err
				}
			}
			return nil
		}
	}

	cat := &store.Catalog{
		DomainID:           domain.ID,
		SourceURL:          sourceURL,
		ParentCatalogID:    parentCatalogID,
		SpecVersion:        parsed.SpecVersion,
		HostDisplayName:    parsed.Host.DisplayName,
		HostIdentifier:     parsed.Host.Identifier,
		RawJSON:            store.LongText(raw),
		ContentHash:        contentHash,
		FetchedAt:          time.Now(),
		VerificationStatus: report.Verdict,
		Entries:            buildEntryRows(parsed.Entries),
	}

	catalogChecks, entryChecks := splitChecks(report.Checks, parsed.Entries)

	if err := e.store.SaveCatalog(cat, catalogChecks, entryChecks); err != nil {
		return err
	}

	ardStatus := store.ARDStatusFoundValid
	if report.Verdict == ard.VerdictInvalid {
		ardStatus = store.ARDStatusFoundInvalid
	}
	if err := e.store.UpdateDomainARDStatus(domain.ID, ardStatus); err != nil {
		e.logger.Warn("crawler: failed to update domain ard_status after verification", "host", host, "error", err)
	}

	e.logger.Info("crawler: catalog verified", "url", sourceURL, "host", host, "verdict", report.Verdict, "entries", len(parsed.Entries))
	e.emit(catalogEvent(host, probeMethod, report, len(parsed.Entries)))

	if depth >= e.maxCatalogDepth() {
		return nil
	}

	return e.enqueueEntryFollowups(ctx, cat, parsed.Entries, host, sourceURL, depth)
}

// enqueueEntryFollowups fans out follow-on work for each entry of a just-
// persisted catalog: catalog_fetch for url-referenced nested catalogs, a
// direct recursive processCatalog call for data-embedded ones,
// registry_harvest for registry entries, and artifact_fetch for everything
// else. Kept separate from processCatalog so persistence and fan-out policy
// don't intertwine.
func (e *Engine) enqueueEntryFollowups(ctx context.Context, cat *store.Catalog, entries []ard.Entry, host, sourceURL string, depth int) error {
	for i, en := range entries {
		entryID := cat.Entries[i].ID
		catID := cat.ID
		kind := mediatype.Parse(en.Type).Kind()

		switch {
		case kind == mediatype.KindCatalog:
			if err := e.followCatalogPointer(ctx, en, host, sourceURL, depth+1, &catID); err != nil {
				return err
			}

		case kind == mediatype.KindRegistry && en.URL != "" && e.cfg.Registry.Harvest:
			if err := e.followRegistryPointer(en.URL, host, 0,
				&store.Registry{EntryID: entryID, BaseURL: en.URL, HarvestStatus: store.HarvestStatusPending},
				provenance{RegistryEntryID: &entryID, RegistryCatalogID: &catID}); err != nil {
				return err
			}

		case en.URL != "" && e.cfg.ARD.FetchArtifacts:
			if _, err := e.ensure(store.KindArtifactFetch, en.URL, host, 0, provenance{ArtifactEntryID: &entryID}); err != nil {
				return err
			}
		}
	}
	return nil
}

// followCatalogPointer follows an ai-catalog pointer entry: a url-referenced
// nested catalog becomes a catalog_fetch at depth; an embedded-data catalog is
// processed recursively at depth. A pointer with neither url nor data is a
// no-op. Shared by the catalog fan-out and the registry-result fan-out.
func (e *Engine) followCatalogPointer(ctx context.Context, en ard.Entry, host, sourceURL string, depth int, parentCatID *uint) error {
	switch {
	case en.URL != "":
		_, err := e.ensure(store.KindCatalogFetch, en.URL, host, depth, provenance{ParentCatalogID: parentCatID})
		return err
	case hasEmbeddedData(en.Data):
		sum := sha256.Sum256(en.Data)
		nestedSource := sourceURL + "#" + en.Identifier
		if err := e.processCatalog(ctx, en.Data, hex.EncodeToString(sum[:]), host, nestedSource, parentCatID, "", depth, nil); err != nil {
			return err
		}
	}
	return nil
}

// followRegistryPointer persists a registries row and enqueues a
// registry_harvest for it at depth. The caller supplies the row (with its
// provenance columns) and the frontier-item provenance; this sets
// prov.RegistryRowID to the newly-saved row's ID. Shared by the catalog
// fan-out and the registry-result fan-out.
func (e *Engine) followRegistryPointer(url, host string, depth int, regRow *store.Registry, prov provenance) error {
	if err := e.store.SaveRegistry(regRow); err != nil {
		return err
	}
	prov.RegistryRowID = &regRow.ID
	_, err := e.ensure(store.KindRegistryHarvest, url, host, depth, prov)
	return err
}

// hasEmbeddedData reports whether raw carries a real JSON value: an absent
// field and an explicit JSON null (which encoding/json leaves as the raw
// bytes "null") are both "no data" for ARD purposes.
func hasEmbeddedData(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// buildEntryRows converts parsed ARD entries into store.CatalogEntry rows
// (CatalogID is filled in by store.SaveCatalog).
func buildEntryRows(entries []ard.Entry) []store.CatalogEntry {
	rows := make([]store.CatalogEntry, len(entries))
	for i, en := range entries {
		rows[i] = buildEntryRow(en, store.EntrySourceCatalog, nil)
	}
	return rows
}

// buildEntryRow converts a single parsed ARD entry into a store.CatalogEntry
// row.
func buildEntryRow(en ard.Entry, source string, sourceRegistryID *uint) store.CatalogEntry {
	var publisher, namespace, name string
	if urn, err := ard.ParseURN(en.Identifier); err == nil {
		publisher = urn.Publisher
		namespace = strings.Join(urn.Namespace, ":")
		name = urn.Name
	}

	// The entry's own bytes are authoritative. Re-marshaling the Go struct
	// would silently drop @context, @id, and any namespaced extension terms
	// the struct does not model. Fall back to marshaling only for entries
	// built in code rather than decoded from a document.
	raw := en.Raw
	if len(raw) == 0 {
		raw, _ = json.Marshal(en)
	}

	return store.CatalogEntry{
		URN:                   en.Identifier,
		URNPublisher:          publisher,
		URNNamespace:          namespace,
		URNName:               name,
		DisplayName:           en.DisplayName,
		MediaType:             en.Type,
		RefURL:                en.URL,
		HasEmbeddedData:       hasEmbeddedData(en.Data),
		Description:           en.Description,
		Version:               en.Version,
		EntryUpdatedAt:        en.UpdatedAt,
		Tags:                  jsonOrEmpty(en.Tags),
		Capabilities:          jsonOrEmpty(en.Capabilities),
		RepresentativeQueries: jsonOrEmpty(en.RepresentativeQueries),
		TrustManifest:         store.LongText(jsonOrEmpty(en.TrustManifest)),
		RawJSON:               store.LongText(raw),
		Source:                source,
		SourceRegistryID:      sourceRegistryID,
	}
}

// jsonOrEmpty marshals v to a JSON string, returning "" on a nil/empty
// value or a marshal error (the latter should not happen for the
// well-typed fields this is used for).
func jsonOrEmpty(v any) string {
	switch t := v.(type) {
	case []string:
		if len(t) == 0 {
			return ""
		}
	case *ard.TrustManifest:
		if t == nil {
			return ""
		}
	}
	b, err := json.Marshal(v)
	if err != nil || !hasEmbeddedData(b) {
		return ""
	}
	return string(b)
}

// splitChecks converts ard.PartitionChecks' shared catalog/entry partition
// into persistence rows. Keeping the identifier-duplicate policy in ard
// ensures initial verification and verify --stored cannot drift.
func splitChecks(checks []ard.Check, entries []ard.Entry) ([]*store.VerificationCheck, map[int][]*store.VerificationCheck) {
	catalog, byEntry := ard.PartitionChecks(checks, entries)
	catalogChecks := make([]*store.VerificationCheck, 0, len(catalog))
	entryChecks := make(map[int][]*store.VerificationCheck)

	toRow := func(c ard.Check) *store.VerificationCheck {
		return &store.VerificationCheck{
			CheckID: c.CheckID, Severity: c.Severity, Passed: c.Passed,
			Message: c.Message, SpecRef: c.SpecRef, CheckedAt: time.Now(),
		}
	}
	for _, check := range catalog {
		catalogChecks = append(catalogChecks, toRow(check))
	}
	for idx, entry := range byEntry {
		rows := make([]*store.VerificationCheck, 0, len(entry))
		for _, check := range entry {
			rows = append(rows, toRow(check))
		}
		entryChecks[idx] = rows
	}
	return catalogChecks, entryChecks
}

// -- artifact_fetch -------------------------------------------------------

// handleArtifactFetch fetches a referenced artifact document (agent card,
// MCP server card, ...) and stores it raw, with its hash. A permanent
// (non-retryable) fetch failure is recorded as an errored artifact rather
// than propagated, since it is a terminal, expected outcome, not a crawl
// bug.
func (e *Engine) handleArtifactFetch(ctx context.Context, item store.FrontierItem) error {
	entryID := uintOrZero(item.ArtifactEntryID)

	fetched, skip, err := e.get(ctx, item.URL)
	if skip {
		return nil
	}
	if err != nil {
		if fetch.Transient(err) {
			return err
		}
		status := 0
		var fe *fetch.Error
		if errors.As(err, &fe) {
			status = fe.Status
		}
		return e.store.SaveArtifact(&store.Artifact{
			EntryID:     entryID,
			URL:         item.URL,
			HTTPStatus:  status,
			FetchStatus: store.FetchStatusError,
			FetchedAt:   time.Now(),
		})
	}

	return e.store.SaveArtifact(&store.Artifact{
		EntryID:     entryID,
		URL:         fetched.URL,
		HTTPStatus:  fetched.Status,
		ContentType: fetched.ContentType,
		RawBody:     fetched.Body,
		ContentHash: fetched.SHA256,
		FetchStatus: store.FetchStatusOK,
		FetchedAt:   time.Now(),
	})
}

// -- registry_harvest -------------------------------------------------------

// handleRegistryHarvest queries a discovered ARD registry's /search
// endpoint (paginating within registry.pageLimit), stores returned entries
// with source=registry provenance, and enqueues further registry_harvest
// items for referrals, bounded by registry.maxReferralDepth.
//
// Search results are relevance-ranked projections, not complete ARD entries:
// they may legitimately omit terms such as representativeQueries, so they are
// persisted with registry provenance rather than verified as full entries.
func (e *Engine) handleRegistryHarvest(ctx context.Context, item store.FrontierItem) error {
	if item.Depth > e.maxReferralDepth() {
		return nil
	}

	known := item.RegistryRowID != nil

	// Only ask the registry to federate when a referral it returns could
	// still be followed; at the depth ceiling the referrals below are
	// discarded, so requesting them would be work neither side can use.
	federation := registry.FederationNone
	if item.Depth+1 <= e.maxReferralDepth() {
		federation = registry.FederationReferrals
	}

	client := registry.New(item.URL, e.registryHTTPClient())
	result, err := client.HarvestAll(ctx, registry.HarvestOptions{
		PageLimit:  e.cfg.Registry.PageLimit,
		Federation: federation,
	})
	if err != nil {
		if registry.IsNotImplemented(err) {
			if known {
				e.updateRegistryStatus(*item.RegistryRowID, store.HarvestStatusError)
			}
			e.logger.Info("crawler: registry does not implement search", "url", item.URL)
			return nil
		}
		return err
	}

	if !known {
		// Provenance lost (e.g. resumed after a restart with a stale row
		// predating the provenance columns): we cannot attribute harvested
		// entries to a catalog/registry row we don't know, so log and stop
		// here rather than guessing.
		e.logger.Warn("crawler: registry_harvest has no known provenance, skipping persistence", "url", item.URL)
		return nil
	}

	regRowID := *item.RegistryRowID
	entryID := uintOrZero(item.RegistryEntryID)
	catalogID := uintOrZero(item.RegistryCatalogID)

	e.updateRegistryStatus(regRowID, store.HarvestStatusOK)

	if len(result.Results) > 0 {
		rows := make([]store.CatalogEntry, len(result.Results))
		for i, res := range result.Results {
			row := buildEntryRow(res.Entry, store.EntrySourceRegistry, &regRowID)
			row.CatalogID = catalogID
			rows[i] = row
		}
		if err := e.store.SaveEntries(rows); err != nil {
			return err
		}

		// Follow catalog/registry POINTERS among the results (pointers only —
		// ordinary results are already persisted and are not artifact-fetched).
		// SaveEntries populated rows[i].ID.
		for i, res := range result.Results {
			switch mediatype.Parse(res.Type).Kind() {
			case mediatype.KindCatalog:
				if err := e.followCatalogPointer(ctx, res.Entry, item.Host, item.URL, 0, &catalogID); err != nil {
					return err
				}
			case mediatype.KindRegistry:
				if res.URL == "" || !e.cfg.Registry.Harvest {
					continue
				}
				resultEntryID := rows[i].ID
				if err := e.followRegistryPointer(res.URL, item.Host, item.Depth+1,
					&store.Registry{EntryID: resultEntryID, BaseURL: res.URL, HarvestStatus: store.HarvestStatusPending, ReferralSourceID: uintPtr(regRowID)},
					provenance{RegistryEntryID: &resultEntryID, RegistryCatalogID: &catalogID}); err != nil {
					return err
				}
			}
		}
	}

	for _, ref := range result.Referrals {
		if ref.URL == "" {
			continue
		}
		nextDepth := item.Depth + 1
		if nextDepth > e.maxReferralDepth() {
			continue
		}
		refRow := &store.Registry{
			EntryID:          entryID,
			BaseURL:          ref.URL,
			HarvestStatus:    store.HarvestStatusPending,
			ReferralSourceID: uintPtr(regRowID),
		}
		if err := e.store.SaveRegistry(refRow); err != nil {
			return err
		}
		if _, err := e.ensure(store.KindRegistryHarvest, ref.URL, item.Host, nextDepth, provenance{
			RegistryEntryID:   &entryID,
			RegistryCatalogID: &catalogID,
			RegistryRowID:     &refRow.ID,
		}); err != nil {
			return err
		}
	}

	return nil
}

// updateRegistryStatus records the outcome of a harvest attempt on the
// registries row; failures are warn-logged rather than propagated since the
// harvested data itself has already been handled.
func (e *Engine) updateRegistryStatus(regRowID uint, status string) {
	if err := e.store.UpdateRegistryStatus(regRowID, status, time.Now()); err != nil {
		e.logger.Warn("crawler: failed to update registry status", "registry_id", regRowID, "error", err)
	}
}

func uintPtr(v uint) *uint { return &v }

// uintOrZero dereferences p, returning 0 for a nil pointer (the "provenance
// unknown" case, e.g. an item enqueued before this frontier_items column
// existed).
func uintOrZero(p *uint) uint {
	if p == nil {
		return 0
	}
	return *p
}
