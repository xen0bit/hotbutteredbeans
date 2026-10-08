package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/xen0bit/hotbutteredbeans/internal/assets"
	"github.com/xen0bit/hotbutteredbeans/internal/buildinfo"
	"github.com/xen0bit/hotbutteredbeans/internal/config"
	"github.com/xen0bit/hotbutteredbeans/internal/embedded"
	"github.com/xen0bit/hotbutteredbeans/internal/hook"
	"github.com/xen0bit/hotbutteredbeans/internal/scan"
)

// --- hook ---

func hookCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Install hbb as a git hook",
		Long: `Install hbb as a git hook in the current repository. The pre-commit hook ranks the
staged changes; the pre-push hook ranks the commits being pushed. Hooks report and let
the commit through unless fail_at is set in .hbb.yaml; skip once with HBB_SKIP=1 or
git's --no-verify. An existing hook is kept and runs first.

For the pre-commit framework, add to .pre-commit-config.yaml:

  - repo: https://github.com/xen0bit/hotbutteredbeans
    rev: <version>
    hooks: [{id: hbb}]`,
	}
	var force, noFetch bool
	install := &cobra.Command{
		Use:   "install [pre-commit|pre-push...]",
		Short: "Install hooks (default: pre-commit)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			if s.repo == nil {
				return errors.New("not in a git repository")
			}
			dir, err := s.repo.HooksDir()
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				exe = "hbb"
			}
			if len(args) == 0 {
				args = []string{"pre-commit"}
			}
			for _, k := range args {
				note, err := hook.Install(dir, k, exe, force)
				if err != nil {
					return err
				}
				fmt.Println(note)
			}
			if noFetch {
				return nil
			}
			// A hook never downloads by default, so fetch now, once.
			m, err := s.model(cmd.Context())
			if err != nil {
				s.note("could not fetch the model now (%v); hooks skip the check until `hbb model fetch` succeeds", err)
				return nil
			}
			if _, err := s.open(cmd.Context(), m); err != nil {
				s.note("could not load the runtime now (%v); see hbb doctor", err)
			}
			fmt.Printf("model ready: %s (%s)\n", m.Ref, m.Source)
			return nil
		},
	}
	install.Flags().BoolVar(&force, "force", false, "replace an existing hook (it is kept as <hook>.hbb-backup) instead of chaining it")
	install.Flags().BoolVar(&noFetch, "no-fetch", false, "do not fetch the model and runtime now")
	uninstall := &cobra.Command{
		Use:   "uninstall [pre-commit|pre-push...]",
		Short: "Remove hbb's hooks (default: both)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			if s.repo == nil {
				return errors.New("not in a git repository")
			}
			dir, err := s.repo.HooksDir()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				args = []string{"pre-commit", "pre-push"}
			}
			for _, k := range args {
				note, err := hook.Uninstall(dir, k)
				if err != nil {
					return err
				}
				fmt.Println(note)
			}
			return nil
		},
	}
	cmd.AddCommand(install, uninstall)
	return cmd
}

// --- model, runtime ---

func modelCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "model", Short: "Fetch, inspect and verify the model"}
	fetch := &cobra.Command{
		Use:   "fetch",
		Short: "Download the model into the cache (a no-op when it is built in)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			m, err := s.model(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("%s: %s%s\n", m.Ref, m.Source, where(m.Where))
			return nil
		},
	}
	info := &cobra.Command{
		Use:   "info",
		Short: "Show the model in use: where it comes from and what it asks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			m, err := s.model(cmd.Context())
			if err != nil {
				return err
			}
			b := m.Bundle
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "model\t%s\n", m.Ref)
			fmt.Fprintf(w, "source\t%s%s\n", m.Source, where(m.Where))
			fmt.Fprintf(w, "base model\t%s\n", b.BaseModel)
			fmt.Fprintf(w, "variants\t%s\n", strings.Join(keys(b.Models), ", "))
			fmt.Fprintf(w, "temperature\t%.4f\n", b.Temperature)
			fmt.Fprintf(w, "window\t%d lines every %d, at most %d characters, %d tokens\n", b.Window.Lines, b.Window.Stride, b.Window.BudgetChars, b.MaxLength)
			fmt.Fprintf(w, "languages\t%d (%d extensions)\n", len(values(b.Extensions)), len(b.Extensions))
			fmt.Fprintf(w, "score cache id\t%s\n", m.ID)
			w.Flush()
			fmt.Println("\nquestions:")
			for _, l := range b.Labels {
				lim := ""
				if x := b.Limits[l]; len(x.Langs) > 0 {
					lim = " [only " + strings.Join(x.Langs, ", ") + "]"
				} else if len(x.NotLangs) > 0 {
					lim = " [not " + strings.Join(x.NotLangs, ", ") + "]"
				}
				fmt.Printf("  %-8s %s%s\n           %s\n", scan.CWE(l), scan.Title(l), lim, b.Questions[l])
			}
			return nil
		},
	}
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Re-hash the cached model files against their pinned sha256",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			m, err := s.model(cmd.Context())
			if err != nil {
				return err
			}
			if m.Source != "cache" {
				fmt.Printf("%s is %s; nothing to verify\n", m.Ref, m.Source)
				return nil
			}
			if err := assets.VerifyModel(m); err != nil {
				return fmt.Errorf("%s: %w (delete %s and fetch again)", m.Ref, err, m.Where)
			}
			fmt.Printf("%s: every file matches\n", m.Ref)
			return nil
		},
	}
	path := &cobra.Command{
		Use:   "path",
		Short: "Print the cached model's folder (for HBB_MODEL_DIR or other tools)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			m, err := s.model(cmd.Context())
			if err != nil {
				return err
			}
			if m.Where == "" {
				return errors.New("the model in use is built into hbb; it has no folder")
			}
			fmt.Println(m.Where)
			return nil
		},
	}
	prune := &cobra.Command{
		Use:   "prune",
		Short: "Delete cached models other than the one in use, and stale scores",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			s.cfg.Offline = true
			keep := ""
			if m, err := s.model(cmd.Context()); err == nil {
				keep = m.Where
			}
			cache := assets.CacheDir()
			var freed int64
			repos, _ := filepath.Glob(filepath.Join(cache, "models", "*", "*"))
			for _, d := range repos {
				if d == keep || filepath.Base(d) == "refs" {
					continue
				}
				freed += du(d)
				if err := os.RemoveAll(d); err != nil {
					return err
				}
				fmt.Println("removed", d)
			}
			scores := filepath.Join(cache, "scores")
			freed += du(scores)
			os.RemoveAll(scores)
			fmt.Printf("freed %s\n", mb(freed))
			return nil
		},
	}
	cmd.AddCommand(fetch, info, verify, path, prune)
	return cmd
}

func runtimeCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "runtime", Short: "Fetch and inspect ONNX Runtime (and the GPU libraries)"}
	var gpu bool
	fetch := &cobra.Command{
		Use:   "fetch",
		Short: "Download ONNX Runtime into the cache; --gpu adds the CUDA build and NVIDIA's libraries (~1.4 GB)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			o := s.assetOptions()
			rt, err := assets.ResolveRuntime(cmd.Context(), assets.RuntimeOptions{Options: o, Lib: s.cfg.ORTLib})
			s.prog.clear()
			if err != nil {
				return err
			}
			fmt.Printf("CPU runtime: %s (%s)\n", rt.Path, rt.Source)
			if !gpu {
				return nil
			}
			if !assets.CUDAPlatform() {
				return fmt.Errorf("ONNX Runtime has no CUDA build for %s", assets.Platform())
			}
			if err := assets.FetchGPU(cmd.Context(), &o); err != nil {
				return err
			}
			s.prog.clear()
			gp := assets.FindGPU(&o)
			fmt.Printf("GPU libraries: %s\n", assets.CUDALibDir(&o))
			if !gp.Usable() {
				fmt.Printf("but the GPU is not usable: %s\n", gp.Why())
			}
			return nil
		},
	}
	fetch.Flags().BoolVar(&gpu, "gpu", false, "also fetch the CUDA build of ONNX Runtime and the CUDA 13 / cuDNN 9 libraries")
	path := &cobra.Command{
		Use:   "path",
		Short: "Print the CPU onnxruntime library hbb loads (for HBB_ORT_LIB or other tools)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			rt, err := assets.ResolveRuntime(cmd.Context(), assets.RuntimeOptions{Options: s.assetOptions(), Lib: s.cfg.ORTLib})
			s.prog.clear()
			if err != nil {
				return err
			}
			fmt.Println(rt.Path)
			return nil
		},
	}
	cmd.AddCommand(fetch, path)
	return cmd
}

