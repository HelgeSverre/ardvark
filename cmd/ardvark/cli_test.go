package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helgesverre/ardvark/internal/probe"
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

// executeRoot runs the root command with args, capturing stdout+stderr, and
// restores the flag-bound package state afterwards so tests don't leak into
// each other.
func executeRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		configPath = "./ardvark.json"
		colorMode = "auto"
		jsonOut = false
		verifyStored = false
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	var buf bytes.Buffer
	rootCmd.SetArgs(args)
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	err := rootCmd.Execute()
	return buf.String(), err
}

// `ardvark mcp --help` must work: the MCP subcommand is wired into the root
// command and documents the exposed tools.
func TestMCPHelp(t *testing.T) {
	out, err := executeRoot(t, "mcp", "--help")
	if err != nil {
		t.Fatalf("mcp --help: %v", err)
	}
	for _, want := range []string{"stdio", "ardvark_probe", "ardvark_stats"} {
		if !strings.Contains(out, want) {
			t.Errorf("mcp --help output missing %q:\n%s", want, out)
		}
	}
}

// `ardvark stats --json` must emit parseable JSON with the documented
// top-level sections.
func TestStatsJSONOutput(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ardvark.json")
	cfg := fmt.Sprintf(`{"storage":{"driver":"sqlite","dsn":%q},"log":{"file":%q}}`,
		filepath.Join(dir, "test.db"), filepath.Join(dir, "test.jsonl"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	out, err := executeRoot(t, "--config", cfgPath, "stats", "--json")
	if err != nil {
		t.Fatalf("stats --json: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("stats --json output is not valid JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"domains", "catalogs", "entries"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("stats --json output missing %q key:\n%s", key, out)
		}
	}
}

// --json must be rejected by commands that don't support it.
func TestJSONFlagRejectedWhereUnsupported(t *testing.T) {
	if _, err := executeRoot(t, "migrate", "--json"); err == nil {
		t.Fatal("migrate --json: want unknown-flag error, got nil")
	}
}
