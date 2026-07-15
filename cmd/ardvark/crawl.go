package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/probe"
	"github.com/helgesverre/ardvark/internal/store"
	"github.com/helgesverre/ardvark/internal/ui"
)

var (
	crawlListFile string
	crawlForce    bool
)

var crawlCmd = &cobra.Command{
	Use:   "crawl [url|domain]...",
	Short: "Seed the frontier and drain it with the crawl engine",
	Long: "crawl seeds the persistent frontier from the given URLs and/or bare domains " +
		"(and/or a --list file), then runs the crawler until the frontier is empty. " +
		"Pending work from prior runs is resumed automatically.",
	RunE: runCrawl,
}

func init() {
	crawlCmd.Flags().StringVar(&crawlListFile, "list", "", "path to a file of newline-separated URLs/domains to seed")
	crawlCmd.Flags().BoolVar(&crawlForce, "force", false, "bypass the host_probe freshness window (re-probe hosts probed recently)")
	rootCmd.AddCommand(crawlCmd)
}

func runCrawl(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	logger, err := newLogger(cfg)
	if err != nil {
		return err
	}

	seeds, err := collectSeeds(args, crawlListFile)
	if err != nil {
		return err
	}

	configSnapshot, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("crawl: marshalling config snapshot: %w", err)
	}

	run, err := st.CreateRun(string(configSnapshot))
	if err != nil {
		return err
	}

	p := printer(cmd)

	fr := frontier.New(st.DB)
	fc := newFetchClient(cfg)
	// The engine's worker pool fires OnProbe from multiple goroutines, and
	// ui.Printer is not goroutine-safe, so serialize the row writes.
	var rowMu sync.Mutex
	eng := crawler.New(cfg, st, fr, fc, logger, crawler.Options{
		RunID:       run.ID,
		Force:       crawlForce,
		BackoffBase: time.Second,
		OnProbe: func(ev crawler.ProbeEvent) {
			rowMu.Lock()
			defer rowMu.Unlock()
			status, result, extra := probeRow(ev)
			p.Row(status, ev.Host, ev.Method, result, extra)
		},
	})

	seeded := 0
	for _, s := range seeds {
		added, err := seedOne(eng, s)
		if err != nil {
			p.Errorf("crawl: failed to seed %q: %v", s, err)
			continue
		}
		if added {
			seeded++
		}
	}
	p.Mutedf("seeded %d of %d requested seed(s)", seeded, len(seeds))

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := eng.Run(ctx); err != nil {
		return err
	}

	pagesFetched, hostsProbed, catalogsFound, catalogsValid, errCount, err := summarizeRun(st, run.ID, run.StartedAt)
	if err != nil {
		return err
	}
	if err := st.FinishRun(run.ID, pagesFetched, hostsProbed, catalogsFound, catalogsValid, errCount); err != nil {
		return err
	}

	p.Summary("run complete: ",
		fmt.Sprintf("%d pages fetched", pagesFetched),
		fmt.Sprintf("%d hosts probed", hostsProbed),
		fmt.Sprintf("%d catalogs found", catalogsFound),
		fmt.Sprintf("%d valid", catalogsValid),
		fmt.Sprintf("%d errors", errCount),
	)
	return nil
}

// probeRow maps a live crawler.ProbeEvent onto the status, result, and
// extra columns of a ui.Printer row, matching the canonical demo output:
//
//	hit   acme.com            well-known       catalog valid          14 entries
//	hit   tools.example.dev   robots_agentmap  valid_with_warnings    queries.count
//	miss  blog.someone.net    well-known       404
//	hit   broken.startup.ai   well-known       invalid                urn.format ×3
func probeRow(ev crawler.ProbeEvent) (status ui.Status, result, extra string) {
	switch ev.Outcome {
	case probe.OutcomeHit:
		switch ev.Verdict {
		case ard.VerdictValidWithWarnings:
			return ui.StatusWarnHit, ev.Verdict, ev.Detail
		case ard.VerdictInvalid:
			return ui.StatusInvalid, ev.Verdict, ev.Detail
		default:
			return ui.StatusHit, "catalog valid", ev.Detail
		}
	case probe.OutcomeMiss:
		return ui.StatusMiss, ev.Detail, ""
	default:
		return ui.StatusError, ev.Detail, ""
	}
}

// collectSeeds merges positional seed arguments with lines from a --list
// file, if given.
func collectSeeds(args []string, listFile string) ([]string, error) {
	seeds := append([]string{}, args...)

	if listFile == "" {
		return seeds, nil
	}

	f, err := os.Open(listFile)
	if err != nil {
		return nil, fmt.Errorf("crawl: opening --list file %s: %w", listFile, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		seeds = append(seeds, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("crawl: reading --list file %s: %w", listFile, err)
	}

	return seeds, nil
}

// seedOne enqueues a single seed: a URL (containing "://") is seeded as a
// page_fetch at depth 0; a bare domain is seeded as a host_probe at depth 0.
func seedOne(eng *crawler.Engine, seed string) (bool, error) {
	if strings.Contains(seed, "://") {
		return eng.EnqueueSeedURL(seed)
	}
	return eng.EnqueueSeedHost(seed, store.DiscoverySourceSeed)
}

// summarizeRun computes crawl_run summary counters for FinishRun. Pages
// fetched and hosts probed are counted from completed frontier items
// belonging to the run; catalogs found/valid are counted from catalog rows
// fetched since the run started (catalogs have no run_id column, so a time
// window is used — accurate for a single crawl run, an approximation if
// runs overlap). Errors are frontier items that exhausted their retry
// budget.
func summarizeRun(st *store.Store, runID uint, startedAt time.Time) (pagesFetched, hostsProbed, catalogsFound, catalogsValid, errCount int, err error) {
	var n int64

	if err = st.DB.Model(&store.FrontierItem{}).
		Where("run_id = ? AND kind = ? AND status = ?", runID, store.KindPageFetch, store.FrontierStatusDone).
		Count(&n).Error; err != nil {
		return
	}
	pagesFetched = int(n)

	if err = st.DB.Model(&store.FrontierItem{}).
		Where("run_id = ? AND kind = ? AND status = ?", runID, store.KindHostProbe, store.FrontierStatusDone).
		Count(&n).Error; err != nil {
		return
	}
	hostsProbed = int(n)

	if err = st.DB.Model(&store.Catalog{}).
		Where("fetched_at >= ?", startedAt).
		Count(&n).Error; err != nil {
		return
	}
	catalogsFound = int(n)

	if err = st.DB.Model(&store.Catalog{}).
		Where("fetched_at >= ? AND verification_status IN ?", startedAt, []string{
			store.VerificationStatusValid, store.VerificationStatusValidWithWarnings,
		}).
		Count(&n).Error; err != nil {
		return
	}
	catalogsValid = int(n)

	if err = st.DB.Model(&store.FrontierItem{}).
		Where("run_id = ? AND status = ?", runID, store.FrontierStatusFailed).
		Count(&n).Error; err != nil {
		return
	}
	errCount = int(n)

	return
}
