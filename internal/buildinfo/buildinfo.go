// Package buildinfo is what a build stamps into hbb: its version, and the model it
// fetches by default. The Makefile and goreleaser set these with -ldflags -X; a plain
// `go install` leaves them empty, and hbb falls back to the module version and the
// model pinned in hbb.lock.json.
package buildinfo

import "runtime/debug"

var (
	Version = ""
	Commit  = ""
	Date    = ""

	// The default model, overriding hbb.lock.json's (Makefile: HBB_MODEL_REPO,
	// HBB_MODEL_REVISION, HBB_MODEL_VARIANT).
	ModelRepo     = ""
	ModelRevision = ""
	ModelVariant  = ""
)

// Ver is the version to report: the stamped one, else the module's (go install), else "dev".
func Ver() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Rev is the commit hbb was built from, if known.
func Rev() string {
	if Commit != "" {
		return Commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}
