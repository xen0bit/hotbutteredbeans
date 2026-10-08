package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/xen0bit/hotbutteredbeans/internal/assets"
	"github.com/xen0bit/hotbutteredbeans/internal/config"
	"github.com/xen0bit/hotbutteredbeans/internal/engine"
	"github.com/xen0bit/hotbutteredbeans/internal/gitx"
)

// globals are the flags every command takes.
type globals struct {
	configPath   string
	device       string
	threads      int
	gpuID        int
	modelDir     string
	modelRepo    string
	modelRev     string
	modelVariant string
	ortLib       string
	offline      bool
	cacheDir     string
	noColor      bool
	quiet        bool
	verbose      bool
}

func (g *globals) register(root *cobra.Command) {
	f := root.PersistentFlags()
	f.StringVar(&g.configPath, "config", "", "settings file (default: .hbb.yaml in the repository root or a parent folder; $HBB_CONFIG)")
	f.StringVar(&g.device, "device", "", "where the model runs: auto (GPU if usable, else CPU), gpu, cpu, coreml ($HBB_DEVICE)")
	f.IntVar(&g.threads, "threads", 0, "CPU threads (0: ONNX Runtime's default; $HBB_THREADS)")
	f.IntVar(&g.gpuID, "gpu-id", 0, "CUDA device ($HBB_GPU_ID)")
	f.StringVar(&g.modelDir, "model-dir", "", "use a local model bundle folder ($HBB_MODEL_DIR)")
	f.StringVar(&g.modelRepo, "model-repo", "", "Hugging Face model repository ($HBB_MODEL_REPO)")
	f.StringVar(&g.modelRev, "model-revision", "", "model revision, a commit to pin ($HBB_MODEL_REVISION)")
	f.StringVar(&g.modelVariant, "model-variant", "", "model graph: q8 or fp32 ($HBB_MODEL_VARIANT)")
	f.StringVar(&g.ortLib, "ort-lib", "", "onnxruntime library to load ($HBB_ORT_LIB, $ONNXRUNTIME_LIB)")
	f.BoolVar(&g.offline, "offline", false, "never download; use what is built in or cached ($HBB_OFFLINE)")
	f.StringVar(&g.cacheDir, "cache-dir", "", "cache folder (default: the user cache folder/hbb; $HBB_CACHE_DIR)")
	f.BoolVar(&g.noColor, "no-color", false, "no colors ($NO_COLOR)")
	f.BoolVarP(&g.quiet, "quiet", "q", false, "no progress or notes on stderr")
	f.BoolVarP(&g.verbose, "verbose", "v", false, "explain each decision (model, runtime, device) on stderr")
}

// session is one command's settings and surroundings.
type session struct {
	g    *globals
	cmd  *cobra.Command
	cfg  config.Config
	repo *gitx.Repo // nil outside a repository
	cwd  string
	hook bool // running as a git hook: never download unless allowed, fail open
	prog *progress
}

func (g *globals) session(cmd *cobra.Command, dir string) (*session, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if dir == "" {
		dir = cwd
	}
	s := &session{g: g, cmd: cmd, cwd: cwd}
	repo, err := gitx.Open(dir)
	switch {
	case err == nil:
		s.repo = repo
	case errors.Is(err, gitx.ErrNotRepo):
	default:
		s.note("git: %v", err)
	}
	if g.configPath != "" {
		os.Setenv("HBB_CONFIG", g.configPath)
	}
	stop := ""
	if s.repo != nil {
		stop = s.repo.Root
	}
	if s.cfg, err = config.Load(dir, stop); err != nil {
		return nil, err
	}
	pf := cmd.Flags()
	set := func(name string, apply func()) {
		if pf.Changed(name) {
			apply()
		}
	}
	set("device", func() { s.cfg.Device = g.device })
	set("threads", func() { s.cfg.Threads = g.threads })
	set("gpu-id", func() { s.cfg.GPUID = g.gpuID })
	set("model-dir", func() { s.cfg.Model.Dir = g.modelDir })
	set("model-repo", func() { s.cfg.Model.Repo = g.modelRepo })
	set("model-revision", func() { s.cfg.Model.Revision = g.modelRev })
	set("model-variant", func() { s.cfg.Model.Variant = g.modelVariant })
	set("ort-lib", func() { s.cfg.ORTLib = g.ortLib })
	set("offline", func() { s.cfg.Offline = g.offline })
	if g.cacheDir != "" {
		os.Setenv("HBB_CACHE_DIR", g.cacheDir)
	}
	if _, err := engine.ParseDevice(s.cfg.Device); err != nil {
		return nil, err
	}
	s.prog = &progress{on: !g.quiet && term.IsTerminal(int(os.Stderr.Fd()))}
	if g.verbose && s.cfg.Path != "" {
		s.note("settings from %s", s.cfg.Path)
	}
	return s, nil
}

