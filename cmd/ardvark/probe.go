package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/probe"
	"github.com/helgesverre/ardvark/internal/store"
	"github.com/helgesverre/ardvark/internal/ui"
)

var probeCmd = &cobra.Command{
	Use:   "probe <host>...",
	Short: "Probe host(s) directly for ARD documents, without HTML spidering",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runProbe,
}

func init() {
	rootCmd.AddCommand(probeCmd)
}

func runProbe(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	fc := newFetchClient(cfg)
	p := printer(cmd)

	var hits, misses, errs int

	for _, host := range args {
		domain, err := st.UpsertDomain(host, store.DiscoverySourceSeed)
		if err != nil {
			p.Errorf("probe: failed to upsert domain %q: %v", host, err)
			errs++
			continue
		}

		results := probe.Probe(cmd.Context(), fc, host)
		for _, r := range results {
			if err := st.RecordProbe(&store.Probe{
				DomainID:    domain.ID,
				Method:      r.Method,
				URL:         r.URL,
				HTTPStatus:  r.HTTPStatus,
				ContentType: r.ContentType,
				Outcome:     r.Outcome,
				ErrorDetail: r.ErrorDetail,
				ProbedAt:    time.Now(),
			}); err != nil {
				p.Errorf("probe: failed to record probe for %q: %v", host, err)
			}

			status, extra := probeRowStatus(r)
			switch r.Outcome {
			case probe.OutcomeHit:
				hits++
			case probe.OutcomeMiss:
				misses++
			case probe.OutcomeError:
				errs++
			}
			p.Row(status, host, r.Method, r.Outcome, extra)
		}
	}

	p.Summary("probe complete: ",
		fmt.Sprintf("%d hits", hits),
		fmt.Sprintf("%d misses", misses),
		fmt.Sprintf("%d errors", errs),
	)
	return nil
}

// probeRowStatus maps a probe.Result to a ui.Status and a detail string for
// display.
func probeRowStatus(r probe.Result) (ui.Status, string) {
	switch r.Outcome {
	case probe.OutcomeHit:
		return ui.StatusHit, fmt.Sprintf("%d %s", r.HTTPStatus, r.ContentType)
	case probe.OutcomeError:
		return ui.StatusError, r.ErrorDetail
	default:
		if r.HTTPStatus != 0 {
			return ui.StatusMiss, fmt.Sprintf("%d", r.HTTPStatus)
		}
		return ui.StatusMiss, ""
	}
}
