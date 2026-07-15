package crawler

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/probe"
)

// ProbeEvent describes one per-host crawl result, delivered to
// Options.OnProbe as the crawl runs so callers can render live output rows
// (see `ardvark crawl`). One event is fired per probe miss/error and per
// verified catalog; a probe hit is reported once, as the verified-catalog
// event, so each host result maps to exactly one row.
type ProbeEvent struct {
	// Host is the probed host.
	Host string
	// Method is the discovery method that found the document:
	// "well_known", "robots_agentmap", or "link_tag". Empty when the
	// method is unknown (e.g. a nested catalog referenced by another
	// catalog, or a catalog_fetch resumed after a restart).
	Method string
	// Outcome is "hit", "miss", or "error" (probe outcome values).
	Outcome string
	// Verdict is the catalog verification verdict — "valid",
	// "valid_with_warnings", or "invalid" — for hit events; empty for
	// misses and errors.
	Verdict string
	// Detail is a short human-readable summary for the row's tail:
	// "14 entries" for a valid catalog, the failing check IDs (e.g.
	// "urn.format ×3") for a warned/invalid one, or "404" for a miss.
	Detail string
	// EntryCount is the number of entries in a verified catalog; zero
	// otherwise.
	EntryCount int
}

// emit invokes the OnProbe callback, if configured. Callbacks fire from
// worker goroutines, so callers must supply a goroutine-safe function.
func (e *Engine) emit(ev ProbeEvent) {
	if e.opts.OnProbe != nil {
		e.opts.OnProbe(ev)
	}
}

// probeDetail summarizes a non-hit probe result for ProbeEvent.Detail:
// the error detail for errors, the HTTP status ("404") for misses, and a
// generic "not found" when neither is available (e.g. a robots.txt with no
// Agentmap directive).
func probeDetail(r probe.Result) string {
	switch {
	case r.Outcome == probe.OutcomeError && r.ErrorDetail != "":
		return r.ErrorDetail
	case r.HTTPStatus > 0:
		return strconv.Itoa(r.HTTPStatus)
	case r.ErrorDetail != "":
		return r.ErrorDetail
	default:
		return "not found"
	}
}

// catalogEvent builds the OnProbe event for a verified catalog: valid
// catalogs report their entry count, warned/invalid catalogs summarize
// their failing checks (e.g. "urn.format ×3").
func catalogEvent(host, method string, report ard.Report, entryCount int) ProbeEvent {
	detail := entriesLabel(entryCount)
	switch report.Verdict {
	case ard.VerdictValidWithWarnings:
		detail = failingChecksSummary(report.Checks, ard.SeverityWarning)
	case ard.VerdictInvalid:
		detail = failingChecksSummary(report.Checks, ard.SeverityError)
	}
	return ProbeEvent{
		Host:       host,
		Method:     method,
		Outcome:    probe.OutcomeHit,
		Verdict:    report.Verdict,
		Detail:     detail,
		EntryCount: entryCount,
	}
}

// entriesLabel formats a catalog entry count for ProbeEvent.Detail.
func entriesLabel(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

// failingChecksSummary condenses the failed checks of the given severity
// into a short row tail: check IDs in first-seen order, "×N" for repeats,
// capped at three IDs ("+N more" beyond that).
func failingChecksSummary(checks []ard.Check, severity string) string {
	counts := make(map[string]int)
	var order []string
	for _, c := range checks {
		if c.Passed || c.Severity != severity {
			continue
		}
		if counts[c.CheckID] == 0 {
			order = append(order, c.CheckID)
		}
		counts[c.CheckID]++
	}

	const maxIDs = 3
	parts := make([]string, 0, len(order))
	for _, id := range order {
		if len(parts) == maxIDs {
			parts = append(parts, fmt.Sprintf("+%d more", len(order)-maxIDs))
			break
		}
		if n := counts[id]; n > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", id, n))
		} else {
			parts = append(parts, id)
		}
	}
	return strings.Join(parts, ", ")
}
