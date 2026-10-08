//go:build !hbb_embed || !((linux && (amd64 || arm64)) || (darwin && arm64) || (windows && (amd64 || arm64)))

package embedded

var runtimeManifest, runtimeLib string
