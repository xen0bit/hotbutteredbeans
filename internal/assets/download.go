package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xen0bit/hotbutteredbeans/internal/buildinfo"
)

// Progress reports a download: bytes done of total (total < 0 when unknown).
type Progress func(name string, done, total int64)

// Options control where assets come from.
type Options struct {
	CacheDir string   // default CacheDir()
	Offline  bool     // never download
	Progress Progress // nil: silent
	Log      func(format string, args ...any)
}

func (o *Options) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Logf logs through o.Log, if set.
func (o *Options) Logf(format string, args ...any) { o.logf(format, args...) }

func (o *Options) cache() string {
	if o.CacheDir != "" {
		return o.CacheDir
	}
	return CacheDir()
}

// CacheDirOrDefault is o.CacheDir, else CacheDir().
func (o *Options) CacheDirOrDefault() string { return o.cache() }

// CacheDir is $HBB_CACHE_DIR, else hbb's folder in the user cache directory
// (~/.cache/hbb, ~/Library/Caches/hbb, %LocalAppData%\hbb).
func CacheDir() string {
	if d := os.Getenv("HBB_CACHE_DIR"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "hbb")
	}
	return filepath.Join(os.TempDir(), "hbb-cache")
}

// ErrOffline is returned when an asset is missing and downloads are off.
var ErrOffline = errors.New("not available offline")

var client = &http.Client{Transport: http.DefaultTransport} // honours HTTPS_PROXY

// fetch downloads url to dst, checking its size and sha256 (lowercase hex; "" skips the
// check, for the small files of an unpinned model), and resuming a partial download
// left by an earlier run. Concurrent fetches of the same dst wait for each other.
func fetch(ctx context.Context, o *Options, url, dst, sum string, size int64, header http.Header) error {
	if o.Offline {
		return fmt.Errorf("%s: %w (unset --offline / HBB_OFFLINE to download it)", filepath.Base(dst), ErrOffline)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	unlock, err := lockFile(dst + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if ok, _ := verified(dst, sum, size); ok { // another process finished it while we waited
		return nil
	}

	part := dst + ".part"
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	have, _ := f.Seek(0, io.SeekEnd)
	if size > 0 && have > size {
		have = 0
	}

	for attempt := 0; ; attempt++ {
		err = get(ctx, o, url, f, &have, size, header, filepath.Base(dst))
		if err == nil || ctx.Err() != nil || attempt == 4 || !retryable(err) {
			break
		}
		o.logf("download of %s interrupted (%v), retrying", filepath.Base(dst), err)
		select {
		case <-time.After(time.Duration(1<<attempt) * time.Second):
		case <-ctx.Done():
		}
	}
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if (size > 0 && n != size) || (sum != "" && got != sum) {
		f.Close()
		os.Remove(part)
		return fmt.Errorf("%s: got %d bytes with sha256 %s, want %d bytes with sha256 %s; the partial file was removed, try again",
			url, n, got, size, sum)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	return markVerified(dst, got)
}

type httpError struct{ code int }

func (e httpError) Error() string { return fmt.Sprintf("HTTP %d %s", e.code, http.StatusText(e.code)) }

func retryable(err error) bool {
	var he httpError
	if errors.As(err, &he) {
		return he.code == 429 || he.code >= 500
	}
	return true // a network error
}

func get(ctx context.Context, o *Options, url string, f *os.File, have *int64, size int64, header http.Header, name string) error {
	if size > 0 && *have == size {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("User-Agent", "hbb/"+buildinfo.Ver())
	if *have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", *have))
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent && *have > 0:
	case resp.StatusCode == http.StatusOK:
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		*have = 0
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		*have = 0
		f.Truncate(0)
		f.Seek(0, io.SeekStart)
		return httpError{resp.StatusCode}
	default:
		return httpError{resp.StatusCode}
	}
	total := size
	if total <= 0 && resp.ContentLength > 0 {
		total = *have + resp.ContentLength
	}
	buf := make([]byte, 1<<20)
	last := time.Time{}
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			*have += int64(n)
			if o.Progress != nil && time.Since(last) > 200*time.Millisecond {
				o.Progress(name, *have, total)
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			if o.Progress != nil {
				o.Progress(name, *have, total)
			}
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// A verified file has a sidecar "<file>.sha256" holding its sha256, size and mtime, so a
// 600 MB model is hashed once, not on every run.

func markVerified(path, sum string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path+".sha256", fmt.Appendf(nil, "%s %d %d\n", sum, st.Size(), st.ModTime().UnixNano()), 0o644)
}

// verified reports whether path exists and matches sum and size (either may be empty/0:
// then only existence and the sidecar's own record count).
func verified(path, sum string, size int64) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if size > 0 && st.Size() != size {
		return false, nil
	}
	raw, err := os.ReadFile(path + ".sha256")
	if err == nil {
		var s string
		var n, mt int64
		if _, err := fmt.Sscanf(string(raw), "%s %d %d", &s, &n, &mt); err == nil &&
			n == st.Size() && mt == st.ModTime().UnixNano() {
			return sum == "" || s == sum, nil
		}
	}
	if sum == "" {
		return false, nil
	}
	got, err := hashFile(path)
	if err != nil {
		return false, err
	}
	if got != sum {
		return false, nil
	}
	return true, markVerified(path, got)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeAtomic writes data to path through a temporary file and a rename.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// safeName turns a repository name into one path element.
func safeName(s string) string {
	return strings.NewReplacer("/", "__", "\\", "__", ":", "_").Replace(s)
}
