// Package mcpserver embeds a stdio MCP (Model Context Protocol) server in
// the ardvark binary (`ardvark mcp`). Each tool is a thin wrapper over the
// shared command cores in internal/jsonout, returning the same typed JSON
// structures the CLI's --json flag emits. Diagnostics go to stderr only —
// stdout carries the stdio MCP protocol.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helgesverre/ardvark/internal/config"
	"github.com/helgesverre/ardvark/internal/fetch"
	"github.com/helgesverre/ardvark/internal/jsonout"
	"github.com/helgesverre/ardvark/internal/store"
)

// Server wires the ardvark tool handlers onto an mcp.Server. Configuration
// is resolved per tool call from configPath, exactly like the CLI (a missing
// file yields pure defaults).
type Server struct {
	configPath string
	version    string
	srv        *mcp.Server
	logger     *log.Logger
}

// New builds the ardvark MCP server, registering every tool.
func New(configPath, version string) *Server {
	s := &Server{
		configPath: configPath,
		version:    version,
		logger:     log.New(os.Stderr, "ardvark mcp: ", log.LstdFlags),
	}
	s.srv = mcp.NewServer(&mcp.Implementation{
		Name:       "ardvark",
		Title:      "ardvark ARD crawler",
		Version:    version,
		WebsiteURL: "https://ardvark.no",
	}, nil)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "ardvark_probe",
		Description: "Probe hosts directly for ARD (Agentic Resource Discovery) ai-catalog.json documents " +
			"via the well-known path and robots.txt Agentmap directives, recording results to the database. " +
			"Use this to check specific hosts without HTML spidering. Makes a few polite HTTP requests per " +
			"host; typically a couple of seconds per host, longer for unreachable ones (network timeouts).",
	}, s.probe)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "ardvark_verify",
		Description: "Verify a single ARD catalog document against the spec (JSON Schema + semantic checks) " +
			"and return the full check report: verdict (valid, valid_with_warnings, invalid) plus every " +
			"check's id, severity, subject, and message. Target is a URL (https://...) or a local file path. " +
			"Fast; one HTTP fetch at most. Does not write to the database.",
	}, s.verify)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "ardvark_crawl",
		Description: "Seed the persistent frontier from the given URLs and/or bare domains, then run the " +
			"crawler until the frontier is empty, and return the final run summary (pages fetched, hosts " +
			"probed, catalogs found, valid, errors). Pending work from prior runs is resumed automatically. " +
			"WARNING: long-running — a crawl drains the entire frontier at polite per-host rates and can " +
			"take minutes to hours depending on how much is queued. Prefer ardvark_probe for quick checks " +
			"of specific hosts.",
	}, s.crawl)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "ardvark_seed",
		Description: "Bootstrap the crawl frontier from an external domain source: ct (Certificate " +
			"Transparency logs), crtsh (crt.sh certificate search), tranco (Tranco top domains), github " +
			"(GitHub code search for deployed catalogs; needs GITHUB_TOKEN), mcp (the MCP server registry), " +
			"curated (community awesome-lists), or commoncrawl (Common Crawl domain ranks; supports offset). " +
			"Queues domains for a later ardvark_crawl; does not probe them itself. Duration varies by " +
			"source: seconds for github/mcp/curated, up to a minute or more for ct/tranco/commoncrawl " +
			"(large downloads).",
	}, s.seed)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "ardvark_stats",
		Description: "Summarize the indexed dataset: total domains by ARD status, catalogs by verification " +
			"verdict, and catalog entries by media type. Read-only and fast; call this first to see what " +
			"the index currently holds.",
	}, s.stats)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "ardvark_info",
		Description: "Report installation metadata: ardvark version, resolved config file path and whether " +
			"it exists, the storage backend (driver, DSN, absolute sqlite database path, existence, size), " +
			"and the event log location. Read-only and instant; never opens the database or the network, " +
			"so it works even when storage is misconfigured.",
	}, s.info)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "ardvark_export",
		Description: "Dump every indexed catalog entry (joined with its domain and verification status) to " +
			"a file as JSONL or CSV, and return the row count. Read-only apart from creating the output " +
			"file; fast for typical dataset sizes.",
	}, s.export)

	return s
}

