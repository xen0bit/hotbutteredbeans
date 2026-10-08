package assets

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"testing/fstest"

	"github.com/xen0bit/hotbutteredbeans/internal/bundle"
	"github.com/xen0bit/hotbutteredbeans/internal/embedded"
	"github.com/xen0bit/hotbutteredbeans/internal/ort"
)

// Model is a resolved model: the bundle's metadata and tokenizer, and the graph to load.
type Model struct {
	Ref    ModelRef
	Source string // "dir", "embedded" or "cache"
	Where  string // the folder it was read from ("" when embedded)
	Bundle *bundle.Bundle
	Graph  ort.ModelSource
	// ID identifies the graph's weights, for the score cache: scores are reused only
	// under the same ID.
	ID string
}

// ModelOptions say which model to use.
type ModelOptions struct {
	Options
	Dir string   // a bundle folder on disk: used as is, nothing is fetched
	Ref ModelRef // the model to use when Dir is empty; zero fields default to DefaultModel
}

// ResolveModel finds the model: opt.Dir, else the embedded model when it is the one
// asked for, else the cache, else a download from Hugging Face.
func ResolveModel(ctx context.Context, opt ModelOptions) (*Model, error) {
	if opt.Dir != "" {
		return modelFromDir(opt.Dir, opt.Ref.Variant)
	}
	ref := withDefaults(opt.Ref)
	if m, ok := embedded.LoadModel(); ok {
		mf := m.Manifest
		if mf.Repo == ref.Repo && mf.Variant == ref.Variant && (opt.Ref.Revision == "" || opt.Ref.Revision == mf.Revision) {
			return modelFromEmbedded(m)
		}
		opt.logf("the built-in model is %s@%.12s (%s); %s was asked for", mf.Repo, mf.Revision, mf.Variant, ref)
	}
	return modelFromCache(ctx, &opt.Options, ref)
}

func withDefaults(r ModelRef) ModelRef {
	d := DefaultModel()
	if r.Repo == "" {
		r.Repo = d.Repo
		if r.Revision == "" {
			r.Revision = d.Revision
		}
	}
	if r.Revision == "" {
		r.Revision = "main"
	}
	if r.Variant == "" {
		r.Variant = d.Variant
	}
	if r.Variant == "" {
		r.Variant = "q8"
	}
	return r
}

func modelFromDir(dir, variant string) (*Model, error) {
	if variant == "" {
		variant = withDefaults(ModelRef{}).Variant
	}
	b, err := bundle.Load(os.DirFS(dir))
	if err != nil {
		return nil, fmt.Errorf("model folder %s: %w", dir, err)
	}
	file, ext, err := b.Graph(variant)
	if err != nil {
		return nil, fmt.Errorf("model folder %s: %w", dir, err)
	}
	// A local folder changes under us (re-exports), so its ID comes from what is there.
	h := sha256.New()
	for _, f := range append([]string{"bundle.json", file}, ext...) {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			return nil, fmt.Errorf("model folder %s: %w", dir, err)
		}
		fmt.Fprintf(h, "%s %d %d\n", f, st.Size(), st.ModTime().UnixNano())
	}
	return &Model{
		Ref: ModelRef{Repo: dir, Variant: variant}, Source: "dir", Where: dir, Bundle: b,
		Graph: ort.ModelSource{Path: filepath.Join(dir, file)},
		ID:    "dir-" + hex.EncodeToString(h.Sum(nil))[:16],
	}, nil
}

func modelFromEmbedded(m embedded.Model) (*Model, error) {
	mf := m.Manifest
	b, err := bundle.Load(fstest.MapFS{
		"bundle.json":    {Data: m.BundleJSON},
		"tokenizer.json": {Data: m.TokenizerJSON},
	})
	if err != nil {
		return nil, fmt.Errorf("built-in model: %w", err)
	}
	src := ort.ModelSource{Graph: m.Graph}
	if mf.External != "" {
		src.External = map[string][]byte{mf.External: m.Weights}
	}
	return &Model{
		Ref: ModelRef{Repo: mf.Repo, Revision: mf.Revision, Variant: mf.Variant}, Source: "embedded",
		Bundle: b, Graph: src, ID: weightsID(mf.SHA256[mf.Graph], mf.SHA256[mf.External]),
	}, nil
}

