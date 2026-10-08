// Package config is hbb's settings: defaults, then .hbb.yaml (found from the
// repository root or the working folder upwards, or $HBB_CONFIG), then HBB_*
// environment variables. Command-line flags apply last, in cmd/hbb.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// FileNames are the names .hbb.yaml is looked for under, in order.
var FileNames = []string{".hbb.yaml", ".hbb.yml"}

// Config is every setting.
type Config struct {
	Device  string `yaml:"device"`  // auto | gpu | cpu | coreml
	Threads int    `yaml:"threads"` // CPU threads, 0: ONNX Runtime's default
	GPUID   int    `yaml:"gpu_id"`
	ORTLib  string `yaml:"ort_lib"`
	Offline bool   `yaml:"offline"`

	Model Model `yaml:"model"`

	Exclude         []string `yaml:"exclude"`
	Include         []string `yaml:"include"`
	DefaultExcludes bool     `yaml:"default_excludes"`
	SkipGenerated   bool     `yaml:"skip_generated"`
	MaxFileSize     Size     `yaml:"max_file_size"`
	Gitignore       bool     `yaml:"gitignore"`

	CWE struct {
		Only []string `yaml:"only"`
		Skip []string `yaml:"skip"`
	} `yaml:"cwe"`

	Scan Ranking `yaml:"scan"`
	Diff Ranking `yaml:"diff"`

	FailAt    float64 `yaml:"fail_at"`    // exit 1 when a finding reaches this P (0: never)
	FailDelta float64 `yaml:"fail_delta"` // diff --compare: exit 1 when a finding's P rises this much (0: never)

	Format       string `yaml:"format"`
	ContentLines int    `yaml:"content_lines"`
	Baseline     string `yaml:"baseline"`
	ScoreCache   bool   `yaml:"score_cache"`

	Hook Hook `yaml:"hook"`

	// Path is the file the settings came from ("" for none).
	Path string `yaml:"-"`
}

// Model selects the model.
type Model struct {
	Repo     string `yaml:"repo"`
	Revision string `yaml:"revision"`
	Variant  string `yaml:"variant"`
	Dir      string `yaml:"dir"`
}

// Ranking is how many findings to show, and from what probability.
type Ranking struct {
	Top     int     `yaml:"top"`
	MinP    float64 `yaml:"min_p"`
	Compare bool    `yaml:"compare"`
}

// Hook configures `hbb diff --hook` (the git hooks).
type Hook struct {
	// Fetch lets a hook download the model and runtime when they are missing; by
	// default a hook never downloads (a commit should not wait on 600 MB) and lets the
	// commit through with a warning instead.
	Fetch   bool `yaml:"fetch"`
	Compare bool `yaml:"compare"`
}

// DefaultExcludes are left out unless default_excludes is false: vendored and
// third-party code, and minified bundles.
var DefaultExcludes = []string{"vendor/", "third_party/", "third-party/", "*.min.js", "*.min.mjs", "*.bundle.js"}

// Default is every setting's default.
func Default() Config {
	return Config{
		Device:          "auto",
		DefaultExcludes: true,
		SkipGenerated:   true,
		MaxFileSize:     1 << 20,
		Gitignore:       true,
		Scan:            Ranking{Top: 30},
		Diff:            Ranking{Top: 20, MinP: 0.5},
		Format:          "text",
		ContentLines:    10,
		ScoreCache:      true,
	}
}

// Load finds and reads the config file (from dir upwards to stop, or $HBB_CONFIG) over
// the defaults, then applies the environment.
func Load(dir, stop string) (Config, error) {
	c := Default()
	path := os.Getenv("HBB_CONFIG")
	if path == "" {
		path = find(dir, stop)
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return c, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return c, fmt.Errorf("%s: %w", path, err)
		}
		c.Path = path
	}
	return c, c.applyEnv()
}

