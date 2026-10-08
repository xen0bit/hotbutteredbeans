//go:build !linux

package assets

import "errors"

func memoryFile(string, []byte) (string, error) {
	return "", errors.New("loading a library from memory needs Linux")
}
