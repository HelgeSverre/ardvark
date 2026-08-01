package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helgesverre/ardvark/internal/jsonout"
	"github.com/helgesverre/ardvark/internal/store"
)

// newTestServer builds a Server whose config points at a temp sqlite
// database, and returns the database DSN so tests can seed rows directly.
func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "ardvark.json")
	cfg := fmt.Sprintf(`{"storage":{"driver":"sqlite","dsn":%q},"log":{"file":%q}}`,
		dsn, filepath.Join(dir, "test.jsonl"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return New(cfgPath, "test"), dsn
}

// seedTestData populates the test database with two domains, one catalog,
// and one entry.
func seedTestData(t *testing.T, dsn string) {
	t.Helper()
	st, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	if _, err := st.UpsertDomain("a.com", store.DiscoverySourceSeed); err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	d, err := st.UpsertDomain("b.com", store.DiscoverySourceSeed)
	if err != nil {
		t.Fatalf("UpsertDomain: %v", err)
	}
	if err := st.SaveCatalog(&store.Catalog{
		DomainID: d.ID, SourceURL: "https://b.com/.well-known/ai-catalog.json",
		FetchedAt: time.Now(), VerificationStatus: store.VerificationStatusValid,
		Entries: []store.CatalogEntry{{URN: "urn:air:b.com:tools:x", MediaType: "application/ai-skill+json"}},
	}, nil, nil); err != nil {
		t.Fatalf("SaveCatalog: %v", err)
	}
}

// The ardvark_stats handler must summarize the dataset through the shared
// jsonout core.
func TestStatsHandler(t *testing.T) {
	s, dsn := newTestServer(t)
	seedTestData(t, dsn)

	_, rep, err := s.stats(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("stats handler: %v", err)
	}
	if rep.Domains.Total != 2 {
		t.Errorf("Domains.Total = %d, want 2", rep.Domains.Total)
	}
	if rep.Catalogs.Total != 1 {
		t.Errorf("Catalogs.Total = %d, want 1", rep.Catalogs.Total)
	}
	if rep.Entries.Total != 1 {
		t.Errorf("Entries.Total = %d, want 1", rep.Entries.Total)
	}
}

// The ardvark_verify handler must verify a local catalog file and return the
// full typed check report.
func TestVerifyHandler(t *testing.T) {
	s, _ := newTestServer(t)

	target := filepath.Join("..", "ard", "testdata", "enterprise-catalog.json")
	_, rep, err := s.verify(t.Context(), nil, VerifyArgs{Target: target})
	if err != nil {
		t.Fatalf("verify handler: %v", err)
	}
	if rep.Source != target {
		t.Errorf("Source = %q, want %q", rep.Source, target)
	}
	if rep.Verdict == "" {
		t.Error("Verdict is empty")
	}
	if len(rep.Checks) == 0 {
		t.Fatal("Checks is empty")
	}
	c := rep.Checks[0]
	if c.ID == "" || c.Severity == "" || c.Subject == "" {
		t.Errorf("check missing fields: %+v", c)
	}

	if _, _, err := s.verify(t.Context(), nil, VerifyArgs{}); err == nil {
		t.Error("verify handler with empty target: want error, got nil")
	}
}

// The ardvark_export handler must write the requested file and report the
// row count; format and out are validated.
func TestExportHandler(t *testing.T) {
	s, dsn := newTestServer(t)
	seedTestData(t, dsn)

	out := filepath.Join(t.TempDir(), "export.jsonl")
	_, res, err := s.export(t.Context(), nil, ExportArgs{Format: "jsonl", Out: out})
	if err != nil {
		t.Fatalf("export handler: %v", err)
	}
	if res.Rows != 1 {
		t.Errorf("Rows = %d, want 1", res.Rows)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading export: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatalf("export line is not JSON: %v", err)
	}
	if row["host"] != "b.com" {
		t.Errorf("host = %v, want b.com", row["host"])
	}
	if _, _, err := s.export(t.Context(), nil, ExportArgs{Format: "jsonl", Out: out}); err == nil {
		t.Error("export handler without overwrite replaced an existing file")
	}
	if _, _, err := s.export(t.Context(), nil, ExportArgs{Format: "jsonl", Out: out, Overwrite: true}); err != nil {
		t.Errorf("export handler with overwrite: %v", err)
	}

	if _, _, err := s.export(t.Context(), nil, ExportArgs{Format: "xml", Out: out}); err == nil {
		t.Error("export handler with bad format: want error, got nil")
	}
	if _, _, err := s.export(t.Context(), nil, ExportArgs{Format: "jsonl"}); err == nil {
		t.Error("export handler without out: want error, got nil")
	}
}

func TestSearchAndGetHandlers(t *testing.T) {
	s, dsn := newTestServer(t)
	seedTestData(t, dsn)

	_, search, err := s.search(t.Context(), nil, jsonout.SearchOptions{Query: "b.com"})
	if err != nil {
		t.Fatalf("search handler: %v", err)
	}
	if len(search.Entries) != 1 {
		t.Fatalf("search entries = %+v, want one", search.Entries)
	}
	entry := search.Entries[0]

	_, gotEntry, err := s.getEntry(t.Context(), nil, GetArgs{Selector: entry.URN})
	if err != nil {
		t.Fatalf("get entry handler: %v", err)
	}
	if gotEntry.EntryID != entry.EntryID {
		t.Errorf("get entry ID = %d, want %d", gotEntry.EntryID, entry.EntryID)
	}

	_, gotCatalog, err := s.getCatalog(t.Context(), nil, jsonout.GetCatalogOptions{Selector: entry.CatalogSourceURL})
	if err != nil {
		t.Fatalf("get catalog handler: %v", err)
	}
	if gotCatalog.ID != entry.CatalogID {
		t.Errorf("get catalog ID = %d, want %d", gotCatalog.ID, entry.CatalogID)
	}
}

// The ardvark_seed handler must reject unknown sources before doing any
// network work.
func TestSeedHandlerUnknownSource(t *testing.T) {
	s, _ := newTestServer(t)
	if _, _, err := s.seed(t.Context(), nil, SeedArgs{Source: "nope"}); err == nil {
		t.Error("seed handler with unknown source: want error, got nil")
	}
}

// A one-shot scripted session — a full initialize + tools/list piped to
// stdin at once, then immediate EOF — must end Run cleanly (nil, exit 0),
// not surface the go-sdk's EOF-initiated shutdown as an error.
func TestRun_StdinEOFIsCleanShutdown(t *testing.T) {
	s, _ := newTestServer(t)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	go io.Copy(io.Discard, outR)

	go func() {
		for _, msg := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"one-shot","version":"0"}}}`,
			`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		} {
			fmt.Fprintln(inW, msg)
		}
		inW.Close()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatalf("Run after stdin EOF = %v, want nil", err)
	}
}