// note prints to stderr unless --quiet.
func (s *session) note(format string, args ...any) {
	if s.g.quiet {
		return
	}
	s.prog.clear()
	fmt.Fprintf(os.Stderr, "hbb: "+format+"\n", args...)
}

func (s *session) verbose(format string, args ...any) {
	if s.g.verbose {
		s.note(format, args...)
	}
}

func (s *session) assetOptions() assets.Options {
	offline := s.cfg.Offline
	if s.hook && !s.cfg.Hook.Fetch {
		offline = true
	}
	return assets.Options{
		Offline: offline,
		Log:     func(f string, a ...any) { s.note(f, a...) },
		Progress: func(name string, done, total int64) {
			if total > 0 {
				s.prog.show(fmt.Sprintf("downloading %s %3d%% (%s of %s)", name, 100*done/total, mb(done), mb(total)))
			}
		},
	}
}

func mb(n int64) string { return fmt.Sprintf("%.0f MB", float64(n)/(1<<20)) }

func (s *session) engineConfig() engine.Config {
	dev, _ := engine.ParseDevice(s.cfg.Device)
	return engine.Config{
		Options:  s.assetOptions(),
		ModelDir: s.cfg.Model.Dir,
		Model:    assets.ModelRef{Repo: s.cfg.Model.Repo, Revision: s.cfg.Model.Revision, Variant: s.cfg.Model.Variant},
		ORTLib:   s.cfg.ORTLib,
		Device:   dev,
		GPUID:    s.cfg.GPUID,
		Threads:  s.cfg.Threads,
		// Auto mode fetches the GPU build of ONNX Runtime by itself when the CUDA
		// libraries are present, except in a hook.
		FetchGPURuntime: !s.hook,
		NoScoreCache:    !s.cfg.ScoreCache,
	}
}

// model resolves the model only (enough to list and cut files).
func (s *session) model(ctx context.Context) (*assets.Model, error) {
	c := s.engineConfig()
	m, err := assets.ResolveModel(ctx, assets.ModelOptions{Options: c.Options, Dir: c.ModelDir, Ref: c.Model})
	s.prog.clear()
	if err == nil {
		s.verbose("model %s (%s%s)", m.Ref, m.Source, where(m.Where))
	}
	return m, err
}

func where(p string) string {
	if p == "" {
		return ""
	}
	return ": " + p
}

// open loads the model on a device.
func (s *session) open(ctx context.Context, m *assets.Model) (*engine.Engine, error) {
	e, err := engine.OpenModel(ctx, s.engineConfig(), m)
	s.prog.clear()
	if err != nil {
		return nil, err
	}
	if e.Fallback != "" && (s.g.verbose || assets.FindGPU(&assets.Options{}).Driver != "") {
		// Only nag about the GPU on machines that have one.
		s.note("running on the CPU: %s", e.Fallback)
	}
	s.verbose("onnxruntime %s (%s: %s) on %s, loaded in %s", e.Lib.Version, e.Runtime.Source, e.Runtime.Path, e.Device,
		e.Load.Round(time.Millisecond))
	return e, nil
}

// rel makes p relative to base with slashes, or "" when it is outside.
func rel(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(r)
}

// progress is one updating line on stderr, when it is a terminal.
type progress struct {
	on    bool
	shown bool
	last  time.Time
}

func (p *progress) show(line string) {
	if !p.on || time.Since(p.last) < 100*time.Millisecond {
		return
	}
	p.last = time.Now()
	fmt.Fprintf(os.Stderr, "\r\x1b[K%s", line)
	p.shown = true
}

func (p *progress) clear() {
	if p.shown {
		fmt.Fprint(os.Stderr, "\r\x1b[K")
		p.shown = false
	}
}
