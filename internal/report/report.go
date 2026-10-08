// Package report writes findings: for people (text, markdown), for tools (json, jsonl,
// sarif, github annotations) and for language models (llm).
package report

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/xen0bit/hotbutteredbeans/internal/gitx"
	"github.com/xen0bit/hotbutteredbeans/internal/scan"
)

// Formats are the output formats.
var Formats = []string{"text", "json", "jsonl", "sarif", "markdown", "llm", "github"}

// Rule is one question the model answers.
type Rule struct {
	Label, CWE, Title, Question string
}

// Report is a scan's outcome.
type Report struct {
	Tool     string         `json:"tool"`
	Version  string         `json:"version"`
	Mode     string         `json:"mode"` // scan | diff
	Root     string         `json:"root"`
	Base     string         `json:"base,omitempty"`
	Head     string         `json:"head,omitempty"`
	Compare  bool           `json:"compare,omitempty"`
	Model    string         `json:"model"`
	Device   string         `json:"device"`
	Files    int            `json:"files"`
	Windows  int            `json:"windows"`
	MinP     float64        `json:"min_p"`
	Findings []scan.Finding `json:"findings"`

	Rules        []Rule `json:"-"`
	ContentLines int    `json:"-"`
	Color        bool   `json:"-"`
	MaxChars     int    `json:"-"` // llm: the budget for code
}

// Write writes r in a format.
func Write(w io.Writer, format string, r *Report) error {
	switch format {
	case "text", "":
		return Text(w, r)
	case "json":
		return JSON(w, r)
	case "jsonl":
		return JSONL(w, r)
	case "sarif":
		return SARIF(w, r)
	case "markdown", "md":
		return Markdown(w, r)
	case "llm":
		return LLM(w, r)
	case "github":
		return GitHub(w, r)
	}
	return fmt.Errorf("unknown format %q (%s)", format, strings.Join(Formats, ", "))
}

// Valid reports whether format is known.
func Valid(format string) bool { return format == "md" || slices.Contains(Formats, format) }

// window groups the findings of one window, in rank order of its best finding.
type window struct {
	first    scan.Finding
	findings []scan.Finding
}

func byWindow(fs []scan.Finding) []*window {
	var out []*window
	idx := map[string]*window{}
	for _, f := range fs {
		w, ok := idx[f.Location]
		if !ok {
			w = &window{first: f}
			idx[f.Location] = w
			out = append(out, w)
		}
		w.findings = append(w.findings, f)
	}
	return out
}

// numbered renders a window's lines with their numbers, marking changed ones with ">".
// limit > 0 keeps that many lines: around the changes in a diff, else from the top.
func numbered(f scan.Finding, limit int, marker bool) (string, int) {
	lines := strings.Split(f.Content, "\n")
	lo, hi := 0, len(lines)
	if limit > 0 && len(lines) > limit {
		if len(f.Changed) > 0 {
			c := f.Changed[0]
			first := c.From - f.From
			lo = max(0, min(first-2, len(lines)-limit))
			hi = lo + limit
		} else {
			hi = limit
		}
	}
	width := len(fmt.Sprint(f.From + len(lines) - 1))
	var b strings.Builder
	for i := lo; i < hi; i++ {
		n := f.From + i
		mark := " "
		if marker && changed(f.Changed, n) {
			mark = ">"
		}
		fmt.Fprintf(&b, "%s%*d | %s\n", mark, width, n, strings.TrimRight(lines[i], "\r"))
	}
	return b.String(), len(lines) - (hi - lo)
}

func changed(rs []gitx.Range, n int) bool {
	for _, r := range rs {
		if n >= r.From && n <= r.To {
			return true
		}
	}
	return false
}

func ranges(rs []gitx.Range) string {
	var parts []string
	for _, r := range rs {
		if r.From == r.To {
			parts = append(parts, fmt.Sprint(r.From))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", r.From, r.To))
		}
	}
	return strings.Join(parts, ",")
}

func delta(f scan.Finding) string {
	if f.Delta == nil {
		return ""
	}
	if f.BaseP == nil {
		return "new code"
	}
	return fmt.Sprintf("%+.2f from %.2f", *f.Delta, *f.BaseP)
}

// span is the lines a finding points at: its changed lines in a diff, else the window.
func span(f scan.Finding) (int, int) {
	if len(f.Changed) > 0 {
		return f.Changed[0].From, f.Changed[len(f.Changed)-1].To
	}
	return f.From, f.To
}

// About explains what the numbers mean, for any reader that was not told.
const About = `hbb ranks code by how much it resembles code that a security fix changed, for each
of the CWE Top 25. p is a calibrated probability that the answer to the question is
"yes" for the window (up to 240 lines). It is a reading order, not a verdict: most high
scores are code worth a look, not proven flaws. A rise from the base version (delta) is
the model's strongest signal: it was trained to score code before a fix above the same
code after it.`