// --- doctor ---

func doctorCmd(g *globals) *cobra.Command {
	var noRun bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Show what hbb will use, and why: model, runtime, device; then score a test window",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var rows [][2]string
			row := func(k, f string, a ...any) { rows = append(rows, [2]string{k, fmt.Sprintf(f, a...)}) }
			print := func() {
				s.prog.clear()
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				for _, r := range rows {
					fmt.Fprintf(w, "%s\t%s\n", r[0], r[1])
				}
				w.Flush()
			}
			fail := func(k string, err error) error {
				row(k, "FAILED: %v", err)
				print()
				return exitCode(exitError)
			}

			flavor := "slim (model and runtime fetched on first use)"
			if _, has := embedded.LoadModel(); has {
				flavor = "full (model and runtime built in)"
			}
			row("hbb", "%s %s/%s, %s", buildinfo.Ver(), runtime.GOOS, runtime.GOARCH, flavor)
			row("settings", "%s", or(s.cfg.Path, "defaults (no .hbb.yaml)"))
			row("cache", "%s", assets.CacheDir())
			if s.repo != nil {
				row("repository", "%s", s.repo.Root)
			}
			o := s.assetOptions()
			gp := assets.FindGPU(&o)
			switch {
			case gp.Usable():
				row("gpu", "NVIDIA driver %s; CUDA libraries in %s", gp.Driver, strings.Join(gp.Where, ", "))
			case gp.Driver != "":
				row("gpu", "NVIDIA driver %s, not usable: %s", gp.Driver, gp.Why())
			default:
				row("gpu", "none (%s)", or(gp.Why(), "no NVIDIA driver"))
			}
			row("device", "%s requested", s.cfg.Device)

			m, err := s.model(ctx)
			if err != nil {
				return fail("model", err)
			}
			row("model", "%s from %s%s", m.Ref, m.Source, where(m.Where))
			if noRun {
				print()
				return nil
			}
			e, err := s.open(ctx, m)
			if err != nil {
				return fail("runtime", err)
			}
			defer e.Close()
			providers, _ := e.Lib.Providers()
			row("runtime", "onnxruntime %s from %s: %s", e.Lib.Version, e.Runtime.Source, e.Runtime.Path)
			row("providers", "%s", strings.Join(providers, ", "))
			row("running on", "%s (loaded in %s)", e.Device, e.Load.Round(time.Millisecond))
			if e.Fallback != "" {
				row("why not GPU", "%s", e.Fallback)
			}
			text := e.Bundle().Window.Windows("doctor/check.py", []byte("import os\n\ndef run(cmd):\n    os.system(\"sh -c \" + cmd)\n"))[0].Text
			t := time.Now()
			logits, err := e.Logits(text) // not from the score cache: this checks the runtime
			if err != nil {
				return fail("test window", err)
			}
			b := e.Bundle()
			best, bp := "", 0.0
			for i, l := range b.Labels {
				if p := b.Probability(logits[i]); p > bp {
					best, bp = l, p
				}
			}
			row("test window", "scored in %s; top question %s p=%.2f (os.system on a concatenated string)",
				time.Since(t).Round(time.Millisecond), scan.CWE(best), bp)
			print()
			return nil
		},
	}
	cmd.Flags().BoolVar(&noRun, "no-run", false, "only resolve; do not load the model or score a test window")
	return cmd
}

