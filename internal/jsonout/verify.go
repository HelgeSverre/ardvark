package jsonout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/store"
)

// VerifyCheck is one verification check result, the JSON-facing shape of an
// ard.Check.
type VerifyCheck struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	// Subject is the entry URN the check applies to, or "catalog" for
	// catalog-wide checks.
	Subject string `json:"subject"`
	Passed  bool   `json:"passed"`
	Message string `json:"message"`
}

// VerifyReport is the full check report for one catalog document.
type VerifyReport struct {
	Source  string        `json:"source"`
	Verdict string        `json:"verdict"`
	Checks  []VerifyCheck `json:"checks"`
}

// VerifyStoredResult is the outcome of re-verifying every stored catalog.
type VerifyStoredResult struct {
	Reports    []VerifyReport `json:"reports"`
	ReVerified int            `json:"re_verified"`
	Invalid    int            `json:"invalid"`
}

// newVerifyReport converts an ard.Report into the JSON-facing VerifyReport,
// labeled with the source it was verified from.
func newVerifyReport(source string, report ard.Report) VerifyReport {
	checks := make([]VerifyCheck, len(report.Checks))
	for i, c := range report.Checks {
		subject := c.Subject
		if subject == "" {
			subject = ard.SubjectCatalog
		}
		checks[i] = VerifyCheck{
			ID:       c.CheckID,
			Severity: c.Severity,
			Subject:  subject,
			Passed:   c.Passed,
			Message:  c.Message,
		}
	}
	return VerifyReport{Source: source, Verdict: report.Verdict, Checks: checks}
}

// VerifyTarget verifies a single catalog document, fetched via HTTP when
// target looks like a URL, or read from the local filesystem otherwise,
// using ardvark's default lenient verification, and returns its full check
// report. Signature unchanged for existing callers (e.g. the MCP server);
// see VerifyTargetStrict for the --strict variant.
func VerifyTarget(ctx context.Context, target string) (VerifyReport, error) {
	return verifyTarget(ctx, target, false)
}

// VerifyTargetStrict is VerifyTarget, but validates against the exact
// published spec schema (ard.VerifyStrict) rather than ardvark's lenient
// default.
func VerifyTargetStrict(ctx context.Context, target string) (VerifyReport, error) {
	return verifyTarget(ctx, target, true)
}

func verifyTarget(ctx context.Context, target string, strict bool) (VerifyReport, error) {
	raw, servingDomain, err := fetchVerifyTarget(ctx, target)
	if err != nil {
		return VerifyReport{}, err
	}
	return newVerifyReport(target, runVerify(raw, servingDomain, strict)), nil
}

// runVerify dispatches to ard.Verify or ard.VerifyStrict depending on
// strict.
func runVerify(raw []byte, servingDomain string, strict bool) ard.Report {
	if strict {
		return ard.VerifyStrict(raw, servingDomain)
	}
	return ard.Verify(raw, servingDomain)
}

// fetchVerifyTarget loads the raw catalog bytes for target, either via HTTP
// (if it looks like a URL) or from the local filesystem, and derives the
// serving domain used by the urn.publisher_matches check.
func fetchVerifyTarget(ctx context.Context, target string) (raw []byte, servingDomain string, err error) {
	if strings.Contains(target, "://") {
		u, perr := url.Parse(target)
		if perr != nil {
			return nil, "", fmt.Errorf("verify: invalid URL %q: %w", target, perr)
		}
		client := &http.Client{Timeout: 15 * time.Second}
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if rerr != nil {
			return nil, "", fmt.Errorf("verify: building request: %w", rerr)
		}
		resp, derr := client.Do(req)
		if derr != nil {
			return nil, "", fmt.Errorf("verify: fetching %s: %w", target, derr)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, "", fmt.Errorf("verify: fetching %s: unexpected status %d", target, resp.StatusCode)
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
		if rerr != nil {
			return nil, "", fmt.Errorf("verify: reading response from %s: %w", target, rerr)
		}
		return body, u.Hostname(), nil
	}

	body, ferr := os.ReadFile(target)
	if ferr != nil {
		return nil, "", fmt.Errorf("verify: reading %s: %w", target, ferr)
	}
	return body, "", nil
}

// VerifyStored re-runs verification against every catalog currently stored
// in the database, updating each catalog's verification_status and replacing
// its verification_checks rows with the fresh results. strict selects
// ard.VerifyStrict instead of the default ard.Verify, as with VerifyTarget.
// onReport, if non-nil, is invoked with each catalog's report as it is
// produced (the CLI's live per-catalog output).
func VerifyStored(st *store.Store, strict bool, onReport func(VerifyReport)) (VerifyStoredResult, error) {
	var catalogs []store.Catalog
	if err := st.DB.Preload("Entries").Find(&catalogs).Error; err != nil {
		return VerifyStoredResult{}, fmt.Errorf("verify --stored: loading catalogs: %w", err)
	}

	res := VerifyStoredResult{Reports: []VerifyReport{}}
	for _, cat := range catalogs {
		domain, err := st.DomainByID(cat.DomainID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return VerifyStoredResult{}, fmt.Errorf("verify --stored: loading domain %d: %w", cat.DomainID, err)
		}

		report := runVerify([]byte(cat.RawJSON), domain.Host, strict)
		vr := newVerifyReport(fmt.Sprintf("%s (%s)", cat.SourceURL, domain.Host), report)
		if onReport != nil {
			onReport(vr)
		}
		res.Reports = append(res.Reports, vr)

		if err := reverifyCatalog(st, cat, report); err != nil {
			return VerifyStoredResult{}, fmt.Errorf("verify --stored: updating catalog %d: %w", cat.ID, err)
		}
		if report.Verdict == ard.VerdictInvalid {
			res.Invalid++
		}
	}
	res.ReVerified = len(catalogs)

	return res, nil
}

// reverifyCatalog persists a fresh verification report for an
// already-stored catalog: updates verification_status and replaces its
// verification_checks rows (catalog-level and per-entry, matched by URN).
func reverifyCatalog(st *store.Store, cat store.Catalog, report ard.Report) error {
	return st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&store.Catalog{}).Where("id = ?", cat.ID).
			Update("verification_status", report.Verdict).Error; err != nil {
			return err
		}

		if err := tx.Where("subject_type = ? AND subject_id = ?", store.SubjectTypeCatalog, cat.ID).
			Delete(&store.VerificationCheck{}).Error; err != nil {
			return err
		}

		entryIDByURN := make(map[string]uint, len(cat.Entries))
		var entryIDs []uint
		for _, e := range cat.Entries {
			entryIDByURN[e.URN] = e.ID
			entryIDs = append(entryIDs, e.ID)
		}
		if len(entryIDs) > 0 {
			if err := tx.Where("subject_type = ? AND subject_id IN ?", store.SubjectTypeEntry, entryIDs).
				Delete(&store.VerificationCheck{}).Error; err != nil {
				return err
			}
		}

		now := time.Now()
		for _, c := range report.Checks {
			row := &store.VerificationCheck{
				SubjectType: store.SubjectTypeCatalog,
				SubjectID:   cat.ID,
				CheckID:     c.CheckID,
				Severity:    c.Severity,
				Passed:      c.Passed,
				Message:     c.Message,
				SpecRef:     c.SpecRef,
				CheckedAt:   now,
			}
			if c.Subject != "" && c.Subject != ard.SubjectCatalog {
				if entryID, ok := entryIDByURN[c.Subject]; ok {
					row.SubjectType = store.SubjectTypeEntry
					row.SubjectID = entryID
				}
			}
			if err := tx.Create(row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
