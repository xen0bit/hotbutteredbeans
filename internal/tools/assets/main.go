// Command assets prepares what hbb is built with. The Makefile and goreleaser run it;
// it is not part of hbb.
//
//	go run ./internal/tools/assets lock    [-repo R] [-revision REV] [-variant V] [-ort 1.30.0]
//	go run ./internal/tools/assets model   [-repo R] [-revision REV] [-variant V]   # -> internal/embedded/model
//	go run ./internal/tools/assets runtime -platform linux/amd64                    # -> internal/embedded/runtime/linux_amd64
//
// lock writes hbb.lock.json: the model's files with their sha256 (from the Hub), the
// ONNX Runtime release archives with theirs (from GitHub), and the NVIDIA wheels of the
// GPU stack (from PyPI). model and runtime fetch through hbb's own verified downloader
// and lay the files out for the hbb_embed build tag.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"

	"github.com/xen0bit/hotbutteredbeans/internal/assets"
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: assets lock|model|runtime [flags]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	def := assets.DefaultModel()
	repo := fs.String("repo", env("HBB_MODEL_REPO", def.Repo), "Hugging Face model repository")
	rev := fs.String("revision", env("HBB_MODEL_REVISION", ""), "revision (a branch, tag or commit; default: the lock's pin, or main for another repo)")
	variant := fs.String("variant", env("HBB_MODEL_VARIANT", def.Variant), "graph variant (q8, fp32)")
	subdir := fs.String("subdir", "onnx", "the bundle's folder in the repository")
	ortVer := fs.String("ort", "1.30.0", "ONNX Runtime version (lock)")
	cuda := fs.String("cuda-wheels", "nvidia-cuda-runtime==13.0.96,nvidia-cublas==13.1.1.3,nvidia-curand==10.4.0.35,nvidia-cudnn-cu13==9.24.0.43",
		"the NVIDIA wheels of the GPU stack (lock)")
	plat := fs.String("platform", "", "goos/goarch (runtime)")
	out := fs.String("out", "", "output (default: hbb.lock.json, internal/embedded/model, internal/embedded/runtime/<goos>_<goarch>)")
	fs.Parse(os.Args[2:])
	if *rev == "" {
		if *repo == def.Repo {
			*rev = def.Revision
		} else {
			*rev = "main"
		}
	}
	ref := assets.ModelRef{Repo: *repo, Revision: *rev, Variant: *variant}
	o := assets.Options{Log: func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }, Progress: progress}

	switch os.Args[1] {
	case "lock":
		check(writeLock(ctx, ref, *subdir, *ortVer, strings.Split(*cuda, ","), or(*out, "hbb.lock.json")))
	case "model":
		check(embedModel(ctx, &o, ref, or(*out, filepath.Join("internal", "embedded", "model"))))
	case "runtime":
		if *plat == "" {
			fail("runtime: -platform goos/goarch is required")
		}
		check(embedRuntime(ctx, &o, *plat, or(*out, filepath.Join("internal", "embedded", "runtime", strings.ReplaceAll(*plat, "/", "_")))))
	default:
		fail("unknown command %q", os.Args[1])
	}
}

// --- lock ---

var ortAssets = map[string]string{
	"linux/amd64":   "onnxruntime-linux-x64-%s.tgz",
	"linux/arm64":   "onnxruntime-linux-aarch64-%s.tgz",
	"darwin/arm64":  "onnxruntime-osx-arm64-%s.tgz",
	"windows/amd64": "onnxruntime-win-x64-%s.zip",
	"windows/arm64": "onnxruntime-win-arm64-%s.zip",
}

var ortCUDAAssets = map[string]string{
	"linux/amd64":   "onnxruntime-linux-x64-gpu_cuda13-%s.tgz",
	"windows/amd64": "onnxruntime-win-x64-gpu_cuda13-%s.zip",
}

// wheelTags picks a wheel's platform for each GPU platform.
var wheelTags = map[string]func(string) bool{
	"linux/amd64":   func(f string) bool { return strings.Contains(f, "manylinux") && strings.HasSuffix(f, "_x86_64.whl") },
	"windows/amd64": func(f string) bool { return strings.HasSuffix(f, "-win_amd64.whl") },
}

