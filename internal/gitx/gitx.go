// Package gitx is what hbb asks of git: the repository root, the files git tracks, the
// changes between two states and the file contents on each side. It runs the git
// command, which every machine with a repository has.
package gitx

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ErrNotRepo is returned outside a git repository.
var ErrNotRepo = errors.New("not a git repository")

// Repo is a work tree.
type Repo struct {
	Root string
}

func run(dir string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.quotepath=off", "-c", "color.ui=never"}, args...)...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("git is not installed (or not on PATH)")
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(firstLine(stderr.String(), err.Error())))
	}
	return out, nil
}

func firstLine(s, def string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Open finds the repository containing dir.
func Open(dir string) (*Repo, error) {
	out, err := run(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return nil, ErrNotRepo
		}
		return nil, err
	}
	root := strings.TrimSpace(string(out))
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return &Repo{Root: filepath.FromSlash(root)}, nil
}

// Files lists the files git knows under the given paths (relative to the root): tracked
// ones and untracked ones .gitignore does not exclude. Paths are slash-separated and
// relative to the root.
func (r *Repo) Files(paths ...string) ([]string, error) {
	args := []string{"ls-files", "-z", "--cached", "--others", "--exclude-standard", "--deduplicate", "--"}
	out, err := run(r.Root, nil, append(args, paths...)...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}

// HooksDir is where git runs hooks from (core.hooksPath aware).
func (r *Repo) HooksDir() (string, error) {
	out, err := run(r.Root, nil, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Root, p)
	}
	return p, nil
}

// Resolve turns a revision into a commit id.
func (r *Repo) Resolve(rev string) (string, error) {
	out, err := run(r.Root, nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("unknown revision %q", rev)
	}
	return strings.TrimSpace(string(out)), nil
}

// MergeBase is the best common ancestor of a and b.
func (r *Repo) MergeBase(a, b string) (string, error) {
	out, err := run(r.Root, nil, "merge-base", a, b)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// HasHead reports whether HEAD exists (false before the first commit).
func (r *Repo) HasHead() bool {
	_, err := r.Resolve("HEAD")
	return err == nil
}

// EmptyTree is git's empty tree, the base of a repository's first commit.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Side is one side of a comparison: a commit, the index, or the work tree.
type Side struct {
	Rev      string // a commit id; "" with Index or WorkTree
	Index    bool
	WorkTree bool
}

func (s Side) String() string {
	switch {
	case s.Index:
		return "index"
	case s.WorkTree:
		return "work tree"
	}
	if len(s.Rev) > 12 {
		return s.Rev[:12]
	}
	return s.Rev
}

// Read returns a file's contents on this side.
func (r *Repo) Read(s Side, path string) ([]byte, error) {
	if s.WorkTree {
		return os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(path)))
	}
	spec := ":" + path
	if !s.Index {
		spec = s.Rev + ":" + path
	}
	return run(r.Root, nil, "cat-file", "blob", spec)
}

// Range is lines From..To, inclusive (1-based).
type Range struct{ From, To int }

// Hunk is one change: OldStart/OldLines on the base side, NewStart/NewLines on the head
// side, as git's "@@ -a,b +c,d @@" header gives them.
type Hunk struct {
	OldStart, OldLines, NewStart, NewLines int
}

// New is the head-side lines the hunk touches; a pure deletion touches the lines around
// the place it removed lines from.
func (h Hunk) New() Range {
	if h.NewLines > 0 {
		return Range{h.NewStart, h.NewStart + h.NewLines - 1}
	}
	return Range{max(1, h.NewStart), h.NewStart + 1}
}

// Old is the base-side lines the hunk touches, likewise.
func (h Hunk) Old() Range {
	if h.OldLines > 0 {
		return Range{h.OldStart, h.OldStart + h.OldLines - 1}
	}
	return Range{max(1, h.OldStart), h.OldStart + 1}
}

// Change is one changed file.
type Change struct {
	Path    string // on the head side
	OldPath string // on the base side ("" for an added file)
	Hunks   []Hunk
	Binary  bool
}

// Diff lists what changed from base to head, with hunks at zero context (git diff
// -U0): added, copied, modified and renamed files; deletions have nothing to scan.
func (r *Repo) Diff(base, head Side, paths ...string) ([]Change, error) {
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--unified=0", "--diff-filter=ACMR", "-M",
		"--src-prefix=a/", "--dst-prefix=b/"}
	switch {
	case head.Index:
		args = append(args, "--cached", base.Rev)
	case head.WorkTree:
		args = append(args, base.Rev)
	default:
		args = append(args, base.Rev, head.Rev)
	}
	out, err := run(r.Root, nil, append(append(args, "--"), paths...)...)
	if err != nil {
		return nil, err
	}
	changes, err := parseDiff(out)
	if err != nil {
		return nil, err
	}
	if head.WorkTree {
		// git diff leaves out untracked files; a commit hook never sees them, but a
		// review of the work tree should: they are added files.
		untracked, err := run(r.Root, nil, append([]string{"ls-files", "-z", "--others", "--exclude-standard", "--"}, paths...)...)
		if err == nil {
			for _, f := range bytes.Split(untracked, []byte{0}) {
				if len(f) == 0 {
					continue
				}
				changes = append(changes, Change{Path: string(f), Hunks: []Hunk{{NewStart: 1, NewLines: 1 << 30}}})
			}
		}
	}
	return changes, nil
}

var hunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseDiff(out []byte) ([]Change, error) {
	var changes []Change
	var cur *Change
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			changes = append(changes, Change{})
			cur = &changes[len(changes)-1]
		case cur == nil:
		case strings.HasPrefix(line, "--- "):
			if p := diffPath(line[4:], "a/"); p != "" {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			cur.Path = diffPath(line[4:], "b/")
		case strings.HasPrefix(line, "rename from "):
			cur.OldPath = unquote(line[len("rename from "):])
		case strings.HasPrefix(line, "rename to "):
			cur.Path = unquote(line[len("rename to "):])
		case strings.HasPrefix(line, "copy to "):
			cur.Path = unquote(line[len("copy to "):])
		case strings.HasPrefix(line, "Binary files "):
			cur.Binary = true
		case strings.HasPrefix(line, "@@ "):
			m := hunkRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("unreadable hunk header %q", line)
			}
			n := func(s string, def int) int {
				if s == "" {
					return def
				}
				v, _ := strconv.Atoi(s)
				return v
			}
			cur.Hunks = append(cur.Hunks, Hunk{n(m[1], 0), n(m[2], 1), n(m[3], 0), n(m[4], 1)})
		}
	}
	// Drop entries with no head path (a pure mode change has no +++ line) and binaries.
	out2 := changes[:0]
	for _, c := range changes {
		if c.Path != "" && !c.Binary && len(c.Hunks) > 0 {
			out2 = append(out2, c)
		}
	}
	return out2, sc.Err()
}

func diffPath(s, prefix string) string {
	s = unquote(strings.TrimRight(s, "\t"))
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, prefix)
}

// unquote undoes git's C-style quoting of unusual paths ("a/\303\244.go").
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}
