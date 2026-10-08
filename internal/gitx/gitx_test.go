package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseDiff(t *testing.T) {
	out := []byte(`diff --git a/x.go b/x.go
index 1..2 100644
--- a/x.go
+++ b/x.go
@@ -3 +3,2 @@ func f() {
-a
+b
+c
@@ -10,2 +11,0 @@
-d
-e
diff --git a/old.py b/new.py
similarity index 90%
rename from old.py
rename to new.py
--- a/old.py
+++ b/new.py
@@ -1 +1 @@
-x
+y
diff --git "a/\303\244.js" "b/\303\244.js"
new file mode 100644
--- /dev/null
+++ "b/\303\244.js"
@@ -0,0 +1,3 @@
+1
+2
+3
diff --git a/bin.png b/bin.png
Binary files a/bin.png and b/bin.png differ
`)
	got, err := parseDiff(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d changes, want 3: %+v", len(got), got)
	}
	x := got[0]
	if x.Path != "x.go" || x.OldPath != "x.go" || !slices.Equal(x.Hunks, []Hunk{{3, 1, 3, 2}, {10, 2, 11, 0}}) {
		t.Errorf("x.go: %+v", x)
	}
	if r := x.Hunks[1].New(); r != (Range{11, 12}) {
		t.Errorf("a deletion touches the lines around it: got %v", r)
	}
	if got[1].Path != "new.py" || got[1].OldPath != "old.py" {
		t.Errorf("rename: %+v", got[1])
	}
	if got[2].Path != "ä.js" || got[2].OldPath != "" || got[2].Hunks[0].New() != (Range{1, 3}) {
		t.Errorf("quoted new file: %+v", got[2])
	}
}

func TestRepo(t *testing.T) {
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
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	write("a.py", "1\n2\n3\n")
	git("add", ".")
	git("commit", "-qm", "one")
	write("a.py", "1\nTWO\n3\n")
	git("add", "a.py")
	write("b.py", "new\n")
	write(".gitignore", "ignored.py\n")
	write("ignored.py", "x\n")

	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := r.Diff(Side{Rev: "HEAD"}, Side{Index: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 || staged[0].Path != "a.py" || staged[0].Hunks[0].New() != (Range{2, 2}) {
		t.Errorf("staged: %+v", staged)
	}
	work, err := r.Diff(Side{Rev: "HEAD"}, Side{WorkTree: true})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range work {
		paths = append(paths, c.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{".gitignore", "a.py", "b.py"}) {
		t.Errorf("work tree: %v (untracked files count, ignored ones do not)", paths)
	}
	if b, err := r.Read(Side{Index: true}, "a.py"); err != nil || string(b) != "1\nTWO\n3\n" {
		t.Errorf("index read: %q %v", b, err)
	}
	if b, err := r.Read(Side{Rev: "HEAD"}, "a.py"); err != nil || string(b) != "1\n2\n3\n" {
		t.Errorf("HEAD read: %q %v", b, err)
	}
	files, err := r.Files(".")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(files, "ignored.py") || !slices.Contains(files, "b.py") {
		t.Errorf("files: %v", files)
	}
}
