package assets

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/xen0bit/hotbutteredbeans/internal/embedded"
)

// Runtime is a resolved ONNX Runtime library.
type Runtime struct {
	Path   string // the library to load
	CUDA   bool   // the GPU build (it runs on the CPU too)
	Source string // "flag", "embedded", "cache", "beside hbb", "download", "system"
}

// RuntimeOptions say which ONNX Runtime to use.
type RuntimeOptions struct {
	Options
	Lib string // a library the user named (--ort-lib, HBB_ORT_LIB): used as is
	// CUDA asks for the GPU build: from the cache, or downloaded when FetchCUDA is set.
	CUDA      bool
	FetchCUDA bool
}

// The library files to take out of an ONNX Runtime archive, by OS: the library itself
// and, for the GPU build, the providers it loads from its own folder.
var (
	mainLib = map[string]*regexp.Regexp{
		"linux":   regexp.MustCompile(`^libonnxruntime\.so\.\d+\.\d+\.\d+$`),
		"darwin":  regexp.MustCompile(`^libonnxruntime\.\d+\.\d+\.\d+\.dylib$`),
		"windows": regexp.MustCompile(`^onnxruntime\.dll$`),
	}
	providerLibs = regexp.MustCompile(`^(lib)?onnxruntime_providers_(shared|cuda)\.(so|dll)$`)
)

// ResolveRuntime finds the onnxruntime library.
func ResolveRuntime(ctx context.Context, opt RuntimeOptions) (*Runtime, error) {
	if opt.Lib != "" {
		if _, err := os.Stat(opt.Lib); err != nil {
			return nil, fmt.Errorf("onnxruntime library: %w", err)
		}
		return &Runtime{Path: opt.Lib, Source: "flag", CUDA: strings.Contains(strings.ToLower(opt.Lib), "gpu")}, nil
	}
	l, err := Locked()
	if err != nil {
		return nil, err
	}
	plat := Platform()
	if opt.CUDA {
		a, ok := l.ONNXRuntime.CUDA[plat]
		if !ok {
			return nil, fmt.Errorf("ONNX Runtime has no CUDA build for %s", plat)
		}
		dir := runtimeDir(&opt.Options, l.ONNXRuntime.Version, runtime.GOOS, runtime.GOARCH, "cuda")
		if p, ok := complete(dir); ok {
			return &Runtime{Path: p, CUDA: true, Source: "cache"}, nil
		}
		if !opt.FetchCUDA {
			return nil, fmt.Errorf("the GPU build of ONNX Runtime is not in the cache: %w", ErrNotFetched)
		}
		p, err := fetchArchive(ctx, &opt.Options, a, dir, runtime.GOOS)
		if err != nil {
			return nil, err
		}
		return &Runtime{Path: p, CUDA: true, Source: "download"}, nil
	}

	if m, lib, ok := embedded.LoadRuntime(); ok {
		p, err := extractEmbedded(&opt.Options, m, lib)
		if err == nil {
			return &Runtime{Path: p, Source: "embedded"}, nil
		}
		opt.logf("could not unpack the built-in onnxruntime (%v); looking elsewhere", err)
	}
	dir := runtimeDir(&opt.Options, l.ONNXRuntime.Version, runtime.GOOS, runtime.GOARCH, "cpu")
	if p, ok := complete(dir); ok {
		return &Runtime{Path: p, Source: "cache"}, nil
	}
	if p := besideExecutable(); p != "" {
		return &Runtime{Path: p, Source: "beside hbb"}, nil
	}
	var dlErr error
	if a, ok := l.ONNXRuntime.CPU[plat]; ok {
		p, err := fetchArchive(ctx, &opt.Options, a, dir, runtime.GOOS)
		if err == nil {
			return &Runtime{Path: p, Source: "download"}, nil
		}
		dlErr = err
	} else {
		dlErr = fmt.Errorf("ONNX Runtime publishes no build for %s; set HBB_ORT_LIB to a libonnxruntime you built", plat)
	}
	if p := onSystem(); p != "" {
		opt.logf("using the system's %s (%v)", p, dlErr)
		return &Runtime{Path: p, Source: "system"}, nil
	}
	return nil, fmt.Errorf("no onnxruntime library: %w", dlErr)
}

// ErrNotFetched is returned for an optional download that was not allowed.
var ErrNotFetched = errors.New("not fetched")

func runtimeDir(o *Options, version, goos, goarch, kind string) string {
	return filepath.Join(o.cache(), "runtime", fmt.Sprintf("onnxruntime-%s-%s-%s-%s", version, goos, goarch, kind))
}

// FetchRuntimeFor fetches the CPU build of ONNX Runtime for another platform into the
// cache and returns its library (for embedding it in a cross-compiled build).
func FetchRuntimeFor(ctx context.Context, o *Options, goos, goarch string) (string, error) {
	l, err := Locked()
	if err != nil {
		return "", err
	}
	a, ok := l.ONNXRuntime.CPU[goos+"/"+goarch]
	if !ok {
		return "", fmt.Errorf("ONNX Runtime publishes no build for %s/%s", goos, goarch)
	}
	dir := runtimeDir(o, l.ONNXRuntime.Version, goos, goarch, "cpu")
	if p, ok := complete(dir); ok {
		return p, nil
	}
	return fetchArchive(ctx, o, a, dir, goos)
}

