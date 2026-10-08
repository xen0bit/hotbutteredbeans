// Package window cuts source files into the windows the secjev encoder was trained on.
// Every byte of a window's text matters: the model has only ever seen this exact shape,
// so the rules here reproduce the Python reference (secjev's scan.py) exactly, and the
// conformance tests hold them to it.
package window

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Window is lines From..To (1-based, inclusive) of a file. Text is what the model reads
// (a header, numbered lines, long lines cut); Content is the lines as they are in the file.
type Window struct {
	From, To int
	Text     string
	Content  string
}

// Overlaps reports whether the window shares a line with from..to.
func (w Window) Overlaps(from, to int) bool { return w.From <= to && from <= w.To }

// Rules are the tiling constants (bundle.json "window").
type Rules struct {
	Lines       int    `json:"lines"`
	Stride      int    `json:"stride"`
	BudgetChars int    `json:"budget_chars"`
	LineCap     int    `json:"line_cap"`
	Cut         string `json:"cut"`
	BinaryProbe int    `json:"binary_probe_bytes"`
}

// DefaultRules are the rules every secjev bundle so far was trained with.
var DefaultRules = Rules{Lines: 240, Stride: 240, BudgetChars: 33084, LineCap: 400,
	Cut: " ... [long line cut]", BinaryProbe: 8192}

// Windows cuts one file: tiles of r.Lines lines every r.Stride lines from line 1, each
// halved until its text is at most r.BudgetChars code points. Lines split on "\n" alone
// ("\r" is kept), and the empty line after a trailing newline is dropped. path is the
// path the header shows, relative to the repository root.
func (r Rules) Windows(path string, data []byte) []Window {
	lines := strings.Split(DecodeUTF8(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	var out []Window
	var fit func(start int, ls []string)
	fit = func(start int, ls []string) {
		text := r.render(path, ls, start, total)
		if len(ls) > 1 && utf8.RuneCountInString(text) > r.BudgetChars {
			h := len(ls) / 2
			fit(start, ls[:h])
			fit(start+h, ls[h:])
			return
		}
		out = append(out, Window{From: start, To: start + len(ls) - 1, Text: text, Content: strings.Join(ls, "\n")})
	}
	for start := 1; start <= total; start += r.Stride {
		fit(start, lines[start-1:min(total, start-1+r.Lines)])
	}
	return out
}

func (r Rules) render(path string, lines []string, start, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "// ==== %s:%d-%d of %d ====\n", path, start, start+len(lines)-1, total)
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d | %s", start+i, r.clip(l))
	}
	return b.String()
}

// clip cuts a line at r.LineCap code points.
func (r Rules) clip(line string) string {
	n := 0
	for i := range line {
		if n == r.LineCap {
			return line[:i] + r.Cut
		}
		n++
	}
	return line
}

// IsBinary reports whether a file is skipped: empty, or a NUL in its first r.BinaryProbe bytes.
func (r Rules) IsBinary(data []byte) bool {
	return len(data) == 0 || bytes.IndexByte(data[:min(len(data), r.BinaryProbe)], 0) >= 0
}

// DecodeUTF8 decodes like Python's bytes.decode("utf-8", errors="replace"): each maximal
// invalid subsequence becomes one U+FFFD (the Unicode "maximal subpart" rule). Go's own
// conversion replaces byte by byte, which would change the windows.
func DecodeUTF8(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	var b strings.Builder
	b.Grow(len(data) + 16)
	need, seen, lower, upper := 0, 0, byte(0x80), byte(0xBF)
	var cp rune
	reset := func() { need, seen, cp, lower, upper = 0, 0, 0, 0x80, 0xBF }
	for i := 0; i < len(data); {
		c := data[i]
		if need == 0 {
			switch {
			case c <= 0x7F:
				b.WriteByte(c)
			case c >= 0xC2 && c <= 0xDF:
				need, cp = 1, rune(c&0x1F)
			case c >= 0xE0 && c <= 0xEF:
				if c == 0xE0 {
					lower = 0xA0
				} else if c == 0xED {
					upper = 0x9F
				}
				need, cp = 2, rune(c&0x0F)
			case c >= 0xF0 && c <= 0xF4:
				if c == 0xF0 {
					lower = 0x90
				} else if c == 0xF4 {
					upper = 0x8F
				}
				need, cp = 3, rune(c&0x07)
			default:
				b.WriteRune(utf8.RuneError)
			}
			i++
			continue
		}
		if c < lower || c > upper {
			reset()
			b.WriteRune(utf8.RuneError)
			continue // the byte starts again
		}
		lower, upper = 0x80, 0xBF
		cp = cp<<6 | rune(c&0x3F)
		seen++
		i++
		if seen == need {
			b.WriteRune(cp)
			reset()
		}
	}
	if need != 0 {
		b.WriteRune(utf8.RuneError)
	}
	return b.String()
}
