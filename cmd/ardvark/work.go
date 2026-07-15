package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/frontier"
	"github.com/helgesverre/ardvark/internal/jsonout"
	"github.com/helgesverre/ardvark/internal/ui"
)

var (
	workForce  bool
	workWorker string
)

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Drain the shared frontier cooperatively, seeding nothing",
	Long: "work runs the crawl engine against the persistent frontier without seeding it: " +
		"multiple work processes may point at the same mysql/postgres database, and each " +
		"takes a disjoint share of hosts (see --worker), so they crawl the frontier " +
		"cooperatively instead of duplicating each other's requests. sqlite is " +
		"single-process (see storage.driver in ardvark.json), so running more than one " +
		"work process against a sqlite database is not supported. Use \"ardvark seed\" or " +
		"\"ardvark crawl\" to add work to the frontier first.",
	RunE: runWork,
}

func init() {
	workCmd.Flags().BoolVar(&workForce, "force", false, "bypass the host_probe freshness window (re-probe hosts probed recently)")
	workCmd.Flags().StringVar(&workWorker, "worker", "", "this process's share of the frontier as \"i/n\" (0-based index / total worker count), overriding config crawler.worker")
	addJSONFlag(workCmd)
	rootCmd.AddCommand(workCmd)
}

// runWork opens the app, resolves this process's worker shard, and runs the
// crawl engine with no seeds until the frontier is globally empty. See
// jsonout.Crawl, which work reuses with an empty seed list rather than
// forking the seeding+engine composition.
func runWork(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	if workWorker != "" {
		index, count, perr := parseWorkerFlag(workWorker)
		if perr != nil {
			return perr
		}
		cfg.Crawler.Worker.Index = index
		cfg.Crawler.Worker.Count = count
	}

	// An empty frontier is a friendly no-op, not an error: a work process
	// is meant to be started ahead of (or independent from) whatever seeds
	// the frontier, so finding nothing to do yet is normal, expected
	// behavior rather than a misuse of the command. Counts() reflects the
	// whole shared frontier regardless of worker sharding (see its doc
	// comment), so a plain frontier.Frontier with no shard option is
	// enough to answer "is there anything to do at all" without running
	// the engine (which would otherwise still create a CrawlRun row and
	// exit immediately with all-zero counts).
	pending, inFlight, err := frontier.New(st.DB).Counts()
	if err != nil {
		return err
	}
	if pending == 0 && inFlight == 0 {
		if jsonOut {
			return printJSON(cmd, jsonout.CrawlResult{})
		}
		printer(cmd).Mutedf("frontier is empty; nothing to work on (seed it first with \"ardvark seed\" or \"ardvark crawl\")")
		return nil
	}

	// In JSON mode the live per-host rows and progress notes are
	// suppressed; only the final run summary object is emitted.
	var cb jsonout.CrawlCallbacks
	if !jsonOut {
		p := printer(cmd)
		// The engine's worker pool fires OnProbe from multiple goroutines,
		// and ui.Printer is not goroutine-safe, so serialize the row writes.
		var rowMu sync.Mutex
		cb = jsonout.CrawlCallbacks{
			OnProbe: func(ev crawler.ProbeEvent) {
				rowMu.Lock()
				defer rowMu.Unlock()
				status, result, extra := probeRow(ev)
				p.Row(status, ev.Host, ev.Method, result, extra)
			},
		}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := jsonout.Crawl(ctx, cfg, st, nil, workForce, cb)
	if err != nil {
		return err
	}

	if jsonOut {
		return printJSON(cmd, res)
	}

	printer(cmd).Summary("run complete",
		ui.Count(res.PagesFetched, "page fetched", "pages fetched"),
		ui.Count(res.HostsProbed, "host probed", "hosts probed"),
		ui.Count(res.CatalogsFound, "catalog found", "catalogs found"),
		fmt.Sprintf("%d valid", res.CatalogsValid),
		ui.Count(res.Errors, "error", "errors"),
	)
	return nil
}

// parseWorkerFlag parses --worker "i/n": two non-negative integers
// separated by a single slash, with 0 <= i < n. Strict parsing catches
// operator typos early (e.g. a stray space or a missing worker) rather
// than letting a misconfigured process silently dequeue nothing forever.
func parseWorkerFlag(s string) (index, count int, err error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("work: --worker %q: want \"i/n\" (e.g. \"2/4\")", s)
	}
	index, ierr := strconv.Atoi(strings.TrimSpace(parts[0]))
	if ierr != nil {
		return 0, 0, fmt.Errorf("work: --worker %q: invalid index: %w", s, ierr)
	}
	count, cerr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if cerr != nil {
		return 0, 0, fmt.Errorf("work: --worker %q: invalid count: %w", s, cerr)
	}
	if count < 1 {
		return 0, 0, fmt.Errorf("work: --worker %q: count must be >= 1", s)
	}
	if index < 0 || index >= count {
		return 0, 0, fmt.Errorf("work: --worker %q: index must satisfy 0 <= index < count", s)
	}
	return index, count, nil
}
