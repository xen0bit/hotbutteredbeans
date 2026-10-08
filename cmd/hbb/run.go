package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/xen0bit/hotbutteredbeans/internal/assets"
	"github.com/xen0bit/hotbutteredbeans/internal/buildinfo"
	"github.com/xen0bit/hotbutteredbeans/internal/config"
	"github.com/xen0bit/hotbutteredbeans/internal/report"
	"github.com/xen0bit/hotbutteredbeans/internal/scan"
	"github.com/xen0bit/hotbutteredbeans/internal/source"
)

// outFlags are the flags of scan and diff: what to read, what to report, when to fail.
type outFlags struct {
	format, output    string
	also              []string
	top               int
	minP              float64
	cwe, skipCWE      []string
	contentLines      int
	failAt, failDelta float64
	exclude, include  []string
	noGitignore       bool
	noDefaultExcludes bool
	includeGenerated  bool
	maxFileSize       string
	baseline          string
	writeBaseline     string
	windowsOut        string
	maxChars          int
	noCache           bool
}

func (o *outFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVarP(&o.format, "format", "f", "", "output: "+strings.Join(report.Formats, ", ")+" (default text; $HBB_FORMAT)")
	f.StringVarP(&o.output, "output", "o", "", "write the report to a file instead of stdout")
	f.StringArrayVar(&o.also, "also", nil, "also write FORMAT=PATH from the same run (e.g. --also sarif=hbb.sarif --also markdown=summary.md); repeatable")
	f.IntVarP(&o.top, "top", "n", 0, "list at most this many findings (0 or -1: all; default 30 for scan, 20 for diff)")
	f.Float64Var(&o.minP, "min-p", 0, "list findings with p at or above this")
	f.StringSliceVar(&o.cwe, "cwe", nil, "only these CWEs (e.g. CWE-89,CWE-78); repeatable")
	f.StringSliceVar(&o.skipCWE, "skip-cwe", nil, "leave out these CWEs; repeatable")
	f.IntVar(&o.contentLines, "content-lines", 0, "source lines shown under each finding (default 10; 0: none; -1: the whole window)")
	f.Float64Var(&o.failAt, "fail-at", 0, "exit 1 when a finding reaches this p (default: never; $HBB_FAIL_AT)")
	f.Float64Var(&o.failDelta, "fail-delta", 0, "with --compare: exit 1 when a finding's p rose this much ($HBB_FAIL_DELTA)")
	f.StringSliceVar(&o.exclude, "exclude", nil, "skip paths matching these globs (.gitignore style); repeatable")
	f.StringSliceVar(&o.include, "include", nil, "only paths matching these globs; repeatable")
	f.BoolVar(&o.noGitignore, "no-gitignore", false, "read files .gitignore excludes too")
	f.BoolVar(&o.noDefaultExcludes, "no-default-excludes", false, "read vendor/, third_party/ and minified bundles too")
	f.BoolVar(&o.includeGenerated, "include-generated", false, "read generated files too")
	f.StringVar(&o.maxFileSize, "max-file-size", "", "skip files larger than this (default 1MiB; 0: no limit)")
	f.StringVar(&o.baseline, "baseline", "", "leave out findings recorded in this baseline file")
	f.StringVar(&o.writeBaseline, "write-baseline", "", "record every finding (at or above --min-p) in this baseline file")
	f.StringVar(&o.windowsOut, "windows-out", "", "also write every window's scores to this file (JSON lines)")
	f.IntVar(&o.maxChars, "max-chars", 60000, "llm format: the budget for code, in characters (0: no limit)")
	f.BoolVar(&o.noCache, "no-cache", false, "score every window again, ignoring the score cache")
}

