package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/store"
)

// discardWriter is an io.Writer that discards everything, used to silence
// the slog logger the test engines need but don't care about.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestParseWorkerFlag(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantIndex int
		wantCount int
		wantErr   bool
	}{
		{name: "valid", in: "2/4", wantIndex: 2, wantCount: 4},
		{name: "valid zero index", in: "0/1", wantIndex: 0, wantCount: 1},
		{name: "spaces tolerated", in: " 1 / 3 ", wantIndex: 1, wantCount: 3},
		{name: "missing slash", in: "2", wantErr: true},
		{name: "too many parts", in: "1/2/3", wantErr: true},
		{name: "non-numeric index", in: "a/4", wantErr: true},
		{name: "non-numeric count", in: "2/b", wantErr: true},
		{name: "index equals count", in: "4/4", wantErr: true},
		{name: "index greater than count", in: "5/4", wantErr: true},
		{name: "negative index", in: "-1/4", wantErr: true},
		{name: "zero count", in: "0/0", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "index within shard space, count beyond it", in: "9000/100000", wantErr: true},
		{name: "count one past shard space ceiling", in: "8192/8193", wantErr: true},
		{name: "count at shard space ceiling, max valid index", in: "8191/8192", wantIndex: 8191, wantCount: 8192},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index, count, err := parseWorkerFlag(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseWorkerFlag(%q): want error, got index=%d count=%d", tt.in, index, count)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWorkerFlag(%q): unexpected error: %v", tt.in, err)
			}
			if index != tt.wantIndex || count != tt.wantCount {
				t.Fatalf("parseWorkerFlag(%q) = (%d, %d), want (%d, %d)", tt.in, index, count, tt.wantIndex, tt.wantCount)
			}
		})
	}
}

// `ardvark work` against an empty frontier is a friendly no-op: exit 0 with
// a note, not an error, since a work process may legitimately be started
// before anything has seeded the frontier.
func TestWorkEmptyFrontierExitsCleanly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ardvark.json")
	cfg := fmt.Sprintf(`{"storage":{"driver":"sqlite","dsn":%q},"log":{"file":%q}}`,
		filepath.Join(dir, "test.db"), filepath.Join(dir, "test.jsonl"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	out, err := executeRoot(t, "--config", cfgPath, "work")
	if err != nil {
		t.Fatalf("work on empty frontier: want nil error, got %v", err)
	}
	if !strings.Contains(out, "frontier is empty") {
		t.Errorf("work on empty frontier: want friendly note in output, got %q", out)
	}
}

// TestTwoWorkersDrainDisjointShards is the end-to-end proof that the
// --worker "i/n" model actually works: two crawler.Engine instances,
// configured as worker 0 of 2 and worker 1 of 2 (mirroring exactly what
// runWork wires up via cfg.Crawler.Worker and frontier.WithWorkerShard),
// share one sqlite-backed store.Store and run concurrently in this single
// test process. sqlite itself is single-process (store.Open caps
// MaxOpenConns at 1), but that just serializes the two engines' queries
// through one connection — it does not stop two engines in one process
// from cooperating over the same frontier, which is exactly what this test
// exercises.
func TestTwoWorkersDrainDisjointShards(t *testing.T) {
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test-only, talking to our own httptest.NewTLSServer instances

	const numHosts = 8
	hosts := make([]string, 0, numHosts)
	for i := 0; i < numHosts; i++ {
		mux := http.NewServeMux()
		// No handlers registered: well-known and robots.txt both 404,
		// which handleHostProbe treats as a clean miss (no retries).
		srv := httptest.NewTLSServer(mux)
		t.Cleanup(srv.Close)
		hosts = append(hosts, strings.TrimPrefix(srv.URL, "https://"))
	}

	dsn := filepath.Join(t.TempDir(), "ardvark.db")
	st, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	baseCfg := config.Defaults()
	baseCfg.Crawler.Concurrency = 4
	baseCfg.Crawler.PerHostRequestsPerSecond = 1000
	baseCfg.Crawler.RequestTimeoutSeconds = 5

	fc := fetch.New(baseCfg.Crawler, fetch.WithTransport(transport))
	logger := slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError + 1}))

	// Seed every host as a host_probe item through an unsharded frontier
	// view: HostShard is derived and stamped at enqueue time regardless of
	// which Frontier instance (sharded or not) does the enqueuing — see
	// frontier.Frontier.Enqueue's doc comment — so any engine can seed on
	// behalf of the eventual workers.
	seedEngine := crawler.New(baseCfg, st, frontier.New(st.DB), fc, logger, crawler.Options{})
	for _, host := range hosts {
		if _, err := seedEngine.EnqueueSeedHost(host, store.DiscoverySourceSeed); err != nil {
			t.Fatalf("EnqueueSeedHost(%s): %v", host, err)
		}
	}

	// Each host_probe fires OnProbe once per probe method (well-known and
	// robots_agentmap), so track *which worker* touched a host rather than
	// how many events it produced — the property under test is that each
	// host is dequeued by exactly one of the two workers, never both.
	var mu sync.Mutex
	seenBy := make(map[string]map[int]bool)

	newWorkerEngine := func(index int) *crawler.Engine {
		cfg := baseCfg
		cfg.Crawler.Worker = config.WorkerConfig{Index: index, Count: 2}
		fr := frontier.New(st.DB, frontier.WithWorkerShard(index, 2))
		return crawler.New(cfg, st, fr, fc, logger, crawler.Options{
			MaxAttempts: 2,
			OnProbe: func(ev crawler.ProbeEvent) {
				mu.Lock()
				defer mu.Unlock()
				if seenBy[ev.Host] == nil {
					seenBy[ev.Host] = make(map[int]bool)
				}
				seenBy[ev.Host][index] = true
			},
		})
	}

	eng0 := newWorkerEngine(0)
	eng1 := newWorkerEngine(1)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = eng0.Run(ctx) }()
	go func() { defer wg.Done(); errs[1] = eng1.Run(ctx) }()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d Run(): %v", i, err)
		}
	}

	pending, err := frontier.New(st.DB).PendingCount()
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Errorf("PendingCount() after both workers ran = %d, want 0 (frontier fully drained)", pending)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seenBy) != numHosts {
		t.Fatalf("probed %d distinct hosts, want %d: %v", len(seenBy), numHosts, seenBy)
	}
	for host, workers := range seenBy {
		if len(workers) != 1 {
			t.Errorf("host %s was probed by %d distinct workers %v, want exactly 1 (no duplicate/overlapping work)", host, len(workers), workers)
		}
	}
}
