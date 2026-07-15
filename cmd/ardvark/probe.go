package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/jsonout"
	"github.com/helgesverre/ardvark/internal/probe"
	"github.com/helgesverre/ardvark/internal/ui"
)

var probeCmd = &cobra.Command{
	Use:   "probe <host>...",
	Short: "Probe host(s) directly for ARD documents, without HTML spidering",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runProbe,
}

func init() {
	addJSONFlag(probeCmd)
	rootCmd.AddCommand(probeCmd)
}

func runProbe(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	fc := newFetchClient(cfg)

	if jsonOut {
		// Live rows are suppressed; persistence errors still go to stderr.
		report := jsonout.ProbeHosts(cmd.Context(), fc, st, args, jsonout.ProbeCallbacks{
			Errorf: func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) },
		})
		return printJSON(cmd, report)
	}

	p := printer(cmd)
	report := jsonout.ProbeHosts(cmd.Context(), fc, st, args, jsonout.ProbeCallbacks{
		Result: func(host string, r probe.Result) {
			status, result, extra := probeRowStatus(r)
			p.Row(status, host, r.Method, result, extra)
		},
		Errorf: p.Errorf,
	})

	p.Summary("probe complete",
		fmt.Sprintf("%d hits", report.Summary.Hits),
		fmt.Sprintf("%d misses", report.Summary.Misses),
		fmt.Sprintf("%d errors", report.Summary.Errors),
	)
	return nil
}

// probeRowStatus maps a probe.Result to a ui.Status, a result label, and an
// optional detail, matching the column semantics of the crawl command's rows
// (the result column carries meaning; it does not repeat the status word).
func probeRowStatus(r probe.Result) (status ui.Status, result, extra string) {
	switch r.Outcome {
	case probe.OutcomeHit:
		return ui.StatusHit, "found", strings.TrimSpace(fmt.Sprintf("%d %s", r.HTTPStatus, r.ContentType))
	case probe.OutcomeError:
		return ui.StatusError, "error", r.ErrorDetail
	default:
		return ui.StatusMiss, probeMissReason(r), ""
	}
}

// probeMissReason gives the human reason for a miss: the recorded detail
// (e.g. "no Agentmap directive", "non-JSON response") when present, else the
// HTTP status, else a generic fallback.
func probeMissReason(r probe.Result) string {
	switch {
	case r.ErrorDetail != "":
		return r.ErrorDetail
	case r.HTTPStatus != 0:
		return strconv.Itoa(r.HTTPStatus)
	default:
		return "not found"
	}
}