// apply puts the flags over the settings; rk is the scan or diff ranking.
func (o *outFlags) apply(cmd *cobra.Command, c *config.Config, rk *config.Ranking) error {
	f := cmd.Flags()
	if f.Changed("format") {
		c.Format = o.format
	}
	if !report.Valid(c.Format) {
		return fmt.Errorf("unknown format %q (%s)", c.Format, strings.Join(report.Formats, ", "))
	}
	for _, a := range o.also {
		format, path, ok := strings.Cut(a, "=")
		if !ok || path == "" || !report.Valid(format) {
			return fmt.Errorf("--also %q: want FORMAT=PATH with FORMAT one of %s", a, strings.Join(report.Formats, ", "))
		}
	}
	if f.Changed("top") {
		rk.Top = o.top
	}
	if f.Changed("min-p") {
		rk.MinP = o.minP
	}
	if f.Changed("cwe") {
		c.CWE.Only = o.cwe
	}
	if f.Changed("skip-cwe") {
		c.CWE.Skip = o.skipCWE
	}
	if f.Changed("content-lines") {
		c.ContentLines = o.contentLines
	}
	if f.Changed("fail-at") {
		c.FailAt = o.failAt
	}
	if f.Changed("fail-delta") {
		c.FailDelta = o.failDelta
	}
	c.Exclude = append(c.Exclude, o.exclude...)
	c.Include = append(c.Include, o.include...)
	if o.noGitignore {
		c.Gitignore = false
	}
	if o.noDefaultExcludes {
		c.DefaultExcludes = false
	}
	if o.includeGenerated {
		c.SkipGenerated = false
	}
	if o.maxFileSize != "" {
		n, err := config.ParseSize(o.maxFileSize)
		if err != nil {
			return err
		}
		c.MaxFileSize = config.Size(n)
	}
	if f.Changed("baseline") {
		c.Baseline = o.baseline
	}
	if o.noCache {
		c.ScoreCache = false
	}
	return nil
}

func filterFor(m *assets.Model, c *config.Config) *source.Filter {
	return &source.Filter{
		Extensions: m.Bundle.Extensions, SkipDirs: m.Bundle.SkipDirs,
		Exclude: c.Excludes(), Include: c.Include,
		SkipGenerated: c.SkipGenerated, MaxBytes: int64(c.MaxFileSize),
	}
}

func labels(cwes []string) []string {
	var out []string
	for _, c := range cwes {
		for _, part := range strings.Split(c, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, scan.Label(part))
			}
		}
	}
	return out
}

// job is a scored scan or diff, ready to report.
type job struct {
	mode       string
	root       string
	base, head string
	compare    bool
	files      int
	targets    []scan.Target
	skipped    scan.Skipped
	ranking    config.Ranking
	out        io.Writer // default stdout
}

// finish scores the targets, reports, and returns the exit code.
func (s *session) finish(ctx context.Context, m *assets.Model, o *outFlags, j job) error {
	for why, n := range j.skipped {
		s.verbose("skipped %d %s file(s)", n, why)
	}
	var res []scan.Result
	device := "-"
	if len(j.targets) > 0 {
		e, err := s.open(ctx, m)
		if err != nil {
			return err
		}
		defer e.Close()
		device = string(e.Device)
		res, stats, err := scan.Score(ctx, e, j.targets, j.compare, func(done, total int) {
			s.prog.show(fmt.Sprintf("scoring on %s: %d/%d windows", e.Device, done, total))
		})
		s.prog.clear()
		if err != nil {
			return err
		}
		s.verbose("%d windows (%d cached), %d tokens, %d truncated; tokenize %s, score %s (%.1f windows/s)",
			stats.Windows, stats.Cached, stats.Tokens, stats.Truncated, stats.Tokenize.Round(1e6), stats.Score.Round(1e6),
			rate(stats.Windows-stats.Cached, stats.Score.Seconds()))
		return s.report(m, o, j, res, device)
	}
	return s.report(m, o, j, res, device)
}

func rate(n int, secs float64) float64 {
	if secs <= 0 {
		return 0
	}
	return float64(n) / secs
}