// --- config ---

func configCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Write or show settings (.hbb.yaml)"}
	var force bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write a commented .hbb.yaml at the repository root (or here)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			dir := s.cwd
			if s.repo != nil {
				dir = s.repo.Root
			}
			p := filepath.Join(dir, ".hbb.yaml")
			if _, err := os.Stat(p); err == nil && !force {
				return fmt.Errorf("%s exists (--force overwrites it)", p)
			}
			if err := os.WriteFile(p, []byte(config.Template), 0o644); err != nil {
				return err
			}
			fmt.Println("wrote", p)
			return nil
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the settings in force (file, environment and flags applied)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			fmt.Printf("# from %s\n", or(s.cfg.Path, "defaults"))
			return yaml.NewEncoder(os.Stdout).Encode(s.cfg)
		},
	}
	cmd.AddCommand(initCmd, show)
	return cmd
}

// --- version ---

func versionCmd(g *globals) *cobra.Command {
	var asJSON bool
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version, build and default model",
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, _ := assets.Locked()
			info := map[string]any{
				"version": buildinfo.Ver(), "commit": buildinfo.Rev(), "date": buildinfo.Date,
				"go": runtime.Version(), "platform": assets.Platform(),
				"model": assets.DefaultModel().String(), "onnxruntime": l.ONNXRuntime.Version, "full": false,
			}
			if m, ok := embedded.LoadModel(); ok {
				info["full"] = true
				info["model"] = assets.ModelRef{Repo: m.Manifest.Repo, Revision: m.Manifest.Revision, Variant: m.Manifest.Variant}.String() + " (built in)"
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(info)
			}
			fmt.Printf("hbb %s", info["version"])
			if c := buildinfo.Rev(); c != "" {
				fmt.Printf(" (%.12s)", c)
			}
			fmt.Printf(" %s %s\nmodel %s\nonnxruntime %s\n", info["platform"], info["go"], info["model"], info["onnxruntime"])
			return nil
		},
	}
}

// --- windows (debugging) ---

func windowsCmd(g *globals) *cobra.Command {
	var dump string
	cmd := &cobra.Command{
		Use:    "windows <file>...",
		Short:  "Show how files are cut into windows and tokens (no model run)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := g.session(cmd, "")
			if err != nil {
				return err
			}
			m, err := s.model(cmd.Context())
			if err != nil {
				return err
			}
			b := m.Bundle
			var enc *json.Encoder
			if dump != "" {
				f, err := os.Create(dump)
				if err != nil {
					return err
				}
				defer f.Close()
				enc = json.NewEncoder(f)
			}
			for _, a := range args {
				data, err := os.ReadFile(a)
				if err != nil {
					return err
				}
				relp := filepath.ToSlash(a)
				if s.repo != nil {
					if abs, err := filepath.Abs(a); err == nil {
						if r := rel(s.repo.Root, abs); r != "" {
							relp = r
						}
					}
				}
				for _, w := range b.Window.Windows(relp, data) {
					ids := b.Tokenizer.Encode(w.Text, b.MaxLength)
					if enc != nil {
						h := sha256.Sum256([]byte(w.Text))
						c := sha256.Sum256([]byte(w.Content))
						enc.Encode(map[string]any{"path": relp, "lang": b.LangOf(relp), "from": w.From, "to": w.To,
							"sha256": hex.EncodeToString(h[:]), "content_sha256": hex.EncodeToString(c[:]), "ids": ids})
						continue
					}
					fmt.Printf("%s:%d-%d  %d chars, %d tokens\n", relp, w.From, w.To, len([]rune(w.Text)), len(ids))
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dump, "dump", "", "write each window's hashes and token ids (JSON lines), as secjev's conformance/dump.py does")
	return cmd
}

// --- helpers ---

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func values(m map[string]string) map[string]bool {
	out := map[string]bool{}
	for _, v := range m {
		out[v] = true
	}
	return out
}

func du(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n
}
