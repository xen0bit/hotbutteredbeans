// Package scan turns files and diffs into windows, scores them, and ranks the
// (window, question) pairs into findings.
package scan

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/xen0bit/hotbutteredbeans/internal/bundle"
	"github.com/xen0bit/hotbutteredbeans/internal/engine"
	"github.com/xen0bit/hotbutteredbeans/internal/gitx"
	"github.com/xen0bit/hotbutteredbeans/internal/window"
)

// Target is a window to score.
type Target struct {
	Path, Lang string
	Window     window.Window
	// Diff mode: the lines of this window that changed, and the windows of the file's
	// base version that the same changes touched (empty for a new file).
	Changed []gitx.Range
	Base    []window.Window
	HasBase bool
}

// Result is a window's calibrated probabilities, on the questions asked of its language.
type Result struct {
	Target
	P        map[string]float64 // label -> P(yes)
	BaseP    map[string]float64 // label -> the highest P(yes) among the base windows
	Compared bool               // scored against the base (BaseP is nil for a new file)
}

// Score scores targets, and their base windows when compare is set.
func Score(ctx context.Context, e *engine.Engine, targets []Target, compare bool, progress func(done, total int)) ([]Result, engine.Stats, error) {
	b := e.Bundle()
	var items []engine.Item
	baseIdx := make([][]int, len(targets))
	for _, t := range targets {
		items = append(items, engine.Item{Text: t.Window.Text})
	}
	if compare {
		for i, t := range targets {
			for _, w := range t.Base {
				baseIdx[i] = append(baseIdx[i], len(items))
				items = append(items, engine.Item{Text: w.Text})
			}
		}
	}
	logits, st, err := e.Score(ctx, items, progress)
	if err != nil {
		return nil, st, err
	}
	res := make([]Result, len(targets))
	for i, t := range targets {
		r := Result{Target: t, P: probabilities(b, t.Lang, logits[i]), Compared: compare}
		if compare && t.HasBase {
			r.BaseP = map[string]float64{}
			for _, k := range baseIdx[i] {
				for q, p := range probabilities(b, t.Lang, logits[k]) {
					r.BaseP[q] = max(r.BaseP[q], p)
				}
			}
		}
		res[i] = r
	}
	return res, st, nil
}

func probabilities(b *bundle.Bundle, lang string, logits []float32) map[string]float64 {
	p := map[string]float64{}
	for q, label := range b.Labels {
		if b.Asked(label, lang) {
			p[label] = b.Probability(logits[q])
		}
	}
	return p
}

// Finding is one (window, question) of the ranking.
type Finding struct {
	CWE      string   `json:"cwe"` // "CWE-89"
	Label    string   `json:"label"`
	Title    string   `json:"title"`
	Question string   `json:"question"`
	P        float64  `json:"p"`
	BaseP    *float64 `json:"base_p,omitempty"` // diff --compare: the same question before the change
	Delta    *float64 `json:"delta,omitempty"`  // P - BaseP (P for a new file)

	Path     string       `json:"path"`
	Lang     string       `json:"lang"`
	From     int          `json:"from"`
	To       int          `json:"to"`
	Location string       `json:"location"` // path:from-to
	Changed  []gitx.Range `json:"changed,omitempty"`
	Content  string       `json:"content"`
	// Fingerprint identifies the finding across runs while the window's code is
	// unchanged: sha256 of path, question and the window's content.
	Fingerprint string `json:"fingerprint"`
}

// Options select and order findings.
type Options struct {
	MinP     float64  // keep findings with P >= MinP
	MinDelta float64  // diff --compare: keep findings with Delta >= MinDelta (when > 0)
	Only     []string // labels to keep (empty: all)
	Skip     []string // labels to drop
	Top      int      // at most this many (0: all)
	ByDelta  bool     // rank by Delta first
	Baseline map[string]bool
}

// Rank builds the findings of results.
func Rank(b *bundle.Bundle, res []Result, o Options) []Finding {
	var out []Finding
	for _, r := range res {
		contentSum := sha256.Sum256([]byte(r.Window.Content))
		for label, p := range r.P {
			if len(o.Only) > 0 && !slices.Contains(o.Only, label) || slices.Contains(o.Skip, label) {
				continue
			}
			if p < o.MinP {
				continue
			}
			f := Finding{CWE: CWE(label), Label: label, Title: Title(label), Question: b.Questions[label], P: p,
				Path: r.Path, Lang: r.Lang, From: r.Window.From, To: r.Window.To,
				Location: fmt.Sprintf("%s:%d-%d", r.Path, r.Window.From, r.Window.To),
				Changed:  r.Changed, Content: r.Window.Content}
			if r.Compared {
				bp := r.BaseP[label]
				d := p - bp
				if r.HasBase {
					f.BaseP = &bp
				}
				f.Delta = &d
			}
			if o.MinDelta > 0 && (f.Delta == nil || *f.Delta < o.MinDelta) {
				continue
			}
			h := sha256.New()
			fmt.Fprintf(h, "%s\x00%s\x00%x", r.Path, label, contentSum)
			f.Fingerprint = hex.EncodeToString(h.Sum(nil))[:32]
			if o.Baseline[f.Fingerprint] {
				continue
			}
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b Finding) int {
		if o.ByDelta && a.Delta != nil && b.Delta != nil {
			if c := cmp.Compare(*b.Delta, *a.Delta); c != 0 {
				return c
			}
		}
		return cmp.Or(cmp.Compare(b.P, a.P), strings.Compare(a.Path, b.Path), cmp.Compare(a.From, b.From),
			strings.Compare(a.Label, b.Label))
	})
	if o.Top > 0 && len(out) > o.Top {
		out = out[:o.Top]
	}
	return out
}

// CWE is a label's CWE id ("cwe_89" -> "CWE-89").
func CWE(label string) string {
	if n, ok := strings.CutPrefix(label, "cwe_"); ok {
		return "CWE-" + n
	}
	return label
}

// Label parses a CWE as a user writes it ("CWE-89", "cwe_89", "89") into a label.
func Label(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "cwe-"), "cwe_")
	return "cwe_" + s
}

// Title is a label's short name.
func Title(label string) string {
	if t, ok := titles[label]; ok {
		return t
	}
	return CWE(label)
}

var titles = map[string]string{
	"cwe_20":  "Improper input validation",
	"cwe_22":  "Path traversal",
	"cwe_77":  "Command injection",
	"cwe_78":  "OS command injection",
	"cwe_79":  "Cross-site scripting",
	"cwe_89":  "SQL injection",
	"cwe_94":  "Code injection",
	"cwe_119": "Memory buffer bounds",
	"cwe_125": "Out-of-bounds read",
	"cwe_190": "Integer overflow",
	"cwe_200": "Sensitive information exposure",
	"cwe_269": "Improper privilege management",
	"cwe_287": "Improper authentication",
	"cwe_306": "Missing authentication",
	"cwe_352": "Cross-site request forgery",
	"cwe_400": "Uncontrolled resource consumption",
	"cwe_416": "Use after free",
	"cwe_434": "Unrestricted file upload",
	"cwe_476": "NULL pointer dereference",
	"cwe_502": "Unsafe deserialization",
	"cwe_787": "Out-of-bounds write",
	"cwe_798": "Hard-coded credentials",
	"cwe_862": "Missing authorization",
	"cwe_863": "Incorrect authorization",
	"cwe_918": "Server-side request forgery",
}
