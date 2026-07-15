// Package ui renders all user-facing CLI output in one consistent style:
// aligned probe rows, muted summaries, and check reports, colored with the
// ardvark palette. Every command prints through a Printer so the output
// looks the same everywhere.
//
// Colors are 24-bit when the terminal advertises truecolor (COLORTERM),
// 256-color otherwise, and disabled entirely when the writer is not a TTY,
// NO_COLOR is set, or TERM=dumb.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Style is a named color in the ardvark palette.
type Style int

const (
	StylePlain  Style = iota
	StyleGood         // soft green — valid, hit
	StyleWarn         // peach — warnings
	StyleBad          // terracotta — invalid, errors
	StyleMuted        // warm brown — misses, detail, summaries
	StyleAccent       // peach — prompts, emphasis
	StyleBold
)

// palette entry: truecolor RGB and the closest 256-color code.
type color struct {
	r, g, b uint8
	c256    uint8
}

var palette = map[Style]color{
	StyleGood:   {0x8F, 0xBF, 0x7F, 108}, // green from the site demo
	StyleWarn:   {0xF4, 0xA9, 0x7F, 216}, // mascot peach
	StyleBad:    {0xE8, 0x73, 0x4A, 173}, // mascot terracotta
	StyleMuted:  {0xB0, 0x8F, 0x73, 138}, // warm brown
	StyleAccent: {0xF4, 0xA9, 0x7F, 216},
}

// Status classifies a probe/crawl row.
type Status int

const (
	StatusHit Status = iota
	StatusWarnHit
	StatusMiss
	StatusInvalid
	StatusError
)

// Printer writes styled output. Zero value is unusable; use New.
type Printer struct {
	w         io.Writer
	color     bool
	truecolor bool
}

// Option configures a Printer.
type Option func(*Printer)

// ForceColor enables color regardless of TTY detection (e.g. --color=always).
func ForceColor() Option { return func(p *Printer) { p.color = true } }

// NoColor disables color regardless of detection (e.g. --color=never).
func NoColor() Option { return func(p *Printer) { p.color = false } }

// New returns a Printer for w. Color is enabled only when w is a terminal,
// NO_COLOR is unset, and TERM is not "dumb".
func New(w io.Writer, opts ...Option) *Printer {
	p := &Printer{
		w:         w,
		color:     isTerminal(w) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb",
		truecolor: supportsTruecolor(),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func supportsTruecolor() bool {
	ct := os.Getenv("COLORTERM")
	return strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit")
}

// Paint wraps s in the escape codes for style. With color disabled it
// returns s unchanged.
func (p *Printer) Paint(style Style, s string) string {
	if !p.color || style == StylePlain {
		return s
	}
	if style == StyleBold {
		return "\x1b[1m" + s + "\x1b[0m"
	}
	c, ok := palette[style]
	if !ok {
		return s
	}
	if p.truecolor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", c.r, c.g, c.b, s)
	}
	return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", c.c256, s)
}

// statusLabel returns the fixed-width label and style for a row status.
func statusLabel(s Status) (string, Style) {
	switch s {
	case StatusHit:
		return "hit", StyleGood
	case StatusWarnHit:
		return "hit", StyleWarn
	case StatusMiss:
		return "miss", StyleMuted
	case StatusInvalid:
		return "hit", StyleBad
	case StatusError:
		return "err", StyleBad
	default:
		return "?", StylePlain
	}
}

// Row prints one aligned crawl/probe result row, matching the canonical
// format:
//
//	hit   acme.com            well-known       catalog valid        14 entries
//	miss  blog.someone.net    well-known       404
//
// extra is optional muted detail at the end of the line.
func (p *Printer) Row(status Status, host, method, result, extra string) {
	label, style := statusLabel(status)
	// Pad before painting: escape codes would break %-5s width math.
	line := "  " + p.Paint(style, fmt.Sprintf("%-5s", label)) +
		fmt.Sprintf(" %-22s %-16s %-22s", host, method, result)
	if extra != "" {
		line += " " + p.Paint(StyleMuted, extra)
	}
	fmt.Fprintln(p.w, strings.TrimRight(line, " "))
}

// Summary prints the muted end-of-run line, joining parts with " · ":
//
//	run complete: 847 hosts probed · 3 catalogs · 41 resources indexed
func (p *Printer) Summary(prefix string, parts ...string) {
	fmt.Fprintln(p.w, p.Paint(StyleMuted, prefix+strings.Join(parts, " · ")))
}

// Check prints one verification check line for `ardvark verify`:
//
//	✓ catalog.spec_version
//	✗ urn.format             entry urn:air:x — publisher segment is not a FQDN
//	! queries.count          entry urn:air:y — 1 representative query (2–5 recommended)
func (p *Printer) Check(passed bool, warning bool, checkID, detail string) {
	var mark string
	switch {
	case passed:
		mark = p.Paint(StyleGood, "✓")
	case warning:
		mark = p.Paint(StyleWarn, "!")
	default:
		mark = p.Paint(StyleBad, "✗")
	}
	line := fmt.Sprintf("  %s %-26s", mark, checkID)
	if detail != "" {
		line += " " + p.Paint(StyleMuted, detail)
	}
	fmt.Fprintln(p.w, strings.TrimRight(line, " "))
}

// Verdict prints the rolled-up verification verdict, colored by outcome.
func (p *Printer) Verdict(verdict string) {
	style := StyleGood
	switch verdict {
	case "invalid":
		style = StyleBad
	case "valid_with_warnings":
		style = StyleWarn
	}
	fmt.Fprintf(p.w, "%s %s\n", p.Paint(StyleBold, "verdict:"), p.Paint(style, verdict))
}

// Infof prints a plain informational line.
func (p *Printer) Infof(format string, args ...any) {
	fmt.Fprintf(p.w, format+"\n", args...)
}

// Mutedf prints a muted line (progress notes, skip reasons).
func (p *Printer) Mutedf(format string, args ...any) {
	fmt.Fprintln(p.w, p.Paint(StyleMuted, fmt.Sprintf(format, args...)))
}

// Errorf prints an error line in terracotta.
func (p *Printer) Errorf(format string, args ...any) {
	fmt.Fprintln(p.w, p.Paint(StyleBad, fmt.Sprintf(format, args...)))
}

// KV prints an aligned key/value pair for `ardvark stats`-style output.
func (p *Printer) KV(key string, value any) {
	fmt.Fprintf(p.w, "  %-28s %v\n", p.Paint(StyleMuted, key), value)
}

// Header prints a bold section header.
func (p *Printer) Header(s string) {
	fmt.Fprintln(p.w, p.Paint(StyleBold, s))
}
