package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gorm.io/gorm"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/store"
	"github.com/helgesverre/ardvark/internal/ui"
)

var verifyStored bool

var verifyCmd = &cobra.Command{
	Use:   "verify <path|url>",
	Short: "Verify a catalog document (local file, remote URL, or all stored catalogs) against the ARD spec",
	Long: "verify runs the ARD verification pipeline (JSON Schema + semantic checks) against a single " +
		"local file or remote URL and prints the check report, exiting 1 if the verdict is invalid. " +
		"With --stored, every catalog already in the database is re-verified instead (useful after a " +
		"spec/schema update); the stored verdict and checks are updated in place.",
	Args: func(cmd *cobra.Command, args []string) error {
		if verifyStored {
			return cobra.MaximumNArgs(0)(cmd, args)
		}
		return cobra.ExactArgs(1)(cmd, args)
	},
	RunE: runVerify,
}

func init() {
	verifyCmd.Flags().BoolVar(&verifyStored, "stored", false, "re-verify every catalog stored in the database instead of a single document")
	rootCmd.AddCommand(verifyCmd)
}

func runVerify(cmd *cobra.Command, args []string) error {
	if verifyStored {
		return runVerifyStored(cmd)
	}
	return runVerifyOne(cmd, args[0])
}

// runVerifyOne verifies a single local file or remote URL and prints the
// check report. It exits with a non-nil error (causing os.Exit(1) via
// Execute) when the verdict is invalid.
func runVerifyOne(cmd *cobra.Command, target string) error {
	raw, servingDomain, err := fetchVerifyTarget(cmd, target)
	if err != nil {
		return err
	}

	report := ard.Verify(raw, servingDomain)
	printReport(printer(cmd), target, report)

	if report.Verdict == ard.VerdictInvalid {
		return fmt.Errorf("verify: %s is invalid", target)
	}
	return nil
}

// fetchVerifyTarget loads the raw catalog bytes for target, either via HTTP
// (if it looks like a URL) or from the local filesystem, and derives the
// serving domain used by the urn.publisher_matches check.
func fetchVerifyTarget(cmd *cobra.Command, target string) (raw []byte, servingDomain string, err error) {
	if strings.Contains(target, "://") {
		u, perr := url.Parse(target)
		if perr != nil {
			return nil, "", fmt.Errorf("verify: invalid URL %q: %w", target, perr)
		}
		client := &http.Client{Timeout: 15 * time.Second}
		req, rerr := http.NewRequestWithContext(cmd.Context(), http.MethodGet, target, nil)
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

// printReport prints one catalog's full check report and rolled-up verdict.
func printReport(p *ui.Printer, label string, report ard.Report) {
	p.Header(label)
	for _, c := range report.Checks {
		detail := c.Message
		if c.Subject != "" && c.Subject != "catalog" {
			detail = c.Subject + " — " + c.Message
		}
		p.Check(c.Passed, c.Severity == ard.SeverityWarning, c.CheckID, detail)
	}
	p.Verdict(report.Verdict)
}

// runVerifyStored re-runs verification against every catalog currently
// stored in the database, updating each catalog's verification_status and
// replacing its verification_checks rows with the fresh results.
func runVerifyStored(cmd *cobra.Command) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	var catalogs []store.Catalog
	if err := st.DB.Preload("Entries").Find(&catalogs).Error; err != nil {
		return fmt.Errorf("verify --stored: loading catalogs: %w", err)
	}

	p := printer(cmd)

	var invalidCount int
	for _, cat := range catalogs {
		var domain store.Domain
		if err := st.DB.First(&domain, cat.DomainID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			return fmt.Errorf("verify --stored: loading domain %d: %w", cat.DomainID, err)
		}

		report := ard.Verify([]byte(cat.RawJSON), domain.Host)
		printReport(p, fmt.Sprintf("%s (%s)", cat.SourceURL, domain.Host), report)

		if err := reverifyCatalog(st, cat, report); err != nil {
			return fmt.Errorf("verify --stored: updating catalog %d: %w", cat.ID, err)
		}
		if report.Verdict == ard.VerdictInvalid {
			invalidCount++
		}
	}

	p.Summary("verify --stored complete: ",
		fmt.Sprintf("%d catalogs re-verified", len(catalogs)),
		fmt.Sprintf("%d invalid", invalidCount),
	)

	if invalidCount > 0 {
		return fmt.Errorf("verify --stored: %d catalog(s) invalid", invalidCount)
	}
	return nil
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
			if c.Subject != "" && c.Subject != "catalog" {
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