func (s *session) report(m *assets.Model, o *outFlags, j job, res []scan.Result, device string) error {
	c := &s.cfg
	b := m.Bundle
	base, err := loadBaseline(c.Baseline)
	if err != nil {
		return err
	}
	opts := scan.Options{MinP: j.ranking.MinP, Only: labels(c.CWE.Only), Skip: labels(c.CWE.Skip),
		ByDelta: j.compare, Baseline: base}
	all := scan.Rank(b, res, opts)

	// The thresholds apply to every finding, whatever --min-p shows.
	failOpts := opts
	failOpts.MinP = 0
	code := exitOK
	for _, f := range scan.Rank(b, res, failOpts) {
		if c.FailAt > 0 && f.P >= c.FailAt || c.FailDelta > 0 && f.Delta != nil && *f.Delta >= c.FailDelta {
			code = exitFindings
			break
		}
	}
	if o.writeBaseline != "" {
		if err := writeBaseline(o.writeBaseline, all); err != nil {
			return err
		}
		s.note("recorded %d findings in %s", len(all), o.writeBaseline)
	}
	if o.windowsOut != "" {
		if err := writeWindows(o.windowsOut, res); err != nil {
			return err
		}
	}
	shown := all
	if j.ranking.Top > 0 && len(shown) > j.ranking.Top {
		shown = shown[:j.ranking.Top]
	}

	var rules []report.Rule
	for _, l := range b.Labels {
		rules = append(rules, report.Rule{Label: l, CWE: scan.CWE(l), Title: scan.Title(l), Question: b.Questions[l]})
	}
	out := j.out
	if out == nil {
		out = os.Stdout
	}
	var file *os.File
	if o.output != "" {
		if file, err = os.Create(o.output); err != nil {
			return err
		}
		out = file
	}
	r := &report.Report{
		Tool: "hbb", Version: buildinfo.Ver(), Mode: j.mode, Root: j.root, Base: j.base, Head: j.head, Compare: j.compare,
		Model: m.Ref.String(), Device: device, Files: j.files, Windows: len(j.targets), MinP: j.ranking.MinP,
		Findings: shown, Rules: rules, ContentLines: c.ContentLines, MaxChars: o.maxChars,
		Color: c.Format == "text" && file == nil && !s.g.noColor && os.Getenv("NO_COLOR") == "" && isTerminal(out),
	}
	if err := report.Write(out, c.Format, r); err != nil {
		return err
	}
	if file != nil {
		if err := file.Close(); err != nil {
			return err
		}
	}
	for _, a := range o.also {
		format, path, _ := strings.Cut(a, "=")
		extra := *r
		extra.Color = false
		if err := writeReport(path, format, &extra); err != nil {
			return err
		}
	}
	if c.Format == "text" && len(all) > len(shown) {
		s.note("%d more findings at p >= %.2f (--top -1 lists all)", len(all)-len(shown), j.ranking.MinP)
	}
	if code == exitFindings {
		s.note("findings reached the failure threshold (fail_at %.2g, fail_delta %.2g)", c.FailAt, c.FailDelta)
		return exitCode(exitFindings)
	}
	return nil
}

// isTerminal reports whether w is a terminal that shows colours.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) && enableColor(f)
}

func writeReport(path, format string, r *report.Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := report.Write(f, format, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// --- baseline ---

type baselineFile struct {
	Version  int                        `json:"version"`
	Findings map[string]baselineFinding `json:"findings"`
}

type baselineFinding struct {
	CWE      string  `json:"cwe"`
	Location string  `json:"location"`
	P        float64 `json:"p"`
}

func loadBaseline(path string) (map[string]bool, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b baselineFile
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("baseline %s: %w", path, err)
	}
	set := map[string]bool{}
	for fp := range b.Findings {
		set[fp] = true
	}
	return set, nil
}

func writeBaseline(path string, fs []scan.Finding) error {
	b := baselineFile{Version: 1, Findings: map[string]baselineFinding{}}
	for _, f := range fs {
		b.Findings[f.Fingerprint] = baselineFinding{CWE: f.CWE, Location: f.Location, P: float64(int(f.P*1000)) / 1000}
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

func writeWindows(path string, res []scan.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, r := range res {
		rec := map[string]any{"path": r.Path, "lang": r.Lang, "from": r.Window.From, "to": r.Window.To,
			"location": fmt.Sprintf("%s:%d-%d", r.Path, r.Window.From, r.Window.To), "p": r.P}
		if r.Compared {
			rec["base_p"] = r.BaseP
		}
		if len(r.Changed) > 0 {
			rec["changed"] = r.Changed
		}
		if err := enc.Encode(rec); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}
