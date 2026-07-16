package seed

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// makeTrancoZip builds an in-memory zip archive with a single CSV file
// containing "rank,domain" rows, matching Tranco's real distribution
// format.
func makeTrancoZip(t *testing.T, rows [][2]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("top-1m.csv")
	if err != nil {
		t.Fatalf("creating zip entry: %v", err)
	}
	for _, row := range rows {
		if _, err := f.Write([]byte(row[0] + "," + row[1] + "\n")); err != nil {
			t.Fatalf("writing csv row: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip writer: %v", err)
	}
	return buf.Bytes()
}

func TestTrancoSeederDomains(t *testing.T) {
	zipBytes := makeTrancoZip(t, [][2]string{
		{"1", "example.com"},
		{"2", "*.wildcard.com"},
		{"3", "third.example.com"},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipBytes)
	}))
	defer srv.Close()

	seeder := &TrancoSeeder{ListURL: srv.URL}
	names, err := seeder.Domains(context.Background(), 2)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	want := []string{"example.com", "wildcard.com"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}

	if seeder.Source() != "tranco" {
		t.Errorf("Source() = %q, want tranco", seeder.Source())
	}
}

func TestTrancoSeederDomains_FewerRowsThanRequested(t *testing.T) {
	zipBytes := makeTrancoZip(t, [][2]string{{"1", "only.example.com"}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipBytes)
	}))
	defer srv.Close()

	seeder := &TrancoSeeder{ListURL: srv.URL}
	names, err := seeder.Domains(context.Background(), 100)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 1 || names[0] != "only.example.com" {
		t.Fatalf("got %v, want [only.example.com]", names)
	}
}

func TestTrancoSeederDomains_SkipsInvalidRowsToReachN(t *testing.T) {
	// Interleave rows that Sanitize drops (an IP address, a bare label with
	// no dot, a duplicate) among enough valid ones that n=3 valid domains
	// are available further down the list than row 3.
	zipBytes := makeTrancoZip(t, [][2]string{
		{"1", "192.0.2.1"},         // IP address, dropped
		{"2", "example.com"},       // valid
		{"3", "nodothost"},         // no dot, dropped
		{"4", "example.com"},       // duplicate, dropped
		{"5", "*.wildcard.com"},    // valid (wildcard stripped)
		{"6", "third.example.com"}, // valid
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipBytes)
	}))
	defer srv.Close()

	seeder := &TrancoSeeder{ListURL: srv.URL}
	names, err := seeder.Domains(context.Background(), 3)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	want := []string{"example.com", "wildcard.com", "third.example.com"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestTrancoSeederDomains_FewerValidRowsThanRequested(t *testing.T) {
	// Only one row survives sanitization even though there are several raw
	// rows; the source is genuinely exhausted, so Domains should return
	// exactly that one instead of erroring or padding.
	zipBytes := makeTrancoZip(t, [][2]string{
		{"1", "192.0.2.1"},
		{"2", "only.example.com"},
		{"3", "nodothost"},
		{"4", "only.example.com"}, // duplicate
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipBytes)
	}))
	defer srv.Close()

	seeder := &TrancoSeeder{ListURL: srv.URL}
	names, err := seeder.Domains(context.Background(), 100)
	if err != nil {
		t.Fatalf("Domains: %v", err)
	}
	if len(names) != 1 || names[0] != "only.example.com" {
		t.Fatalf("got %v, want [only.example.com]", names)
	}
}

func TestTrancoSeederDomains_RejectsNonPositiveN(t *testing.T) {
	seeder := &TrancoSeeder{ListURL: "http://example.invalid"}
	if _, err := seeder.Domains(context.Background(), 0); err == nil {
		t.Fatal("expected error for n=0")
	}
}

func TestTrancoSeederDomains_InvalidZip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not a zip file"))
	}))
	defer srv.Close()

	seeder := &TrancoSeeder{ListURL: srv.URL}
	if _, err := seeder.Domains(context.Background(), 1); err == nil {
		t.Fatal("expected error for invalid zip body")
	}
}
