// Package assets finds, fetches and verifies what hbb runs on: the model bundle and the
// ONNX Runtime library (plus, for GPUs, NVIDIA's CUDA libraries). Each comes from the
// first place that has it: a path the user gave, the binary itself (full builds), the
// cache, the system, and last a pinned download.
package assets

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sync"

	hbb "github.com/xen0bit/hotbutteredbeans"
	"github.com/xen0bit/hotbutteredbeans/internal/buildinfo"
)

// Lock is hbb.lock.json.
type Lock struct {
	Model       ModelLock   `json:"model"`
	ONNXRuntime RuntimeLock `json:"onnxruntime"`
	// CUDA lists, per platform, the NVIDIA wheels (from PyPI) whose libraries the CUDA
	// execution provider needs, for `hbb runtime fetch --gpu`.
	CUDA map[string][]Archive `json:"cuda"`
}

// ModelLock pins a model repository on Hugging Face.
type ModelLock struct {
	Repo     string          `json:"repo"`
	Revision string          `json:"revision"` // a commit sha
	Subdir   string          `json:"subdir"`   // the bundle's folder in the repository
	Variant  string          `json:"variant"`  // the default graph
	Files    map[string]File `json:"files"`    // by path relative to Subdir
}

// File is a file's expected sha256 and size.
type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// RuntimeLock pins ONNX Runtime's release archives by platform ("linux/amd64").
type RuntimeLock struct {
	Version string             `json:"version"`
	CPU     map[string]Archive `json:"cpu"`
	CUDA    map[string]Archive `json:"cuda"`
}

// Archive is a download (a .tgz, .zip or .whl) and its sha256.
type Archive struct {
	Name   string `json:"name,omitempty"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

var (
	lockOnce sync.Once
	lock     Lock
	lockErr  error
)

// Locked is the lock compiled into this binary.
func Locked() (*Lock, error) {
	lockOnce.Do(func() {
		if err := json.Unmarshal(hbb.Lock, &lock); err != nil {
			lockErr = fmt.Errorf("hbb.lock.json: %w", err)
		}
	})
	return &lock, lockErr
}

// Platform is this binary's "goos/goarch".
func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// ModelRef names a model: a Hugging Face repository at a revision, and a graph variant.
type ModelRef struct {
	Repo, Revision, Variant string
}

func (r ModelRef) String() string {
	if r.Revision == "" {
		return fmt.Sprintf("%s (%s)", r.Repo, r.Variant)
	}
	rev := r.Revision
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return fmt.Sprintf("%s@%s (%s)", r.Repo, rev, r.Variant)
}

// DefaultModel is the model this build uses unless told otherwise: the build's stamp,
// else the lock's pin.
func DefaultModel() ModelRef {
	l, _ := Locked()
	r := ModelRef{Repo: l.Model.Repo, Revision: l.Model.Revision, Variant: l.Model.Variant}
	if buildinfo.ModelRepo != "" && buildinfo.ModelRepo != r.Repo {
		r.Repo = buildinfo.ModelRepo
		r.Revision = "" // another repository: the lock's revision is not its
	}
	if buildinfo.ModelRevision != "" {
		r.Revision = buildinfo.ModelRevision
	}
	if buildinfo.ModelVariant != "" {
		r.Variant = buildinfo.ModelVariant
	}
	if r.Revision == "" {
		r.Revision = "main"
	}
	return r
}

// pinned returns the lock's hashes for ref's files, or nil when the lock pins another
// model (the hashes then come from the Hugging Face API).
func pinned(ref ModelRef) *ModelLock {
	l, err := Locked()
	if err != nil || l.Model.Repo != ref.Repo || l.Model.Revision != ref.Revision {
		return nil
	}
	return &l.Model
}
