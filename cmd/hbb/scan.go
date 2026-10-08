package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/xen0bit/hotbutteredbeans/internal/assets"
	"github.com/xen0bit/hotbutteredbeans/internal/gitx"
	"github.com/xen0bit/hotbutteredbeans/internal/scan"
	"github.com/xen0bit/hotbutteredbeans/internal/source"
)

func scanCmd(g *globals) *cobra.Command {
	var o outFlags
	var stdin bool
	var name, lang string
	cmd := &cobra.Command{
		Use:   "scan [path...]",
		Short: "Rank every window of a tree, or of some files",
		Long: `Cut every source file under the paths (default: the current folder) into the model's
windows, score them, and list the (window, CWE) pairs most likely to hold a weakness.

In a git repository, paths in window headers are relative to the repository root and
.gitignore is respected. On the CPU, a whole repository takes minutes; hbb diff is the
fast path for changes, and scores are cached, so a second scan only scores what changed.`,
		Example: `  hbb scan
  hbb scan src/ --top 50 --cwe CWE-89,CWE-78
  hbb scan --format sarif -o hbb.sarif
  cat handler.go | hbb scan --stdin --name api/handler.go`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) > 0 && !stdin {
				dir = args[0]
				if st, err := os.Stat(dir); err == nil && !st.IsDir() {
					dir = filepath.Dir(dir)
				}
				dir, _ = filepath.Abs(dir)
			}
			s, err := g.session(cmd, dir)
			if err != nil {
				return err
			}
			if err := o.apply(cmd, &s.cfg, &s.cfg.Scan); err != nil {
				return err
			}
			ctx := cmd.Context()
			m, err := s.model(ctx)
			if err != nil {
				return err
			}
			f := filterFor(m, &s.cfg)

			if stdin {
				if name == "" {
					return errors.New("--stdin needs --name, the path the window headers show (its extension sets the language)")
				}
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return err
				}
				l := lang
				if l == "" {
					l = m.Bundle.LangOf(name)
				}
				if l == "" {
					return fmt.Errorf("%s: no language for this extension; give one with --lang", name)
				}
				var targets []scan.Target
				for _, w := range m.Bundle.Window.Windows(filepath.ToSlash(name), data) {
					targets = append(targets, scan.Target{Path: filepath.ToSlash(name), Lang: l, Window: w})
				}
				return s.finish(ctx, m, &o, job{mode: "scan", root: "-", files: 1, targets: targets, ranking: s.cfg.Scan})
			}

			if len(args) == 0 {
				args = []string{"."}
			}
			root, files, err := s.listFiles(args, f)
			if err != nil {
				return err
			}
			if lang != "" {
				for i := range files {
					files[i].Lang = lang
				}
			}
			targets, n, skipped := scan.FromFiles(files, m.Bundle.Window, f)
			if len(targets) == 0 {
				s.note("no source files to scan under %s", strings.Join(args, ", "))
			}
			return s.finish(ctx, m, &o, job{mode: "scan", root: root, files: n, targets: targets, skipped: skipped, ranking: s.cfg.Scan})
		},
	}
	o.register(cmd)
	cmd.Flags().BoolVar(&stdin, "stdin", false, "read one file from standard input")
	cmd.Flags().StringVar(&name, "name", "", "with --stdin: the file's path, for its header and language")
	cmd.Flags().StringVar(&lang, "lang", "", "read every file as this language (e.g. python), whatever its extension")
	return cmd
}

// listFiles resolves scan's path arguments to files. Folders are walked (through git in
// a repository, honouring .gitignore); files named explicitly are always read when the
// model knows their language. The root is the repository's, else the single folder
// given, else the working folder.
func (s *session) listFiles(args []string, f *source.Filter) (string, []source.File, error) {
	root := s.cwd
	if s.repo != nil {
		root = s.repo.Root
	} else if len(args) == 1 {
		if st, err := os.Stat(args[0]); err == nil && st.IsDir() {
			root, _ = filepath.Abs(args[0])
		}
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	var files []source.File
	seen := map[string]bool{}
	add := func(fs ...source.File) {
		for _, x := range fs {
			if !seen[x.Rel] {
				seen[x.Rel] = true
				files = append(files, x)
			}
		}
	}
	for _, a := range args {
		abs, err := filepath.Abs(a)
		if err != nil {
			return "", nil, err
		}
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			abs = r
		}
		st, err := os.Stat(abs)
		if err != nil {
			return "", nil, err
		}
		r := rel(root, abs)
		if r == "" && abs != root {
			return "", nil, fmt.Errorf("%s is outside %s; scan it on its own", a, root)
		}
		if !st.IsDir() {
			lang := f.Extensions[strings.ToLower(strings.TrimPrefix(filepath.Ext(abs), "."))]
			if lang == "" {
				s.note("%s: the model reads no language with this extension; skipped (--lang sets one)", a)
				continue
			}
			add(source.File{Rel: r, Full: abs, Lang: lang})
			continue
		}
		if s.repo != nil && s.cfg.Gitignore {
			spec := r
			if spec == "" {
				spec = "."
			}
			list, err := s.repo.Files(spec)
			if err != nil {
				return "", nil, err
			}
			add(source.FromList(root, list, f)...)
			continue
		}
		list, err := source.Walk(root, abs, f)
		if err != nil {
			return "", nil, err
		}
		add(list...)
	}
	return root, files, nil
}

