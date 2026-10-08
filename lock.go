// Package hotbutteredbeans carries the files every hbb binary is built with. The CLI
// is in cmd/hbb.
package hotbutteredbeans

import _ "embed"

// Lock is hbb.lock.json: the default model and the ONNX Runtime and CUDA downloads,
// each pinned by sha256. Regenerate it with `make lock`.
//
//go:embed hbb.lock.json
var Lock []byte
