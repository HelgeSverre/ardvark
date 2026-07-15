package main

import (
	"testing"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/probe"
	"github.com/helgesverre/ardvark/internal/ui"
)

func TestProbeRow(t *testing.T) {
	tests := []struct {
		name       string
		ev         crawler.ProbeEvent
		wantStatus ui.Status
		wantResult string
		wantExtra  string
	}{
		{
			name:       "hit valid",
			ev:         crawler.ProbeEvent{Outcome: probe.OutcomeHit, Verdict: ard.VerdictValid, Detail: "14 entries"},
			wantStatus: ui.StatusHit,
			wantResult: "catalog valid",
			wantExtra:  "14 entries",
		},
		{
			name:       "hit with warnings",
			ev:         crawler.ProbeEvent{Outcome: probe.OutcomeHit, Verdict: ard.VerdictValidWithWarnings, Detail: "queries.count"},
			wantStatus: ui.StatusWarnHit,
			wantResult: "valid_with_warnings",
			wantExtra:  "queries.count",
		},
		{
			name:       "hit invalid",
			ev:         crawler.ProbeEvent{Outcome: probe.OutcomeHit, Verdict: ard.VerdictInvalid, Detail: "urn.format ×3"},
			wantStatus: ui.StatusInvalid,
			wantResult: "invalid",
			wantExtra:  "urn.format ×3",
		},
		{
			name:       "miss",
			ev:         crawler.ProbeEvent{Outcome: probe.OutcomeMiss, Detail: "404"},
			wantStatus: ui.StatusMiss,
			wantResult: "404",
			wantExtra:  "",
		},
		{
			name:       "error",
			ev:         crawler.ProbeEvent{Outcome: probe.OutcomeError, Detail: "tls handshake failed"},
			wantStatus: ui.StatusError,
			wantResult: "tls handshake failed",
			wantExtra:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, result, extra := probeRow(tt.ev)
			if status != tt.wantStatus {
				t.Errorf("status = %v, want %v", status, tt.wantStatus)
			}
			if result != tt.wantResult {
				t.Errorf("result = %q, want %q", result, tt.wantResult)
			}
			if extra != tt.wantExtra {
				t.Errorf("extra = %q, want %q", extra, tt.wantExtra)
			}
		})
	}
}