func writeLock(ctx context.Context, ref assets.ModelRef, subdir, ortVer string, wheels []string, out string) error {
	var l assets.Lock
	// the model
	var info struct {
		SHA string `json:"sha"`
	}
	if err := getJSON(ctx, fmt.Sprintf("https://huggingface.co/api/models/%s/revision/%s", ref.Repo, ref.Revision), &info); err != nil {
		return err
	}
	var tree []struct {
		Type, Path, OID string
		Size            int64
		LFS             *struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	}
	if err := getJSON(ctx, fmt.Sprintf("https://huggingface.co/api/models/%s/tree/%s/%s", ref.Repo, info.SHA, subdir), &tree); err != nil {
		return err
	}
	l.Model = assets.ModelLock{Repo: ref.Repo, Revision: info.SHA, Subdir: subdir, Variant: ref.Variant, Files: map[string]assets.File{}}
	for _, e := range tree {
		if e.Type != "file" {
			continue
		}
		name := path.Base(e.Path)
		if e.LFS != nil {
			l.Model.Files[name] = assets.File{SHA256: e.LFS.OID, Size: e.LFS.Size}
			continue
		}
		data, err := get(ctx, fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", ref.Repo, info.SHA, e.Path))
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		l.Model.Files[name] = assets.File{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	}
	fmt.Fprintf(os.Stderr, "model %s@%s: %d files\n", ref.Repo, info.SHA, len(l.Model.Files))

	// ONNX Runtime
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"assets"`
	}
	if err := getJSON(ctx, "https://api.github.com/repos/microsoft/onnxruntime/releases/tags/v"+ortVer, &rel); err != nil {
		return err
	}
	l.ONNXRuntime = assets.RuntimeLock{Version: ortVer, CPU: map[string]assets.Archive{}, CUDA: map[string]assets.Archive{}}
	for _, set := range []struct {
		names map[string]string
		into  map[string]assets.Archive
	}{{ortAssets, l.ONNXRuntime.CPU}, {ortCUDAAssets, l.ONNXRuntime.CUDA}} {
		for plat, pat := range set.names {
			name := fmt.Sprintf(pat, ortVer)
			found := false
			for _, a := range rel.Assets {
				if a.Name == name {
					if !strings.HasPrefix(a.Digest, "sha256:") {
						return fmt.Errorf("%s: GitHub lists no sha256", name)
					}
					set.into[plat] = assets.Archive{Name: name, URL: a.URL, SHA256: strings.TrimPrefix(a.Digest, "sha256:"), Size: a.Size}
					found = true
				}
			}
			if !found {
				return fmt.Errorf("onnxruntime v%s has no %s", ortVer, name)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "onnxruntime %s: %d CPU, %d CUDA archives\n", ortVer, len(l.ONNXRuntime.CPU), len(l.ONNXRuntime.CUDA))

	// the CUDA libraries
	l.CUDA = map[string][]assets.Archive{}
	for _, w := range wheels {
		name, ver, ok := strings.Cut(strings.TrimSpace(w), "==")
		if !ok {
			return fmt.Errorf("cuda wheel %q: want name==version", w)
		}
		var pkg struct {
			URLs []struct {
				Filename    string            `json:"filename"`
				URL         string            `json:"url"`
				Size        int64             `json:"size"`
				Digests     map[string]string `json:"digests"`
				PackageType string            `json:"packagetype"`
			} `json:"urls"`
		}
		if err := getJSON(ctx, fmt.Sprintf("https://pypi.org/pypi/%s/%s/json", name, ver), &pkg); err != nil {
			return err
		}
		for plat, match := range wheelTags {
			found := false
			for _, u := range pkg.URLs {
				if u.PackageType == "bdist_wheel" && match(u.Filename) {
					l.CUDA[plat] = append(l.CUDA[plat], assets.Archive{Name: name + " " + ver, URL: u.URL, SHA256: u.Digests["sha256"], Size: u.Size})
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%s %s has no wheel for %s", name, ver, plat)
			}
		}
	}
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", out)
	return os.WriteFile(out, append(raw, '\n'), 0o644)
}

// --- embedding ---

// embedModel fetches the model through hbb's verified downloader, then lays it out under
// the fixed names internal/embedded embeds.
func embedModel(ctx context.Context, o *assets.Options, ref assets.ModelRef, out string) error {
	m, err := assets.ResolveModel(ctx, assets.ModelOptions{Options: *o, Ref: ref})
	if err != nil {
		return err
	}
	if m.Source != "cache" {
		return fmt.Errorf("expected the model from the cache, got it from %s", m.Source)
	}
	file, ext, err := m.Bundle.Graph(ref.Variant)
	if err != nil {
		return err
	}
	if len(ext) > 1 {
		return fmt.Errorf("the %s graph has %d external data files; embedding supports one", ref.Variant, len(ext))
	}
	raw, err := os.ReadFile(filepath.Join(m.Where, "manifest.json"))
	if err != nil {
		return err
	}
	var mf struct {
		Files map[string]assets.File `json:"files"`
	}
	if err := json.Unmarshal(raw, &mf); err != nil {
		return err
	}
	man := map[string]any{"repo": m.Ref.Repo, "revision": m.Ref.Revision, "variant": ref.Variant, "graph": file,
		"sha256": map[string]string{}}
	sums := man["sha256"].(map[string]string)
	copies := [][2]string{{"bundle.json", "bundle.json"}, {"tokenizer.json", "tokenizer.json"}, {file, "graph.onnx"}}
	if len(ext) == 1 {
		man["external"] = ext[0]
		copies = append(copies, [2]string{ext[0], "weights.bin"})
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if len(ext) == 0 {
		if err := os.WriteFile(filepath.Join(out, "weights.bin"), nil, 0o644); err != nil {
			return err
		}
	}
	for _, c := range copies {
		sums[c[0]] = mf.Files[c[0]].SHA256
		if err := linkOrCopy(filepath.Join(m.Where, c[0]), filepath.Join(out, c[1])); err != nil {
			return err
		}
	}
	raw, _ = json.MarshalIndent(man, "", "  ")
	fmt.Fprintf(os.Stderr, "model %s ready to embed in %s\n", m.Ref, out)
	return os.WriteFile(filepath.Join(out, "manifest.json"), raw, 0o644)
}

func embedRuntime(ctx context.Context, o *assets.Options, plat, out string) error {
	goos, goarch, _ := strings.Cut(plat, "/")
	rt, err := assets.FetchRuntimeFor(ctx, o, goos, goarch)
	if err != nil {
		return err
	}
	l, _ := assets.Locked()
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := linkOrCopy(rt, filepath.Join(out, "lib.bin")); err != nil {
		return err
	}
	data, err := os.ReadFile(rt)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	raw, _ := json.MarshalIndent(map[string]string{"version": l.ONNXRuntime.Version, "name": filepath.Base(rt),
		"sha256": hex.EncodeToString(sum[:])}, "", "  ")
	fmt.Fprintf(os.Stderr, "onnxruntime %s for %s ready to embed in %s\n", l.ONNXRuntime.Version, plat, out)
	return os.WriteFile(filepath.Join(out, "manifest.json"), raw, 0o644)
}

// linkOrCopy hard-links src to dst (no second copy of 600 MB), else copies it.
func linkOrCopy(src, dst string) error {
	os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	outf, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(outf, in); err != nil {
		outf.Close()
		return err
	}
	return outf.Close()
}

// --- helpers ---

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if tok := os.Getenv("HF_TOKEN"); tok != "" && strings.Contains(url, "huggingface.co") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" && strings.Contains(url, "api.github.com") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func getJSON(ctx context.Context, url string, v any) error {
	data, err := get(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

var lastPct = map[string]int{}

func progress(name string, done, total int64) {
	if total <= 0 {
		return
	}
	pct := int(100 * done / total)
	if pct/10 != lastPct[name]/10 || pct == 100 && lastPct[name] != 100 {
		fmt.Fprintf(os.Stderr, "  %s %d%%\n", name, pct)
		lastPct[name] = pct
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "assets: "+f+"\n", a...)
	os.Exit(1)
}
