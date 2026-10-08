package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableColor turns on ANSI escape handling in the Windows console.
func enableColor(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
