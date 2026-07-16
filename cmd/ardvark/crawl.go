package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/helgesverre/ardvark/internal/ard"
	"github.com/helgesverre/ardvark/internal/crawler"
	"github.com/helgesverre/ardvark/internal/jsonout"
	"github.com/helgesverre/ardvark/internal/probe"
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
		"Pending work from prior runs is resumed automatically. When a worker fleet is " +
		"configured (crawler.worker.count > 1), this process only dequeues its own " +
		"shard (crawler.worker.index) but still waits for the whole frontier to drain " +
		"before exiting, so a lone crawl run will seed every shard yet sit idle waiting " +
		"for peers on the shards it does not own — run \"ardvark work\" for the other " +
		"worker indices to drain them.",
	RunE: runCrawl,
}

func init() {
	crawlCmd.Flags().StringVar(&crawlListFile, "list", "", "path to a file of newline-separated URLs/domains to seed")
	crawlCmd.Flags().BoolVar(&crawlForce, "force", false, "bypass the host_probe freshness window (re-probe hosts probed recently)")
	addJSONFlag(crawlCmd)
	rootCmd.AddCommand(crawlCmd)
}

func runCrawl(cmd *cobra.Command, args []string) error {
	cfg, st, err := openApp()
	if err != nil {
		return err
	}
	defer st.Close()

	seeds, err := collectSeeds(args, crawlListFile)
	if err != nil {
		return err
	}

	// In JSON mode the live per-host rows and progress notes are suppressed;
	// only the final run summary object is emitted (seed failures still go
	// to stderr as plain text).
	var cb jsonout.CrawlCallbacks
	if jsonOut {
		cb = jsonout.CrawlCallbacks{
			SeedError: func(seed string, err error) {
				fmt.Fprintf(os.Stderr, "crawl: failed to seed %q: %v\n", seed, err)
			},
		}
	} else {
		p := printer(cmd)
		cb = crawlCallbacks(p)
		cb.SeedError = func(seed string, err error) {
			p.Errorf("crawl: failed to seed %q: %v", seed, err)
		}
		cb.Seeded = func(seeded, requested int) {
			p.Mutedf("seeded %d of %d requested seed(s)", seeded, requested)
		}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := jsonout.Crawl(ctx, cfg, st, seeds, crawlForce, cb)
	if err != nil {
		return err
	}

	if jsonOut {
		return printJSON(cmd, res)
	}

	printCrawlSummary(printer(cmd), res)
	return nil
}

// crawlCallbacks builds the jsonout.CrawlCallbacks shared by "crawl" and
// "work" for non-JSON output: both commands drive the same engine and print
// live per-host rows the same way, so the OnProbe wiring (including the
// mutex that serializes writes against ui.Printer, which is not
// goroutine-safe, against the engine's concurrent worker pool) lives here
// once rather than being copy-pasted per command. Callers that need
// additional callbacks (crawl's SeedError/Seeded) set them on the returned
// value.
func crawlCallbacks(p *ui.Printer) jsonout.CrawlCallbacks {
	var rowMu sync.Mutex
	return jsonout.CrawlCallbacks{
		OnProbe: func(ev crawler.ProbeEvent) {
			rowMu.Lock()
			defer rowMu.Unlock()
			status, result, extra := probeRow(ev)
			p.Row(status, ev.Host, ev.Method, result, extra)
		},
	}
}

// printCrawlSummary prints the final run-complete summary line shared by
// "crawl" and "work" in non-JSON mode.
func printCrawlSummary(p *ui.Printer, res jsonout.CrawlResult) {
	p.Summary("run complete",
		ui.Count(res.PagesFetched, "page fetched", "pages fetched"),
		ui.Count(res.HostsProbed, "host probed", "hosts probed"),
		ui.Count(res.CatalogsFound, "catalog found", "catalogs found"),
		fmt.Sprintf("%d valid", res.CatalogsValid),
		ui.Count(res.Errors, "error", "errors"),
	)
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
