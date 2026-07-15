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

	if _, _, err := s.export(t.Context(), nil, ExportArgs{Format: "xml", Out: out}); err == nil {
		t.Error("export handler with bad format: want error, got nil")
	}
	if _, _, err := s.export(t.Context(), nil, ExportArgs{Format: "jsonl"}); err == nil {
		t.Error("export handler without out: want error, got nil")
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

// An MCP client connecting over an in-memory transport must see all seven
// ardvark tools with descriptions.
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

	got := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
	for _, want := range []string{
		"ardvark_probe", "ardvark_verify", "ardvark_crawl",
		"ardvark_seed", "ardvark_stats", "ardvark_info", "ardvark_export",
	} {
		if !got[want] {
			t.Errorf("tools/list missing %s (got %v)", want, res.Tools)
		}
	}
}
