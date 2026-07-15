package jsonout

import (
	"context"
	"time"

	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/probe"
	"github.com/helgesverre/ardvark/internal/store"
)

// ProbeAttempt is one probe method's outcome for a host, the JSON-facing
// shape of a probe.Result.
type ProbeAttempt struct {
	Method      string   `json:"method"`
	URL         string   `json:"url"`
	HTTPStatus  int      `json:"http_status,omitempty"`
	ContentType string   `json:"content_type,omitempty"`
	Outcome     string   `json:"outcome"`
	ErrorDetail string   `json:"error_detail,omitempty"`
	CatalogURLs []string `json:"catalog_urls,omitempty"`
}

// HostProbe groups the probe attempts for a single host. Error is set when
// the host could not be probed at all (e.g. its domain row failed to
// upsert), in which case Results is empty.
type HostProbe struct {
	Host    string         `json:"host"`
	Error   string         `json:"error,omitempty"`
	Results []ProbeAttempt `json:"results,omitempty"`
}

// ProbeSummary is the rolled-up outcome count across all probe attempts.
type ProbeSummary struct {
	Hits   int `json:"hits"`
	Misses int `json:"misses"`
	Errors int `json:"errors"`
}

// ProbeReport is the full result of probing a set of hosts.
type ProbeReport struct {
	Hosts   []HostProbe  `json:"hosts"`
	Summary ProbeSummary `json:"summary"`
}

// ProbeCallbacks surfaces live probe progress to the CLI. Either field may
// be nil.
type ProbeCallbacks struct {
	// Result is invoked with each per-method probe result as it completes.
	Result func(host string, r probe.Result)
	// Errorf is invoked with recoverable persistence errors (domain upsert
	// and probe recording failures).
	Errorf func(format string, args ...any)
}

// ProbeHosts probes each host directly for ARD documents (well-known path
// and robots.txt Agentmap), recording every attempt as a probes row, and
// returns the per-host per-method results plus summary counts.
func ProbeHosts(ctx context.Context, fc *fetch.Client, st *store.Store, hosts []string, cb ProbeCallbacks) ProbeReport {
	rep := ProbeReport{Hosts: []HostProbe{}}

	for _, host := range hosts {
		domain, err := st.UpsertDomain(host, store.DiscoverySourceSeed)
		if err != nil {
			cb.errorf("probe: failed to upsert domain %q: %v", host, err)
			rep.Summary.Errors++
			rep.Hosts = append(rep.Hosts, HostProbe{Host: host, Error: err.Error()})
			continue
		}

		hp := HostProbe{Host: host}
		for _, r := range probe.Probe(ctx, fc, host) {
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
				cb.errorf("probe: failed to record probe for %q: %v", host, err)
			}

			switch r.Outcome {
			case probe.OutcomeHit:
				rep.Summary.Hits++
			case probe.OutcomeMiss:
				rep.Summary.Misses++
			case probe.OutcomeError:
				rep.Summary.Errors++
			}
			if cb.Result != nil {
				cb.Result(host, r)
			}
			hp.Results = append(hp.Results, ProbeAttempt{
				Method:      r.Method,
				URL:         r.URL,
				HTTPStatus:  r.HTTPStatus,
				ContentType: r.ContentType,
				Outcome:     r.Outcome,
				ErrorDetail: r.ErrorDetail,
				CatalogURLs: r.CatalogURLs,
			})
		}
		rep.Hosts = append(rep.Hosts, hp)
	}

	return rep
}

// errorf invokes the Errorf callback when set.
func (cb ProbeCallbacks) errorf(format string, args ...any) {
	if cb.Errorf != nil {
		cb.Errorf(format, args...)
	}
}
