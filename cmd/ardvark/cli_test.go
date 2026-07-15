package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/probe"
	"github.com/helgesverre/ardvark/internal/store"
	"github.com/helgesverre/ardvark/internal/ui"
)

func TestCollectSeeds(t *testing.T) {
	dir := t.TempDir()
	listPath := filepath.Join(dir, "seeds.txt")
	content := "example.com\n# comment\n\nhttps://foo.example/\n"
	if err := os.WriteFile(listPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing seed list: %v", err)
	}

	tests := []struct {
		name     string
		args     []string
		listFile string
		want     []string
	}{
		{
			name: "args only",
			args: []string{"a.com", "https://b.com"},
			want: []string{"a.com", "https://b.com"},
		},
		{
			name:     "list file only",
			listFile: listPath,
			want:     []string{"example.com", "https://foo.example/"},
		},
		{
			name:     "args and list merged, in order",
			args:     []string{"a.com"},
			listFile: listPath,
			want:     []string{"a.com", "example.com", "https://foo.example/"},
		},
		{
			name: "no args, no list",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := collectSeeds(tt.args, tt.listFile)
			if err != nil {
				t.Fatalf("collectSeeds() error = %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("collectSeeds() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("collectSeeds()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCollectSeedsMissingListFile(t *testing.T) {
	if _, err := collectSeeds(nil, "/does/not/exist.txt"); err == nil {
		t.Fatal("collectSeeds() with missing list file: want error, got nil")
	}
}

func TestProbeRowStatus(t *testing.T) {
	tests := []struct {
		name       string
		result     probe.Result
		wantStatus ui.Status
	}{
		{
			name:       "hit",
			result:     probe.Result{Outcome: probe.OutcomeHit, HTTPStatus: 200, ContentType: "application/json"},
			wantStatus: ui.StatusHit,
		},
		{
			name:       "miss with status",
			result:     probe.Result{Outcome: probe.OutcomeMiss, HTTPStatus: 404},
			wantStatus: ui.StatusMiss,
		},
		{
			name:       "miss with no status",
			result:     probe.Result{Outcome: probe.OutcomeMiss},
			wantStatus: ui.StatusMiss,
		},
		{
			name:       "error",
			result:     probe.Result{Outcome: probe.OutcomeError, ErrorDetail: "boom"},
			wantStatus: ui.StatusError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, _, _ := probeRowStatus(tt.result)
			if gotStatus != tt.wantStatus {
				t.Fatalf("probeRowStatus() status = %v, want %v", gotStatus, tt.wantStatus)
			}
		})
	}
}

func TestColorOptions(t *testing.T) {
	var buf bytes.Buffer
	// --color=always forces escape codes even for a non-TTY writer.
	ui.New(&buf, colorOptions("always")...).Errorf("boom")
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("--color=always: want escape codes, got %q", buf.String())
	}

	buf.Reset()
	ui.New(&buf, colorOptions("never")...).Errorf("boom")
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("--color=never: want plain output, got %q", buf.String())
	}

	// auto passes no options, leaving TTY/NO_COLOR/TERM detection in charge.
	if opts := colorOptions("auto"); opts != nil {
		t.Errorf("--color=auto: want nil options, got %d", len(opts))
	}
}

func TestWriteJSONLAndCSV(t *testing.T) {
	rows := []exportRow{
		{Host: "example.com", URN: "urn:air:example.com:skills:x", DisplayName: "X", MediaType: "application/ai-skill+json"},
		{Host: "other.net", URN: "urn:air:other.net:skills:y", DisplayName: "Y, with comma", MediaType: "application/ai-skill+json"},
	}

	var jsonlBuf bytes.Buffer
	if err := writeJSONL(&jsonlBuf, rows); err != nil {
		t.Fatalf("writeJSONL() error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(jsonlBuf.String(), "\n"), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("writeJSONL() produced %d lines, want %d", len(lines), len(rows))
	}
	if !strings.Contains(lines[0], "example.com") {
		t.Fatalf("writeJSONL() line 0 = %q, want to contain host", lines[0])
	}

	var csvBuf bytes.Buffer
	if err := writeCSV(&csvBuf, rows); err != nil {
		t.Fatalf("writeCSV() error = %v", err)
	}
	csvOut := csvBuf.String()
	if !strings.HasPrefix(csvOut, "host,catalog_source_url") {
		t.Fatalf("writeCSV() missing header, got %q", csvOut)
	}
	if !strings.Contains(csvOut, `"Y, with comma"`) {
		t.Fatalf("writeCSV() did not quote field containing a comma, got %q", csvOut)
	}
}

func TestGroupCount(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.UpsertDomain("a.com", store.DiscoverySourceSeed); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if _, err := st.UpsertDomain("b.com", store.DiscoverySourceSeed); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	dc, err := st.UpsertDomain("c.com", store.DiscoverySourceCTLog)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if err := st.RecordProbe(&store.Probe{DomainID: dc.ID, Method: store.ProbeMethodWellKnown, Outcome: store.ProbeOutcomeHit}); err != nil {
		t.Fatalf("RecordProbe: %v", err)
	}
	if err := st.UpdateDomainARDStatus(dc.ID, store.ARDStatusFoundValid); err != nil {
		t.Fatalf("UpdateDomainARDStatus: %v", err)
	}

	groups, err := groupCount(st, "domains", "ard_status")
	if err != nil {
		t.Fatalf("groupCount() error = %v", err)
	}

	counts := make(map[string]int64, len(groups))
	for _, g := range groups {
		counts[g.key] = g.count
	}

	if counts[store.ARDStatusUnprobed] != 2 {
		t.Errorf("unprobed count = %d, want 2", counts[store.ARDStatusUnprobed])
	}
	if counts[store.ARDStatusFoundValid] != 1 {
		t.Errorf("found_valid count = %d, want 1", counts[store.ARDStatusFoundValid])
	}
}

func TestSummarizeRun(t *testing.T) {
	st := newTestStore(t)

	run, err := st.CreateRun("{}")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := st.DB.Create(&store.FrontierItem{
		RunID: run.ID, Kind: store.KindPageFetch, URL: "https://a.com/", Host: "a.com",
		Status: store.FrontierStatusDone, DedupKey: "page_fetch:https://a.com/",
	}).Error; err != nil {
		t.Fatalf("seeding frontier item: %v", err)
	}
	if err := st.DB.Create(&store.FrontierItem{
		RunID: run.ID, Kind: store.KindHostProbe, Host: "a.com",
		Status: store.FrontierStatusDone, DedupKey: "host_probe:a.com",
	}).Error; err != nil {
		t.Fatalf("seeding frontier item: %v", err)
	}
	if err := st.DB.Create(&store.FrontierItem{
		RunID: run.ID, Kind: store.KindArtifactFetch, URL: "https://a.com/x", Host: "a.com",
		Status: store.FrontierStatusFailed, DedupKey: "artifact_fetch:https://a.com/x",
	}).Error; err != nil {
		t.Fatalf("seeding frontier item: %v", err)
	}

	domain, err := st.UpsertDomain("a.com", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if err := st.SaveCatalog(&store.Catalog{
		DomainID: domain.ID, SourceURL: "https://a.com/.well-known/ai-catalog.json",
		FetchedAt: time.Now(), VerificationStatus: store.VerificationStatusValid,
	}, nil, nil); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}

	pagesFetched, hostsProbed, catalogsFound, catalogsValid, errCount, err := summarizeRun(st, run.StartedAt.Add(-time.Minute))
	if err != nil {
		t.Fatalf("summarizeRun() error = %v", err)
	}
	if pagesFetched != 1 {
		t.Errorf("pagesFetched = %d, want 1", pagesFetched)
	}
	if hostsProbed != 1 {
		t.Errorf("hostsProbed = %d, want 1", hostsProbed)
	}
	if catalogsFound != 1 {
		t.Errorf("catalogsFound = %d, want 1", catalogsFound)
	}
	if catalogsValid != 1 {
		t.Errorf("catalogsValid = %d, want 1", catalogsValid)
	}
	if errCount != 1 {
		t.Errorf("errCount = %d, want 1", errCount)
	}
}

// newTestStore opens an isolated in-memory-like sqlite store (file-backed
// in a t.TempDir so concurrent connections within a single test see the
// same data) with the schema migrated.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "ardvark.db")
	st, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
