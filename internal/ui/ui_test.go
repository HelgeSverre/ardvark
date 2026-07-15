package ui

import (
	"bytes"
	"strings"
	"testing"
)

func plainPrinter(buf *bytes.Buffer) *Printer {
	return New(buf, NoColor())
}

func colorPrinter(buf *bytes.Buffer) *Printer {
	p := New(buf, ForceColor())
	p.truecolor = true
	return p
}

func TestRowAlignmentPlain(t *testing.T) {
	var buf bytes.Buffer
	p := plainPrinter(&buf)
	p.Row(StatusHit, "acme.com", "well-known", "catalog valid", "14 entries")
	p.Row(StatusMiss, "blog.someone.net", "well-known", "404", "")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	// Same column start for host on both lines.
	if strings.Index(lines[0], "acme.com") != strings.Index(lines[1], "blog.someone.net") {
		t.Errorf("host columns misaligned:\n%s\n%s", lines[0], lines[1])
	}
	if !strings.HasPrefix(lines[0], "  hit  ") {
		t.Errorf("unexpected row prefix: %q", lines[0])
	}
	if !strings.HasSuffix(lines[0], "14 entries") {
		t.Errorf("extra detail missing: %q", lines[0])
	}
	// No trailing padding on short rows.
	if strings.HasSuffix(lines[1], " ") {
		t.Errorf("trailing whitespace on row: %q", lines[1])
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("plain printer emitted escape codes: %q", buf.String())
	}
}

func TestRowColors(t *testing.T) {
	cases := []struct {
		status Status
		want   string // expected truecolor prefix for the label
	}{
		{StatusHit, "\x1b[38;2;143;191;127m"},     // green
		{StatusWarnHit, "\x1b[38;2;244;169;127m"}, // peach
		{StatusInvalid, "\x1b[38;2;232;115;74m"},  // terracotta
		{StatusMiss, "\x1b[38;2;176;143;115m"},    // muted brown
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		colorPrinter(&buf).Row(tc.status, "h", "m", "r", "")
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("status %d: want escape %q in %q", tc.status, tc.want, buf.String())
		}
	}
}

func TestSummary(t *testing.T) {
	var buf bytes.Buffer
	plainPrinter(&buf).Summary("run complete: ", "847 hosts probed", "3 catalogs", "41 resources indexed")
	want := "run complete: 847 hosts probed · 3 catalogs · 41 resources indexed\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestCheckMarks(t *testing.T) {
	var buf bytes.Buffer
	p := plainPrinter(&buf)
	p.Check(true, false, "catalog.spec_version", "")
	p.Check(false, true, "queries.count", "1 query (2-5 recommended)")
	p.Check(false, false, "urn.format", "bad prefix")
	out := buf.String()
	for _, want := range []string{"✓ catalog.spec_version", "! queries.count", "✗ urn.format"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestVerdictStyles(t *testing.T) {
	for verdict, wantEsc := range map[string]string{
		"valid":               "\x1b[38;2;143;191;127m",
		"valid_with_warnings": "\x1b[38;2;244;169;127m",
		"invalid":             "\x1b[38;2;232;115;74m",
	} {
		var buf bytes.Buffer
		colorPrinter(&buf).Verdict(verdict)
		if !strings.Contains(buf.String(), wantEsc+verdict) {
			t.Errorf("verdict %s: want %q in %q", verdict, wantEsc, buf.String())
		}
	}
}

func TestNonTTYDisablesColor(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf) // bytes.Buffer is not a terminal
	if p.color {
		t.Error("color should be disabled for non-TTY writer")
	}
}

func Test256Fallback(t *testing.T) {
	var buf bytes.Buffer
	p := New(&buf, ForceColor())
	p.truecolor = false
	p.Errorf("boom")
	if !strings.Contains(buf.String(), "\x1b[38;5;173m") {
		t.Errorf("want 256-color terracotta escape, got %q", buf.String())
	}
}