func diffCmd(g *globals) *cobra.Command {
	var o outFlags
	var staged, compare, noCompare, hookMode, prePush bool
	cmd := &cobra.Command{
		Use:   "diff [<base> | <base>..<head> | <base>...<head>] [-- <path>...]",
		Short: "Rank only the windows a change touched",
		Long: `Score the model's windows that contain changed lines. The windows are the same tiles a
full scan uses (every 240 lines from line 1), so a window's score does not depend on
where the change fell in it.

  hbb diff                  the work tree (staged, unstaged and untracked) against HEAD
  hbb diff --staged         the index against HEAD: what the next commit holds
  hbb diff main             the work tree against main
  hbb diff main...HEAD      the branch since it left main (what a pull request shows)
  hbb diff A..B             commit A against commit B

--compare also scores the base version of the same code and reports how much each
score rose: the model was trained to score code before a security fix above the code
after it, so a rise is its strongest signal. Ranking is then by the rise.`,
		Example: `  hbb diff --staged
  hbb diff origin/main...HEAD --compare --format llm --max-chars 40000
  hbb diff origin/main...HEAD --format sarif -o hbb.sarif --fail-at 0.9`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			s.hook = hookMode
			if err := o.apply(cmd, &s.cfg, &s.cfg.Diff); err != nil {
				return err
			}
			if cmd.Flags().Changed("compare") {
				s.cfg.Diff.Compare = compare
			} else if hookMode {
				s.cfg.Diff.Compare = s.cfg.Hook.Compare
			}
			if noCompare {
				s.cfg.Diff.Compare = false
			}
			err = s.runDiff(cmd, args, &o, staged, prePush)
			if err != nil && hookMode {
				var code exitCode
				if errors.As(err, &code) {
					return err
				}
				// A hook fails open: a missing model or a broken runtime must not block commits.
				if errors.Is(err, assets.ErrOffline) {
					s.note("the model is not downloaded yet, so this commit was not checked; run `hbb model fetch` once (or set hook.fetch: true)")
				} else {
					s.note("skipping the security triage: %v", err)
				}
				return nil
			}
			return err
		},
	}
	o.register(cmd)
	f := cmd.Flags()
	f.BoolVar(&staged, "staged", false, "the index against HEAD (alias --cached)")
	f.BoolVar(&staged, "cached", false, "same as --staged")
	f.BoolVar(&compare, "compare", false, "also score the base version and report each score's rise")
	f.BoolVar(&noCompare, "no-compare", false, "do not compare, whatever the settings say")
	f.BoolVar(&hookMode, "hook", false, "run as a git hook: report to stderr, never download unless hook.fetch, never block on errors")
	f.BoolVar(&prePush, "pre-push", false, "read the refs being pushed from stdin, as git's pre-push hook passes them")
	f.MarkHidden("cached")
	return cmd
}