// Run serves MCP over stdio until ctx is cancelled or the client
// disconnects. A client closing our stdin (EOF) is the normal end of a
// stdio session — including one-shot scripted use that pipes a whole
// session in at once — so an EOF-initiated shutdown returns nil rather
// than an error/exit 1.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Printf("serving MCP over stdio (config %s)", s.configPath)
	err := s.srv.Run(ctx, &mcp.StdioTransport{})
	if err != nil && isClientDisconnect(err) {
		s.logger.Printf("client disconnected: %v", err)
		return nil
	}
	return err
}

// isClientDisconnect reports whether err is the go-sdk's EOF-initiated
// shutdown. The sdk wraps the transport EOF in an internal jsonrpc2 wire
// error ("server is closing: EOF") that does not satisfy
// errors.Is(err, io.EOF), hence the string fallback.
func isClientDisconnect(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "server is closing") && strings.HasSuffix(msg, "EOF")
}

// Run builds the ardvark MCP server and serves it over stdio — the
// `ardvark mcp` entry point.
func Run(ctx context.Context, configPath, version string) error {
	return New(configPath, version).Run(ctx)
}

// openApp loads and validates ardvark.json from configPath (a missing file
// is not an error: config.Load returns pure defaults) and opens the
// configured database, running AutoMigrate — the same config resolution as
// the CLI. The caller must Close the store.
func (s *Server) openApp() (config.Config, *store.Store, error) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return config.Config{}, nil, err
	}
	st, err := store.Open(cfg.Storage.Driver, cfg.Storage.DSN)
	if err != nil {
		return config.Config{}, nil, err
	}
	return cfg, st, nil
}

// logf writes a diagnostic line to stderr (never stdout: stdio carries the
// MCP protocol).
func (s *Server) logf(format string, args ...any) {
	s.logger.Printf(format, args...)
}

// ProbeArgs are the arguments to ardvark_probe.
type ProbeArgs struct {
	Hosts []string `json:"hosts" jsonschema:"bare hostnames to probe for ARD documents, e.g. example.com"`
}

func (s *Server) probe(ctx context.Context, req *mcp.CallToolRequest, args ProbeArgs) (*mcp.CallToolResult, jsonout.ProbeReport, error) {
	if len(args.Hosts) == 0 {
		return nil, jsonout.ProbeReport{}, fmt.Errorf("probe: hosts must contain at least one hostname")
	}
	cfg, st, err := s.openApp()
	if err != nil {
		return nil, jsonout.ProbeReport{}, err
	}
	defer st.Close()

	rep := jsonout.ProbeHosts(ctx, fetch.New(cfg.Crawler), st, args.Hosts, jsonout.ProbeCallbacks{Errorf: s.logf})
	return nil, rep, nil
}

// VerifyArgs are the arguments to ardvark_verify.
type VerifyArgs struct {
	Target string `json:"target" jsonschema:"catalog document to verify: a URL (https://...) or a local file path"`
}

func (s *Server) verify(ctx context.Context, req *mcp.CallToolRequest, args VerifyArgs) (*mcp.CallToolResult, jsonout.VerifyReport, error) {
	if args.Target == "" {
		return nil, jsonout.VerifyReport{}, fmt.Errorf("verify: target is required")
	}
	rep, err := jsonout.VerifyTarget(ctx, args.Target)
	if err != nil {
		return nil, jsonout.VerifyReport{}, err
	}
	return nil, rep, nil
}

// CrawlArgs are the arguments to ardvark_crawl.
type CrawlArgs struct {
	Seeds []string `json:"seeds,omitempty" jsonschema:"URLs and/or bare domains to seed the frontier with; may be empty to just drain pending work"`
	Force bool     `json:"force,omitempty" jsonschema:"bypass the host_probe freshness window (re-probe hosts probed recently)"`
}