// The ardvark_info handler must report the server's version and the resolved
// config and database locations without opening the database.
func TestInfoHandler(t *testing.T) {
	s, dsn := newTestServer(t)

	_, rep, err := s.info(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("info handler: %v", err)
	}
	if rep.Version != "test" {
		t.Errorf("Version = %q, want %q", rep.Version, "test")
	}
	if !rep.Config.Exists {
		t.Errorf("Config.Exists = false, want true (config at %s)", rep.Config.Path)
	}
	if rep.Config.Path != s.configPath {
		t.Errorf("Config.Path = %q, want %q", rep.Config.Path, s.configPath)
	}
	if rep.Storage.Driver != "sqlite" {
		t.Errorf("Storage.Driver = %q, want sqlite", rep.Storage.Driver)
	}
	if rep.Storage.Path != dsn {
		t.Errorf("Storage.Path = %q, want %q", rep.Storage.Path, dsn)
	}
	// newTestServer never opens the store, so the database file must not
	// exist yet — and info must not have created it.
	if rep.Storage.Exists {
		t.Error("Storage.Exists = true, want false (info must not create the database)")
	}
	if _, err := os.Stat(dsn); !os.IsNotExist(err) {
		t.Errorf("database file exists after info call (stat err = %v)", err)
	}
}

