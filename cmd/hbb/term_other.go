//go:build !windows

package main

import "os"

func enableColor(*os.File) bool { return true }