func weightsID(sums ...string) string {
	h := sha256.New()
	for _, s := range sums {
		io.WriteString(h, s+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// --- the cache and Hugging Face ---

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func hfEndpoint() string {
	if e := os.Getenv("HF_ENDPOINT"); e != "" {
		return e
	}
	return "https://huggingface.co"
}

func hfHeader() http.Header {
	h := http.Header{}
	tok := os.Getenv("HF_TOKEN")
	if tok == "" {
		tok = os.Getenv("HUGGING_FACE_HUB_TOKEN")
	}
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	return h
}

func hfGetJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header = hfHeader()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 404 {
			hint = " (a private or gated repository needs HF_TOKEN)"
		}
		return fmt.Errorf("%s: %w%s", url, httpError{resp.StatusCode}, hint)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v)
}

// manifest is what a cached model folder records about its files.
type manifest struct {
	Repo     string          `json:"repo"`
	Revision string          `json:"revision"`
	Files    map[string]File `json:"files"`
}

func modelDir(o *Options, repo, sha string) string {
	return filepath.Join(o.cache(), "models", safeName(repo), sha)
}

// revisionSHA resolves a branch or tag to a commit, remembering the answer for offline use.
func revisionSHA(ctx context.Context, o *Options, ref ModelRef) (string, error) {
	if shaRE.MatchString(ref.Revision) {
		return ref.Revision, nil
	}
	refFile := filepath.Join(o.cache(), "models", safeName(ref.Repo), "refs", safeName(ref.Revision))
	if o.Offline {
		if raw, err := os.ReadFile(refFile); err == nil {
			return string(raw), nil
		}
		return "", fmt.Errorf("%s: revision %q: %w", ref.Repo, ref.Revision, ErrOffline)
	}
	var info struct {
		SHA string `json:"sha"`
	}
	if err := hfGetJSON(ctx, fmt.Sprintf("%s/api/models/%s/revision/%s", hfEndpoint(), ref.Repo, ref.Revision), &info); err != nil {
		if raw, rerr := os.ReadFile(refFile); rerr == nil {
			o.logf("could not resolve %s@%s (%v); using the cached %.12s", ref.Repo, ref.Revision, err, raw)
			return string(raw), nil
		}
		return "", err
	}
	if !shaRE.MatchString(info.SHA) {
		return "", fmt.Errorf("%s@%s: the Hub returned no commit", ref.Repo, ref.Revision)
	}
	o.logf("%s@%s is %s; pin it with HBB_MODEL_REVISION=%s", ref.Repo, ref.Revision, info.SHA, info.SHA)
	_ = writeAtomic(refFile, []byte(info.SHA), 0o644)
	return info.SHA, nil
}

// remoteFiles lists a revision's bundle files with their hashes: the lock's when it pins
// this revision, else the Hub's (LFS files carry a sha256; small files a git blob id).
func remoteFiles(ctx context.Context, o *Options, repo, sha, subdir string) (map[string]File, map[string]string, error) {
	if l := pinned(ModelRef{Repo: repo, Revision: sha}); l != nil {
		return l.Files, nil, nil
	}
	var tree []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		OID  string `json:"oid"`
		LFS  *struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"lfs"`
	}
	if err := hfGetJSON(ctx, fmt.Sprintf("%s/api/models/%s/tree/%s/%s", hfEndpoint(), repo, sha, subdir), &tree); err != nil {
		return nil, nil, err
	}
	files, gitOIDs := map[string]File{}, map[string]string{}
	for _, e := range tree {
		if e.Type != "file" {
			continue
		}
		name := path.Base(e.Path)
		if e.LFS != nil {
			files[name] = File{SHA256: e.LFS.OID, Size: e.LFS.Size}
		} else {
			files[name] = File{Size: e.Size}
			gitOIDs[name] = e.OID
		}
	}
	return files, gitOIDs, nil
}

func modelFromCache(ctx context.Context, o *Options, ref ModelRef) (*Model, error) {
	sha, err := revisionSHA(ctx, o, ref)
	if err != nil {
		return nil, err
	}
	l, _ := Locked()
	subdir := l.Model.Subdir
	if subdir == "" {
		subdir = "onnx"
	}
	dir := modelDir(o, ref.Repo, sha)
	mfPath := filepath.Join(dir, "manifest.json")

	var mf manifest
	if raw, err := os.ReadFile(mfPath); err == nil {
		_ = json.Unmarshal(raw, &mf)
	}
	var gitOIDs map[string]string
	if mf.Files == nil {
		if o.Offline {
			return nil, fmt.Errorf("model %s is not in the cache (%s): %w; run `hbb model fetch` once while online",
				ref, dir, ErrOffline)
		}
		if mf.Files, gitOIDs, err = remoteFiles(ctx, o, ref.Repo, sha, subdir); err != nil {
			return nil, err
		}
		mf.Repo, mf.Revision = ref.Repo, sha
	}

	get := func(name string) error {
		want, ok := mf.Files[name]
		if !ok {
			return fmt.Errorf("%s@%.12s has no %s/%s", ref.Repo, sha, subdir, name)
		}
		dst := filepath.Join(dir, name)
		if ok, _ := verified(dst, want.SHA256, want.Size); ok {
			return nil
		}
		url := fmt.Sprintf("%s/%s/resolve/%s/%s/%s", hfEndpoint(), ref.Repo, sha, subdir, name)
		if want.SHA256 == "" {
			return fetchSmall(ctx, o, url, dst, gitOIDs[name], &want, mf.Files, name)
		}
		if want.Size > 50<<20 {
			o.logf("downloading %s (%s) from %s", name, humanBytes(want.Size), ref.Repo)
		}
		return fetch(ctx, o, url, dst, want.SHA256, want.Size, hfHeader())
	}
	for _, f := range []string{"bundle.json", "tokenizer.json"} {
		if err := get(f); err != nil {
			return nil, err
		}
	}
	b, err := bundle.Load(os.DirFS(dir))
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", ref, err)
	}
	file, ext, err := b.Graph(ref.Variant)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", ref, err)
	}
	sums := []string{}
	for _, f := range append([]string{file}, ext...) {
		if err := get(f); err != nil {
			return nil, err
		}
		sums = append(sums, mf.Files[f].SHA256)
	}
	if raw, err := json.MarshalIndent(mf, "", "  "); err == nil {
		_ = writeAtomic(mfPath, raw, 0o644)
	}
	ref.Revision = sha
	return &Model{Ref: ref, Source: "cache", Where: dir, Bundle: b,
		Graph: ort.ModelSource{Path: filepath.Join(dir, file)}, ID: weightsID(sums...)}, nil
}

// fetchSmall downloads a small file the Hub stores in git (no sha256 published), checks
// its git blob id, and records its sha256 in the manifest.
func fetchSmall(ctx context.Context, o *Options, url, dst, gitOID string, want *File, files map[string]File, name string) error {
	if o.Offline {
		return fmt.Errorf("%s: %w", name, ErrOffline)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header = hfHeader()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %w", url, httpError{resp.StatusCode})
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return err
	}
	if gitOID != "" {
		h := sha1.New()
		fmt.Fprintf(h, "blob %d\x00", len(data))
		h.Write(data)
		if got := hex.EncodeToString(h.Sum(nil)); got != gitOID {
			return fmt.Errorf("%s: git blob id %s, the Hub lists %s", url, got, gitOID)
		}
	}
	if err := writeAtomic(dst, data, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	want.SHA256 = hex.EncodeToString(sum[:])
	files[name] = *want
	return markVerified(dst, want.SHA256)
}

// VerifyModel re-hashes a cached model's files against its manifest.
func VerifyModel(m *Model) error {
	if m.Source != "cache" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(m.Where, "manifest.json"))
	if err != nil {
		return err
	}
	var mf manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return err
	}
	var errs []error
	for name, want := range mf.Files {
		p := filepath.Join(m.Where, name)
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue // a variant that was never fetched
		}
		got, err := hashFile(p)
		if err != nil {
			errs = append(errs, err)
		} else if got != want.SHA256 {
			errs = append(errs, fmt.Errorf("%s: sha256 %s, want %s", name, got, want.SHA256))
		} else {
			_ = markVerified(p, got)
		}
	}
	return errors.Join(errs...)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