func (s *Server) crawl(ctx context.Context, req *mcp.CallToolRequest, args CrawlArgs) (*mcp.CallToolResult, jsonout.CrawlResult, error) {
	cfg, st, err := s.openApp()
	if err != nil {
		return nil, jsonout.CrawlResult{}, err
	}
	defer st.Close()

	s.logf("crawl starting: %d seed(s), force=%v", len(args.Seeds), args.Force)
	res, err := jsonout.Crawl(ctx, cfg, st, args.Seeds, args.Force, jsonout.CrawlCallbacks{
		SeedError: func(seed string, err error) { s.logf("crawl: failed to seed %q: %v", seed, err) },
	})
	if err != nil {
		return nil, jsonout.CrawlResult{}, err
	}
	s.logf("crawl finished: %d pages, %d hosts probed, %d catalogs", res.PagesFetched, res.HostsProbed, res.CatalogsFound)
	return nil, res, nil
}

// SeedArgs are the arguments to ardvark_seed.
type SeedArgs struct {
	Source string `json:"source" jsonschema:"domain source: one of ct, crtsh, tranco, github, mcp, curated, commoncrawl"`
	Count  int    `json:"count,omitempty" jsonschema:"how many domains (entries, for ct) to harvest; 0 uses the configured default"`
	Offset int    `json:"offset,omitempty" jsonschema:"ranked domains to skip before collecting (commoncrawl only)"`
}

func (s *Server) seed(ctx context.Context, req *mcp.CallToolRequest, args SeedArgs) (*mcp.CallToolResult, jsonout.SeedResult, error) {
	cfg, st, err := s.openApp()
	if err != nil {
		return nil, jsonout.SeedResult{}, err
	}
	defer st.Close()

	seeder, n, label, err := jsonout.BuildSeeder(ctx, cfg, args.Source, args.Count, args.Offset)
	if err != nil {
		return nil, jsonout.SeedResult{}, err
	}
	res, err := jsonout.RunSeeder(ctx, cfg, st, seeder, n, label)
	if err != nil {
		return nil, jsonout.SeedResult{}, err
	}
	return nil, res, nil
}

func (s *Server) stats(ctx context.Context, req *mcp.CallToolRequest, args any) (*mcp.CallToolResult, jsonout.StatsReport, error) {
	_, st, err := s.openApp()
	if err != nil {
		return nil, jsonout.StatsReport{}, err
	}
	defer st.Close()

	rep, err := jsonout.Stats(st)
	if err != nil {
		return nil, jsonout.StatsReport{}, err
	}
	return nil, rep, nil
}

func (s *Server) info(ctx context.Context, req *mcp.CallToolRequest, args any) (*mcp.CallToolResult, jsonout.InfoReport, error) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		return nil, jsonout.InfoReport{}, err
	}
	return nil, jsonout.Info(cfg, s.configPath, s.version), nil
}

// ExportArgs are the arguments to ardvark_export.
type ExportArgs struct {
	Format string `json:"format" jsonschema:"output format: jsonl or csv"`
	Out    string `json:"out" jsonschema:"file path to write the export to"`
}

func (s *Server) export(ctx context.Context, req *mcp.CallToolRequest, args ExportArgs) (*mcp.CallToolResult, jsonout.ExportResult, error) {
	if args.Out == "" {
		return nil, jsonout.ExportResult{}, fmt.Errorf("export: out is required (stdout carries the MCP protocol)")
	}
	_, st, err := s.openApp()
	if err != nil {
		return nil, jsonout.ExportResult{}, err
	}
	defer st.Close()

	res, err := jsonout.Export(st, args.Format, args.Out, nil)
	if err != nil {
		return nil, jsonout.ExportResult{}, err
	}
	return nil, res, nil
}
