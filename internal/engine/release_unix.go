//go:build linux || darwin

package engine

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// release tells the kernel it may drop the pages of embedded data ONNX Runtime has
// copied: they are clean pages of the binary's file, so if anything did read them again
// they would simply be faulted back in from it. Without this, a full build holds the
// weights twice (the binary's mapped copy and ONNX Runtime's).
func release(b []byte) {
	page := uintptr(os.Getpagesize())
	start := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	lo := (start + page - 1) &^ (page - 1)
	hi := (start + uintptr(len(b))) &^ (page - 1)
	if hi <= lo {
		return
	}
	_ = unix.Madvise(unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(b)), lo-start)), hi-lo), unix.MADV_DONTNEED)
}