// complete returns the library in an unpacked runtime folder, if the unpacking finished.
func complete(dir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, ".complete"))
	if err != nil {
		return "", false
	}
	p := filepath.Join(dir, strings.TrimSpace(string(raw)))
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// fetchArchive downloads an ONNX Runtime archive, unpacks its libraries into dir and
// returns the main one.
func fetchArchive(ctx context.Context, o *Options, a Archive, dir, goos string) (string, error) {
	name := a.Name
	if name == "" {
		name = path.Base(a.URL)
	}
	o.logf("downloading %s (%s)", name, humanBytes(a.Size))
	tmp := filepath.Join(o.cache(), "downloads", name)
	if err := fetch(ctx, o, a.URL, tmp, a.SHA256, a.Size, nil); err != nil {
		return "", err
	}
	defer func() { os.Remove(tmp); os.Remove(tmp + ".sha256"); os.Remove(tmp + ".lock") }()
	main := mainLib[goos]
	var lib string
	err := unpack(tmp, func(name string) bool { return main.MatchString(name) || providerLibs.MatchString(name) },
		func(name string, r io.Reader) error {
			if main.MatchString(name) {
				lib = name
			}
			return writeFileFrom(filepath.Join(dir, name), r, 0o755)
		})
	if err != nil {
		return "", fmt.Errorf("unpacking %s: %w", name, err)
	}
	if lib == "" {
		return "", fmt.Errorf("%s holds no onnxruntime library", name)
	}
	if err := writeAtomic(filepath.Join(dir, ".complete"), []byte(lib), 0o644); err != nil {
		return "", err
	}
	return filepath.Join(dir, lib), nil
}

// unpack calls each for the files of a .tgz, .zip or .whl whose base name want accepts.
func unpack(archive string, want func(string) bool, each func(name string, r io.Reader) error) error {
	if strings.HasSuffix(archive, ".tgz") || strings.HasSuffix(archive, ".tar.gz") {
		f, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if h.Typeflag == tar.TypeReg && want(path.Base(h.Name)) {
				if err := each(path.Base(h.Name), tr); err != nil {
					return err
				}
			}
		}
	}
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !want(path.Base(f.Name)) {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		err = each(path.Base(f.Name), r)
		r.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func writeFileFrom(dst string, r io.Reader, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), perm)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// extractEmbedded writes the built-in library to the cache (loading needs a file),
// once: later runs find it there, verified by its sidecar.
func extractEmbedded(o *Options, m embedded.RuntimeManifest, lib []byte) (string, error) {
	dir := filepath.Join(o.cache(), "runtime", fmt.Sprintf("onnxruntime-%s-%s-%s-embedded", m.Version, runtime.GOOS, runtime.GOARCH))
	p := filepath.Join(dir, m.Name)
	if ok, _ := verified(p, m.SHA256, int64(len(lib))); ok {
		return p, nil
	}
	sum := sha256.Sum256(lib)
	if got := hex.EncodeToString(sum[:]); got != m.SHA256 {
		return "", fmt.Errorf("the built-in library's sha256 is %s, its manifest says %s", got, m.SHA256)
	}
	if err := writeAtomic(p, lib, 0o755); err != nil {
		// A read-only home (a locked-down container): Linux can load from memory.
		if mp, merr := memoryFile(m.Name, lib); merr == nil {
			return mp, nil
		}
		return "", err
	}
	return p, markVerified(p, m.SHA256)
}

// besideExecutable finds an onnxruntime library next to hbb itself (how packagers ship it).
func besideExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return findLib([]string{filepath.Dir(exe), filepath.Join(filepath.Dir(exe), "..", "lib")})
}

// onSystem finds an onnxruntime library in the usual system folders. On Windows only
// hbb's own folder counts: System32's onnxruntime.dll belongs to Windows ML and is old.
func onSystem() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	dirs := []string{"/usr/local/lib", "/usr/lib", "/usr/lib64", "/opt/homebrew/lib", "/usr/local/opt/onnxruntime/lib"}
	if m, _ := filepath.Glob("/usr/lib/*-linux-gnu"); len(m) > 0 {
		dirs = append(dirs, m...)
	}
	return findLib(dirs)
}

func findLib(dirs []string) string {
	names := map[string][]string{
		"linux":   {"libonnxruntime.so", "libonnxruntime.so.1"},
		"darwin":  {"libonnxruntime.dylib"},
		"windows": {"onnxruntime.dll"},
	}[runtime.GOOS]
	for _, d := range dirs {
		for _, n := range names {
			p := filepath.Join(d, n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
		if runtime.GOOS != "windows" {
			if m, _ := filepath.Glob(filepath.Join(d, "libonnxruntime*.so.*")); len(m) > 0 {
				return m[0]
			}
		}
	}
	return ""
}
