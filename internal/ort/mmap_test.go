//go:build linux || darwin

package ort

import (
	"os"
	"syscall"
	"testing"
)

// mapReadOnly maps a file PROT_READ, as the embedded weights sit in a binary's
// read-only data: ONNX Runtime must never write to them.
func mapReadOnly(t *testing.T, path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	b, err := syscall.Mmap(int(f.Fd()), 0, int(st.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Munmap(b) })
	return b
}
