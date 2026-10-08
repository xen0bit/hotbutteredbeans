// Package embedded holds the model and the ONNX Runtime library inside full builds
// (build tag hbb_embed). `make full` (or goreleaser) fetches them into model/ and
// runtime/<goos>_<goarch>/ first; without the tag this package is empty and hbb fetches
// both on first use instead.
//
// The large files are embedded as strings, so they are used in place, from the
// binary's read-only data: the 600 MB of weights are never copied into the heap.
package embedded

import (
	"encoding/json"
	"unsafe"
)

// ModelManifest describes the embedded model (model/manifest.json, written by the fetch).
type ModelManifest struct {
	Repo     string            `json:"repo"`
	Revision string            `json:"revision"`
	Variant  string            `json:"variant"`
	Graph    string            `json:"graph"`    // the graph's file name in the bundle
	External string            `json:"external"` // its one external data file, or ""
	SHA256   map[string]string `json:"sha256"`   // by file name in the bundle
}

// Model is the embedded model, or ok == false in a slim build.
type Model struct {
	Manifest                  ModelManifest
	BundleJSON, TokenizerJSON []byte
	Graph, Weights            []byte // read-only: never write to them
}

// RuntimeManifest describes the embedded ONNX Runtime library.
type RuntimeManifest struct {
	Version string `json:"version"`
	Name    string `json:"name"` // the library's file name
	SHA256  string `json:"sha256"`
}

// LoadModel returns the embedded model.
func LoadModel() (Model, bool) {
	if modelManifest == "" {
		return Model{}, false
	}
	var m Model
	if err := json.Unmarshal([]byte(modelManifest), &m.Manifest); err != nil {
		return Model{}, false
	}
	m.BundleJSON, m.TokenizerJSON = []byte(bundleJSON), []byte(tokenizerJSON)
	m.Graph, m.Weights = bytes(graph), bytes(weights)
	return m, true
}

// LoadRuntime returns the embedded ONNX Runtime library for this platform.
func LoadRuntime() (RuntimeManifest, []byte, bool) {
	if runtimeManifest == "" {
		return RuntimeManifest{}, nil, false
	}
	var m RuntimeManifest
	if err := json.Unmarshal([]byte(runtimeManifest), &m); err != nil {
		return RuntimeManifest{}, nil, false
	}
	return m, bytes(runtimeLib), true
}

// bytes views a string as a byte slice without copying it.
func bytes(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}
