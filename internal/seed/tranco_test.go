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