func (s *session) runDiff(cmd *cobra.Command, args []string, o *outFlags, staged, prePush bool) error {
	if s.repo == nil {
		return errors.New("hbb diff needs a git repository (hbb scan reads any folder)")
	}
	ctx := cmd.Context()
	var paths []string
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		paths, args = args[dash:], args[:dash]
	}
	for i, p := range paths { // pathspecs are relative to the root for git -C root
		abs, _ := filepath.Abs(p)
		if r := rel(s.repo.Root, abs); r != "" {
			paths[i] = r
		}
	}
	if len(args) > 1 {
		return errors.New("give one revision or range")
	}

	type pair struct{ base, head gitx.Side }
	var pairs []pair
	switch {
	case prePush && os.Getenv("PRE_COMMIT_FROM_REF") != "" && os.Getenv("PRE_COMMIT_TO_REF") != "":
		// the pre-commit framework passes the pushed range in its environment
		b, err := s.repo.Resolve(os.Getenv("PRE_COMMIT_FROM_REF"))
		if err != nil {
			return err
		}
		h, err := s.repo.Resolve(os.Getenv("PRE_COMMIT_TO_REF"))
		if err != nil {
			return err
		}
		pairs = []pair{{gitx.Side{Rev: b}, gitx.Side{Rev: h}}}
	case prePush:
		ps, err := s.pushPairs(os.Stdin)
		if err != nil {
			return err
		}
		for _, p := range ps {
			pairs = append(pairs, pair{p[0], p[1]})
		}
	case staged:
		if len(args) > 0 {
			return errors.New("--staged compares the index with HEAD; it takes no revision")
		}
		pairs = []pair{{s.headOrEmpty(), gitx.Side{Index: true}}}
	case len(args) == 0:
		pairs = []pair{{s.headOrEmpty(), gitx.Side{WorkTree: true}}}
	default:
		b, h, err := s.parseRange(args[0])
		if err != nil {
			return err
		}
		pairs = []pair{{b, h}}
	}
	if s.hook {
		s.cmd.SetOut(os.Stderr)
	}

	m, err := s.model(ctx)
	if err != nil {
		return err
	}
	f := filterFor(m, &s.cfg)
	var targets []scan.Target
	skipped := scan.Skipped{}
	files := 0
	seen := map[string]bool{}
	for _, p := range pairs {
		changes, err := s.repo.Diff(p.base, p.head, paths...)
		if err != nil {
			return err
		}
		ts, n, sk, err := scan.FromDiff(s.repo, changes, p.base, p.head, m.Bundle.Window, f, s.cfg.Diff.Compare)
		if err != nil {
			return err
		}
		files += n
		for k, v := range sk {
			skipped[k] += v
		}
		for _, t := range ts {
			key := fmt.Sprintf("%s:%d-%d", t.Path, t.Window.From, t.Window.To)
			if !seen[key] {
				seen[key] = true
				targets = append(targets, t)
			}
		}
	}
	j := job{mode: "diff", root: s.repo.Root, compare: s.cfg.Diff.Compare, files: files, targets: targets,
		skipped: skipped, ranking: s.cfg.Diff}
	if len(pairs) == 1 {
		j.base, j.head = pairs[0].base.String(), pairs[0].head.String()
	}
	if s.hook {
		j.out = os.Stderr
		if len(targets) == 0 {
			return nil // nothing the model reads changed: stay silent
		}
	}
	return s.finish(ctx, m, o, j)
}

func (s *session) headOrEmpty() gitx.Side {
	if s.repo.HasHead() {
		return gitx.Side{Rev: "HEAD"}
	}
	return gitx.Side{Rev: gitx.EmptyTree}
}

// parseRange reads "A", "A..B" or "A...B" (git's meanings).
func (s *session) parseRange(r string) (gitx.Side, gitx.Side, error) {
	if a, b, ok := strings.Cut(r, "..."); ok {
		a, b = or(a, "HEAD"), or(b, "HEAD")
		head, err := s.repo.Resolve(b)
		if err != nil {
			return gitx.Side{}, gitx.Side{}, err
		}
		mb, err := s.repo.MergeBase(a, head)
		if err != nil {
			return gitx.Side{}, gitx.Side{}, fmt.Errorf("no common ancestor of %s and %s (a shallow clone? fetch with --depth 0 or fetch-depth: 0): %w", a, b, err)
		}
		return gitx.Side{Rev: mb}, gitx.Side{Rev: head}, nil
	}
	if a, b, ok := strings.Cut(r, ".."); ok {
		base, err := s.repo.Resolve(or(a, "HEAD"))
		if err != nil {
			return gitx.Side{}, gitx.Side{}, err
		}
		head, err := s.repo.Resolve(or(b, "HEAD"))
		if err != nil {
			return gitx.Side{}, gitx.Side{}, err
		}
		return gitx.Side{Rev: base}, gitx.Side{Rev: head}, nil
	}
	base, err := s.repo.Resolve(r)
	if err != nil {
		return gitx.Side{}, gitx.Side{}, err
	}
	return gitx.Side{Rev: base}, gitx.Side{WorkTree: true}, nil
}

const zeroSHA = "0000000000000000000000000000000000000000"

// pushPairs reads git's pre-push input ("<local ref> <local sha> <remote ref> <remote
// sha>" per line) into (base, head) pairs. A new branch is compared with where it left
// the remote's default branch.
func (s *session) pushPairs(r io.Reader) ([][2]gitx.Side, error) {
	var out [][2]gitx.Side
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 || f[1] == zeroSHA || strings.Repeat("0", len(f[1])) == f[1] {
			continue // a deletion pushes no code
		}
		local, remote := f[1], f[3]
		if remote == zeroSHA || strings.Repeat("0", len(remote)) == remote {
			base := ""
			for _, ref := range []string{"refs/remotes/origin/HEAD", "refs/remotes/origin/main", "refs/remotes/origin/master"} {
				if mb, err := s.repo.MergeBase(ref, local); err == nil {
					base = mb
					break
				}
			}
			if base == "" {
				s.note("%s: a new branch with no known base; not checked", f[0])
				continue
			}
			remote = base
		} else if _, err := s.repo.Resolve(remote); err != nil {
			s.note("%s: the remote's commit %.12s is not here (fetch first); not checked", f[0], remote)
			continue
		}
		out = append(out, [2]gitx.Side{{Rev: remote}, {Rev: local}})
	}
	return out, sc.Err()
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
