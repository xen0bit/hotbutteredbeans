package scan

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xen0bit/hotbutteredbeans/internal/gitx"
	"github.com/xen0bit/hotbutteredbeans/internal/source"
	"github.com/xen0bit/hotbutteredbeans/internal/window"
)

// FromDiff must pick the model's own tiles that contain changed lines, never windows
// re-centred on the change, and pair them with the base tiles the same hunks touched.
func TestFromDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	var lines []string
	for i := 1; i <= 600; i++ {
		lines = append(lines, fmt.Sprintf("x%d = %d", i, i))
	}
	write := func() {
		os.WriteFile(filepath.Join(dir, "big.py"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	git("init", "-q", "-b", "main")
	write()
	git("add", ".")
	git("commit", "-qm", "one")
	lines[300-1] = "x300 = eval(input())" // in the second tile (241-480)
	write()

	r, err := gitx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	base, head := gitx.Side{Rev: "HEAD"}, gitx.Side{WorkTree: true}
	changes, err := r.Diff(base, head)
	if err != nil {
		t.Fatal(err)
	}
	f := &source.Filter{Extensions: map[string]string{"py": "python"}}
	ts, n, _, err := FromDiff(r, changes, base, head, window.DefaultRules, f, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || len(ts) != 1 {
		t.Fatalf("got %d targets from %d files, want 1 from 1", len(ts), n)
	}
	tg := ts[0]
	if tg.Window.From != 241 || tg.Window.To != 480 || !strings.HasPrefix(tg.Window.Text, "// ==== big.py:241-480 of 600 ====") {
		t.Errorf("target window %d-%d %.40q", tg.Window.From, tg.Window.To, tg.Window.Text)
	}
	if len(tg.Changed) != 1 || tg.Changed[0] != (gitx.Range{From: 300, To: 300}) {
		t.Errorf("changed %v", tg.Changed)
	}
	if !tg.HasBase || len(tg.Base) != 1 || tg.Base[0].From != 241 || strings.Contains(tg.Base[0].Text, "eval") {
		t.Errorf("base windows %+v", tg.Base)
	}
}

func TestLabels(t *testing.T) {
	for in, want := range map[string]string{"CWE-89": "cwe_89", "cwe_89": "cwe_89", "89": "cwe_89", " cwe-79 ": "cwe_79"} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q", in, got)
		}
	}
	if CWE("cwe_89") != "CWE-89" {
		t.Error("CWE")
	}
}
