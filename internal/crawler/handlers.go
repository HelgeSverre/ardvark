package crawler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/harvest"
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
		e.enqueue(store.KindHostProbe, "", host, 0, provenance{})
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
		e.enqueue(store.KindCatalogFetch, hintURL, hintHost, 0, provenance{ProbeMethod: store.ProbeMethodLinkTag})
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

// -- host_probe ---------------------------------------------------------

// handleHostProbe probes item.Host for ARD documents via internal/probe,
// persists the domain and probe rows, and on a hit enqueues catalog_fetch
// for every discovered catalog URL. It skips hosts probed within the
// freshness window unless Options.Force is set.
func (e *Engine) handleHostProbe(ctx context.Context, item store.FrontierItem) error {
	if !e.opts.Force {
		recently, err := e.store.RecentlyProbed(item.Host, e.refreshWindow())
		if err != nil {
			// Fail open: the freshness check is an optimization, so a broken
			// lookup must not block the probe — but it shouldn't be silent.
			e.logger.Warn("crawler: recently-probed check failed", "host", item.Host, "error", err)
		} else if recently {
			e.logger.Debug("crawler: skipping recently-probed host", "host", item.Host)
			return nil
		}
	}

	domain, err := e.store.UpsertDomain(item.Host, store.DiscoverySourceSeed)
	if err != nil {
		return err
	}

	results := probe.Probe(ctx, e.fetch, item.Host)
	hadHit := false
	for _, r := range results {
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

		if r.Outcome != probe.OutcomeHit {
			e.emit(ProbeEvent{Host: item.Host, Method: r.Method, Outcome: r.Outcome, Detail: probeDetail(r)})
			continue
		}
		hadHit = true
		for _, catalogURL := range r.CatalogURLs {
			e.enqueue(store.KindCatalogFetch, catalogURL, item.Host, 0, provenance{ProbeMethod: r.Method})
		}
	}

	if !hadHit {
		if err := e.store.UpdateDomainARDStatus(domain.ID, store.ARDStatusNotFound); err != nil {
			e.logger.Warn("crawler: failed to mark domain not_found", "host", item.Host, "error", err)
		}
	}

	e.logger.Info("crawler: host probed", "host", item.Host, "domain_id", domain.ID)
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
		return err
	}

	transportChecks := ard.TransportChecks(fetched.ContentType, fetched.Body, e.cfg.Crawler.MaxBodyBytes)

	return e.processCatalog(ctx, fetched.Body, fetched.SHA256, item.Host, item.URL, item.ParentCatalogID, item.ProbeMethod, item.Depth, transportChecks)
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
		if prior, err := e.store.LatestCatalogBySource(domain.ID, sourceURL); err == nil && prior.ContentHash == contentHash {
			e.logger.Debug("crawler: catalog unchanged since last fetch, skipping re-save", "url", sourceURL, "host", host)
			e.emit(ProbeEvent{
				Host:    host,
				Method:  probeMethod,
				Outcome: probe.OutcomeHit,
				Verdict: prior.VerificationStatus,
				Detail:  "unchanged",
			})
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

	e.enqueueEntryFollowups(ctx, cat, parsed.Entries, host, sourceURL, depth)
	return nil
}

// enqueueEntryFollowups fans out follow-on work for each entry of a just-
// persisted catalog: catalog_fetch for url-referenced nested catalogs, a
// direct recursive processCatalog call for data-embedded ones,
// registry_harvest for registry entries, and artifact_fetch for everything
// else. Kept separate from processCatalog so persistence and fan-out policy
// don't intertwine.
func (e *Engine) enqueueEntryFollowups(ctx context.Context, cat *store.Catalog, entries []ard.Entry, host, sourceURL string, depth int) {
	for i, en := range entries {
		entryID := cat.Entries[i].ID

		switch {
		case en.Type == mediaTypeAICatalog && en.URL != "":
			catID := cat.ID
			e.enqueue(store.KindCatalogFetch, en.URL, host, depth+1, provenance{ParentCatalogID: &catID})

		case en.Type == mediaTypeAICatalog && hasEmbeddedData(en.Data):
			sum := sha256.Sum256(en.Data)
			nestedSource := sourceURL + "#" + en.Identifier
			catID := cat.ID
			if err := e.processCatalog(ctx, en.Data, hex.EncodeToString(sum[:]), host, nestedSource, &catID, "", depth+1, nil); err != nil {
				e.logger.Warn("crawler: failed to process embedded nested catalog", "identifier", en.Identifier, "error", err)
			}

		case en.Type == mediaTypeAIRegistry && en.URL != "" && e.cfg.Registry.Harvest:
			regRow := &store.Registry{
				EntryID:       entryID,
				BaseURL:       en.URL,
				HarvestStatus: store.HarvestStatusPending,
			}
			if err := e.store.SaveRegistry(regRow); err != nil {
				e.logger.Warn("crawler: failed to save registry row", "url", en.URL, "error", err)
				continue
			}
			catID := cat.ID
			e.enqueue(store.KindRegistryHarvest, en.URL, host, 0, provenance{
				RegistryEntryID:   &entryID,
				RegistryCatalogID: &catID,
				RegistryRowID:     &regRow.ID,
			})

		case en.URL != "" && e.cfg.ARD.FetchArtifacts:
			e.enqueue(store.KindArtifactFetch, en.URL, host, 0, provenance{ArtifactEntryID: &entryID})
		}
	}
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

	raw, _ := json.Marshal(en)

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

// splitChecks partitions ard.Verify's flat check list into catalog-level
// checks and per-entry checks (keyed by entry slice index), matching each
// check's Subject against entry identifiers.
func splitChecks(checks []ard.Check, entries []ard.Entry) ([]*store.VerificationCheck, map[int][]*store.VerificationCheck) {
	idxByIdentifier := make(map[string]int, len(entries))
	for i, en := range entries {
		if en.Identifier != "" {
			idxByIdentifier[en.Identifier] = i
		}
	}

	var catalogChecks []*store.VerificationCheck
	entryChecks := make(map[int][]*store.VerificationCheck)

	for _, c := range checks {
		row := &store.VerificationCheck{
			CheckID:   c.CheckID,
			Severity:  c.Severity,
			Passed:    c.Passed,
			Message:   c.Message,
			SpecRef:   c.SpecRef,
			CheckedAt: time.Now(),
		}
		if c.Subject != "" && c.Subject != "catalog" {
			if idx, ok := idxByIdentifier[c.Subject]; ok {
				entryChecks[idx] = append(entryChecks[idx], row)
				continue
			}
		}
		catalogChecks = append(catalogChecks, row)
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
func (e *Engine) handleRegistryHarvest(ctx context.Context, item store.FrontierItem) error {
	if item.Depth > e.maxReferralDepth() {
		return nil
	}

	known := item.RegistryRowID != nil

	client := registry.New(item.URL, e.registryHTTPClient())
	result, err := client.HarvestAll(ctx, registry.HarvestOptions{
		PageLimit: e.cfg.Registry.PageLimit,
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
			e.logger.Warn("crawler: failed to save referral registry row", "url", ref.URL, "error", err)
			continue
		}
		e.enqueue(store.KindRegistryHarvest, ref.URL, item.Host, nextDepth, provenance{
			RegistryEntryID:   &entryID,
			RegistryCatalogID: &catalogID,
			RegistryRowID:     &refRow.ID,
		})
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
