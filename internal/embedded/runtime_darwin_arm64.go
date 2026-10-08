//go:build hbb_embed

package embedded

import _ "embed"

var (
	//go:embed runtime/darwin_arm64/manifest.json
	runtimeManifest string
	//go:embed runtime/darwin_arm64/lib.bin
	runtimeLib string
)
