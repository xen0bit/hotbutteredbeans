//go:build hbb_embed

package embedded

import _ "embed"

var (
	//go:embed model/manifest.json
	modelManifest string
	//go:embed model/bundle.json
	bundleJSON string
	//go:embed model/tokenizer.json
	tokenizerJSON string
	//go:embed model/graph.onnx
	graph string
	//go:embed model/weights.bin
	weights string
)