func find(dir, stop string) string {
	for d := dir; d != ""; {
		for _, n := range FileNames {
			p := filepath.Join(d, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
		if d == stop {
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return ""
}

func (c *Config) applyEnv() error {
	var errs []error
	str := func(k string, dst *string) {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			*dst = v
		}
	}
	num := func(k string, dst *int) {
		if v := os.Getenv(k); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", k, v, err))
				return
			}
			*dst = n
		}
	}
	flt := func(k string, dst *float64) {
		if v := os.Getenv(k); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", k, v, err))
				return
			}
			*dst = f
		}
	}
	boolean := func(k string, dst *bool) {
		if v := os.Getenv(k); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", k, v, err))
				return
			}
			*dst = b
		}
	}
	str("HBB_DEVICE", &c.Device)
	num("HBB_THREADS", &c.Threads)
	num("HBB_GPU_ID", &c.GPUID)
	str("ONNXRUNTIME_LIB", &c.ORTLib)
	str("HBB_ORT_LIB", &c.ORTLib)
	boolean("HBB_OFFLINE", &c.Offline)
	str("HBB_MODEL_REPO", &c.Model.Repo)
	str("HBB_MODEL_REVISION", &c.Model.Revision)
	str("HBB_MODEL_VARIANT", &c.Model.Variant)
	str("HBB_MODEL_DIR", &c.Model.Dir)
	str("HBB_FORMAT", &c.Format)
	flt("HBB_FAIL_AT", &c.FailAt)
	flt("HBB_FAIL_DELTA", &c.FailDelta)
	str("HBB_BASELINE", &c.Baseline)
	boolean("HBB_HOOK_FETCH", &c.Hook.Fetch)
	return errors.Join(errs...)
}

// Excludes is the exclude globs in force.
func (c *Config) Excludes() []string {
	if c.DefaultExcludes {
		return append(append([]string(nil), DefaultExcludes...), c.Exclude...)
	}
	return c.Exclude
}

// Size is a byte count that reads "1MiB", "500KB", "2M" or a plain number.
type Size int64

func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseSize(n.Value)
	if err != nil {
		return err
	}
	*s = Size(v)
	return nil
}

func (s Size) MarshalYAML() (any, error) { return FormatSize(int64(s)), nil }

// ParseSize reads a size.
func ParseSize(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(v, u.suf) {
			v, mult = strings.TrimSpace(strings.TrimSuffix(v, u.suf)), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("size %q: want e.g. 1MiB, 500KB or 0 for no limit", v)
	}
	return int64(f * float64(mult)), nil
}

// FormatSize writes a size the way ParseSize reads it.
func FormatSize(n int64) string {
	switch {
	case n == 0:
		return "0"
	case n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	}
	return strconv.FormatInt(n, 10)
}

// Template is a commented .hbb.yaml with the defaults, for `hbb config init`.
const Template = `# hbb settings. Command-line flags override these, and HBB_* environment
# variables override the file (HBB_DEVICE, HBB_THREADS, HBB_MODEL_DIR, ...).

# Where the model runs: auto (the GPU when a usable one is found, else the CPU),
# gpu (fail without one), cpu, or coreml (macOS).
device: auto
threads: 0            # CPU threads; 0 lets ONNX Runtime choose (the physical cores)

# model:
#   repo: billytesterman/secjev-encoder
#   revision: <commit>  # pin a commit, not a branch
#   variant: q8         # q8 (593 MB) or fp32 (1.4 GB)
#   dir: ""             # a local bundle folder instead (nothing is downloaded)

# Which files are read. Globs follow .gitignore: "*.min.js" matches at any depth,
# "vendor/" is a folder and everything in it, "**" crosses folders.
exclude: []
include: []           # when set, only files matching these
default_excludes: true  # vendor/, third_party/, minified bundles
skip_generated: true  # "Code generated ... DO NOT EDIT", "@generated"
max_file_size: 1MiB
gitignore: true       # in a repository, skip what .gitignore excludes

cwe:
  only: []            # e.g. [CWE-89, CWE-78]
  skip: []

scan:
  top: 30             # findings listed
  min_p: 0
diff:
  top: 20
  min_p: 0.5
  compare: false      # also score the code before the change, and report the rise

# hbb reports and exits 0 by default: it ranks code to read, it does not prove a flaw.
# Set a threshold to fail (exit 1) on findings at or above it.
fail_at: 0            # e.g. 0.9
fail_delta: 0         # with compare: e.g. 0.3

format: text          # text, json, jsonl, sarif, markdown, llm, github
content_lines: 10     # source lines shown under each finding (0: none, -1: all)
# baseline: .hbb-baseline.json   # findings accepted earlier (hbb baseline)

hook:
  fetch: false        # let git hooks download the model when it is missing
  compare: false
`
