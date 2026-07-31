// Package runtimebundle exposes the version of the container-side runtime
// bundle. The version covers vaka-init, nft, their filesystem layout, and the
// injected policy contract.
package runtimebundle

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var versionFile string

// Version returns the exact version expected by both the host CLI and
// vaka-init. VERSION is also the source used by build.sh for image tags.
func Version() string {
	return strings.TrimSpace(versionFile)
}
