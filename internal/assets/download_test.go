package assets

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchResumesAndVerifies(t *testing.T) {
	body := bytes.Repeat([]byte("hot buttered beans "), 50000)
	sum := sha256.Sum256(body)
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		http.ServeContent(w, r, "f", fixedTime, bytes.NewReader(body))
	}))
	defer srv.Close()
	dir := t.TempDir()
	dst := filepath.Join(dir, "f.bin")
	os.WriteFile(dst+".part", body[:1000], 0o644) // an interrupted earlier download
	o := &Options{CacheDir: dir}
	if err := fetch(context.Background(), o, srv.URL, dst, hex.EncodeToString(sum[:]), int64(len(body)), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, body) {
		t.Fatal("content differs")
	}
	if len(ranges) != 1 || ranges[0] != "bytes=1000-" {
		t.Errorf("requests %q: want one resumed request", ranges)
	}
	if ok, _ := verified(dst, hex.EncodeToString(sum[:]), int64(len(body))); !ok {
		t.Error("not recorded as verified")
	}
	bad := filepath.Join(dir, "bad.bin")
	err := fetch(context.Background(), o, srv.URL, bad, strings.Repeat("0", 64), int64(len(body)), nil)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("a wrong hash must fail: %v", err)
	}
	if _, err := os.Stat(bad); err == nil {
		t.Error("a file with the wrong hash was kept")
	}
	if err := fetch(context.Background(), &Options{CacheDir: dir, Offline: true}, srv.URL, filepath.Join(dir, "x"), "", 0, nil); err == nil {
		t.Error("offline must not download")
	}
}

func TestUnpack(t *testing.T) {
	dir := t.TempDir()
	tgz := filepath.Join(dir, "a.tgz")
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	for _, f := range []string{"onnxruntime-linux-x64-1.30.0/lib/libonnxruntime.so.1.30.0", "onnxruntime-linux-x64-1.30.0/include/x.h"} {
		tw.WriteHeader(&tar.Header{Name: f, Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
		tw.Write([]byte("ok"))
	}
	tw.Close()
	gz.Close()
	os.WriteFile(tgz, tb.Bytes(), 0o644)
	var got []string
	err := unpack(tgz, mainLib["linux"].MatchString, func(name string, r io.Reader) error { got = append(got, name); return nil })
	if err != nil || len(got) != 1 || got[0] != "libonnxruntime.so.1.30.0" {
		t.Errorf("tgz: %v %v", got, err)
	}
	zp := filepath.Join(dir, "a.zip")
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("onnxruntime-win-x64-1.30.0/lib/onnxruntime.dll")
	w.Write([]byte("ok"))
	zw.Close()
	os.WriteFile(zp, zb.Bytes(), 0o644)
	got = nil
	err = unpack(zp, mainLib["windows"].MatchString, func(name string, r io.Reader) error { got = append(got, name); return nil })
	if err != nil || len(got) != 1 || got[0] != "onnxruntime.dll" {
		t.Errorf("zip: %v %v", got, err)
	}
}

func TestLockIsComplete(t *testing.T) {
	l, err := Locked()
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Model.Revision) != 40 || l.Model.Files["bundle.json"].SHA256 == "" {
		t.Errorf("model pin: %+v", l.Model)
	}
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/arm64", "windows/amd64", "windows/arm64"} {
		if a := l.ONNXRuntime.CPU[p]; len(a.SHA256) != 64 || a.URL == "" {
			t.Errorf("%s: %+v", p, a)
		}
	}
}

var fixedTime = mustTime()
