package ort

import (
	"os"
	"testing"
)

func mapReadOnly(t *testing.T, path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