// An MCP client connecting over an in-memory transport must see every
// ardvark tool with descriptions, schemas, and safety annotations.
func TestToolsList(t *testing.T) {
	s, _ := newTestServer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := s.srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	got := make(map[string]*mcp.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = tool
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("tool %s is missing an inferred schema", tool.Name)
		}
		if tool.Annotations == nil {
			t.Errorf("tool %s has no safety annotations", tool.Name)
		}
	}
	for _, want := range []string{
		"ardvark_probe", "ardvark_verify", "ardvark_crawl",
		"ardvark_seed", "ardvark_stats", "ardvark_info", "ardvark_export",
		"ardvark_search", "ardvark_get_catalog", "ardvark_get_entry",
	} {
		if got[want] == nil {
			t.Errorf("tools/list missing %s (got %v)", want, res.Tools)
		}
	}
	if tool := got["ardvark_search"]; tool != nil && !tool.Annotations.ReadOnlyHint {
		t.Error("ardvark_search must be annotated read-only")
	}
	if tool := got["ardvark_export"]; tool != nil && (tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint) {
		t.Error("ardvark_export must be annotated destructive")
	}
	if tool := got["ardvark_crawl"]; tool != nil && (tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint) {
		t.Error("ardvark_crawl must be annotated open-world")
	}
}

func TestResourceTemplatesAndRead(t *testing.T) {
	s, dsn := newTestServer(t)
	seedTestData(t, dsn)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := s.srv.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	templates := map[string]bool{}
	for tmpl, err := range session.ResourceTemplates(ctx, nil) {
		if err != nil {
			t.Fatalf("resources/templates/list: %v", err)
		}
		templates[tmpl.URITemplate] = true
	}
	for _, want := range []string{"ardvark://catalog/{id}", "ardvark://entry/{id}"} {
		if !templates[want] {
			t.Errorf("resource templates missing %s: %v", want, templates)
		}
	}
	toolResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ardvark_search",
		Arguments: map[string]any{"query": "b.com"},
	})
	if err != nil {
		t.Fatalf("tools/call ardvark_search: %v", err)
	}
	if toolResult.IsError || toolResult.StructuredContent == nil {
		t.Fatalf("ardvark_search result = %+v", toolResult)
	}
	encoded, err := json.Marshal(toolResult.StructuredContent)
	if err != nil {
		t.Fatalf("encoding structured search result: %v", err)
	}
	var search jsonout.SearchResult
	if err := json.Unmarshal(encoded, &search); err != nil {
		t.Fatalf("decoding structured search result: %v", err)
	}
	if len(search.Entries) != 1 {
		t.Fatalf("structured search entries = %+v", search.Entries)
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ardvark://entry/1"})
	if err != nil {
		t.Fatalf("resources/read entry: %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].MIMEType != "application/json" {
		t.Fatalf("entry resource contents = %+v", res.Contents)
	}
	var entry jsonout.EntryDetail
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &entry); err != nil {
		t.Fatalf("decoding entry resource: %v", err)
	}
	if entry.URN != "urn:air:b.com:tools:x" {
		t.Errorf("entry resource URN = %q", entry.URN)
	}

	res, err = session.ReadResource(ctx, &mcp.ReadResourceParams{URI: fmt.Sprintf("ardvark://catalog/%d", search.Entries[0].CatalogID)})
	if err != nil {
		t.Fatalf("resources/read catalog: %v", err)
	}
	if len(res.Contents) != 1 || res.Contents[0].MIMEType != "application/json" {
		t.Fatalf("catalog resource contents = %+v", res.Contents)
	}
	var catalog jsonout.CatalogDetail
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &catalog); err != nil {
		t.Fatalf("decoding catalog resource: %v", err)
	}
	if catalog.SourceURL != search.Entries[0].CatalogSourceURL || len(catalog.Entries) != 1 {
		t.Errorf("catalog resource = %+v", catalog)
	}

	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ardvark://entry/999"}); err == nil {
		t.Error("resources/read missing entry: want error")
	}
}

func TestResourceID(t *testing.T) {
	for _, tt := range []struct {
		uri  string
		kind string
		want string
		ok   bool
	}{
		{uri: "ardvark://catalog/42", kind: "catalog", want: "42", ok: true},
		{uri: "ardvark://entry/1", kind: "entry", want: "1", ok: true},
		{uri: "ardvark://entry/0", kind: "entry"},
		{uri: "ardvark://entry/1/2", kind: "entry"},
		{uri: "ardvark://catalog/1?x=y", kind: "catalog"},
		{uri: "file://entry/1", kind: "entry"},
	} {
		got, ok := resourceID(tt.uri, tt.kind)
		if got != tt.want || ok != tt.ok {
			t.Errorf("resourceID(%q, %q) = %q, %v; want %q, %v", tt.uri, tt.kind, got, ok, tt.want, tt.ok)
		}
	}
}
