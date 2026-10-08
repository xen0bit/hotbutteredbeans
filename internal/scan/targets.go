package scan

import (
	"os"

	"github.com/xen0bit/hotbutteredbeans/internal/gitx"
	"github.com/xen0bit/hotbutteredbeans/internal/source"
	"github.com/xen0bit/hotbutteredbeans/internal/window"
)

// Skipped counts files left out, by reason.
type Skipped map[string]int

// FromFiles cuts files into windows: every window of every file.
func FromFiles(files []source.File, rules window.Rules, f *source.Filter) ([]Target, int, Skipped) {
	var out []Target
	skipped := Skipped{}
	n := 0
	for _, file := range files {
		data, err := os.ReadFile(file.Full)
		if err != nil {
			skipped["unreadable"]++
			continue
		}
		if why := f.Skip(data); why != "" {
			skipped[why]++
			continue
		}
		if rules.IsBinary(data) {
			skipped["binary"]++
			continue
		}
		n++
		for _, w := range rules.Windows(file.Rel, data) {
			out = append(out, Target{Path: file.Rel, Lang: file.Lang, Window: w})
		}
	}
	return out, n, skipped
}

// FromDiff picks, in each changed file, the windows the changes touch. The windows are
// the model's own tiles of the whole file (from line 1, every 240 lines), never windows
// re-centred on a change: the model has only seen the tiles. With compare, each target
// also gets the base version's tiles that the same changes touched.
func FromDiff(repo *gitx.Repo, changes []gitx.Change, base, head gitx.Side, rules window.Rules, f *source.Filter, compare bool) ([]Target, int, Skipped, error) {
	var out []Target
	skipped := Skipped{}
	n := 0
	for _, c := range changes {
		lang := f.Lang(c.Path)
		if lang == "" {
			continue
		}
		data, err := repo.Read(head, c.Path)
		if err != nil {
			skipped["unreadable"]++
			continue
		}
		if why := f.Skip(data); why != "" {
			skipped[why]++
			continue
		}
		if rules.IsBinary(data) {
			skipped["binary"]++
			continue
		}
		var baseWins []window.Window
		hasBase := false
		if compare && c.OldPath != "" && base.Rev != gitx.EmptyTree {
			if old, err := repo.Read(base, c.OldPath); err == nil && !rules.IsBinary(old) {
				// The base keeps its own header path, as it was trained: a renamed
				// file's old windows name the old path.
				baseWins = rules.Windows(c.OldPath, old)
				hasBase = true
			}
		}
		n++
		for _, w := range rules.Windows(c.Path, data) {
			var changed []gitx.Range
			var bw []window.Window
			for _, h := range c.Hunks {
				nr := h.New()
				if !w.Overlaps(nr.From, nr.To) {
					continue
				}
				changed = append(changed, gitx.Range{From: max(nr.From, w.From), To: min(nr.To, w.To)})
				if hasBase {
					or := h.Old()
					for _, b := range baseWins {
						if b.Overlaps(or.From, or.To) && !containsWindow(bw, b) {
							bw = append(bw, b)
						}
					}
				}
			}
			if len(changed) > 0 {
				out = append(out, Target{Path: c.Path, Lang: lang, Window: w, Changed: changed, Base: bw, HasBase: hasBase})
			}
		}
	}
	return out, n, skipped, nil
}

func containsWindow(ws []window.Window, w window.Window) bool {
	for _, x := range ws {
		if x.From == w.From && x.To == w.To {
			return true
		}
	}
	return false
}
