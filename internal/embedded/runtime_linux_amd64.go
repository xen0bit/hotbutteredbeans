//go:build hbb_embed

package embedded

import _ "embed"

var (
	//go:embed runtime/linux_amd64/manifest.json
	runtimeManifest string
	//go:embed runtime/linux_amd64/lib.bin
	runtimeLib string
)
