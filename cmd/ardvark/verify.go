package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/jsonout"
	"github.com/helgesverre/ardvark/internal/ui"
)

var verifyStored bool
var verifyStrict bool

var verifyCmd = &cobra.Command{
	Use:   "verify <path|url>",
	Short: "Verify a catalog document (local file, remote URL, or all stored catalogs) against the ARD spec",
	Long: "verify runs the ARD verification pipeline (JSON Schema + semantic checks) against a single " +
		"local file or remote URL and prints the check report, exiting 1 if the verdict is invalid. " +
		"By default this is ardvark's lenient verification, which accepts a few deliberate deviations " +
		"from the published spec schema (documented in internal/ard/schema/PROVENANCE.md); --strict " +
		"validates against the exact published spec schema instead, with format assertions (uri, " +
		"date-time, ...) enforced as errors rather than warnings. With --stored, every catalog already " +
		"in the database is re-verified instead (useful after a spec/schema update); the stored verdict " +
		"and checks are updated in place.",
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
	verifyCmd.Flags().BoolVar(&verifyStrict, "strict", false, "validate against the exact published spec schema, with format assertions as errors, instead of ardvark's default lenient verification")
	addJSONFlag(verifyCmd)
	rootCmd.AddCommand(verifyCmd)
}

func runVerify(cmd *cobra.Command, args []string) error {
	if verifyStored {
		return runVerifyStored(cmd)
	}
	return runVerifyOne(cmd, args[0])
}

// runVerifyOne verifies a single local file or remote URL and prints the
// check report (or, with --json, the full typed report). It exits with a
// non-nil error (causing os.Exit(1) via Execute) when the verdict is
// invalid — in JSON mode too.
func runVerifyOne(cmd *cobra.Command, target string) error {
	verifyFn := jsonout.VerifyTarget
	if verifyStrict {
		verifyFn = jsonout.VerifyTargetStrict
	}
	report, err := verifyFn(cmd.Context(), target)
	if err != nil {
		return err
	}

	if jsonOut {
		if err := printJSON(cmd, report); err != nil {
			return err
		}
	} else {
		printReport(printer(cmd), report)
	}

	if report.Verdict == ard.VerdictInvalid {
		return fmt.Errorf("verify: %s is invalid", target)
	}
	return nil
}

// printReport prints one catalog's full check report and rolled-up verdict.
func printReport(p *ui.Printer, report jsonout.VerifyReport) {
	p.Header(report.Source)
	for _, c := range report.Checks {
		detail := c.Message
		if c.Subject != "" && c.Subject != ard.SubjectCatalog {
			detail = c.Subject + " — " + c.Message
		}
		p.Check(c.Passed, c.Severity == ard.SeverityWarning, c.ID, detail)
	}
	p.Verdict(report.Verdict)
}

// runVerifyStored re-runs verification against every catalog currently
// stored in the database, updating each catalog's verification_status and
// replacing its verification_checks rows with the fresh results.
func runVerifyStored(cmd *cobra.Command) error {
	_, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	p := printer(cmd)

	var onReport func(jsonout.VerifyReport)
	if !jsonOut {
		onReport = func(r jsonout.VerifyReport) { printReport(p, r) }
	}

	res, err := jsonout.VerifyStored(st, verifyStrict, onReport)
	if err != nil {
		return err
	}

	if jsonOut {
		if err := printJSON(cmd, res); err != nil {
			return err
		}
	} else {
		p.Summary("verify --stored complete",
			fmt.Sprintf("%d catalogs re-verified", res.ReVerified),
			fmt.Sprintf("%d invalid", res.Invalid),
		)
	}

	if res.Invalid > 0 {
		return fmt.Errorf("verify --stored: %d catalog(s) invalid", res.Invalid)
	}
	return nil
}
