// Package runtimebundle exposes the version of the container-side runtime
// bundle. The version covers vaka-init, nft, their filesystem layout, and the
// injected policy contract.
package runtimebundle

import (
	_ "embed"
	"strings"
)

const (
	// ImageTagPrefix keeps runtime bundle tags disjoint from historical helper
	// images that were tagged with the Vaka CLI release version.
	ImageTagPrefix = "runtime-"
	// VersionLabel is stamped on the runtime image and verified after pull.
	VersionLabel = "agent.vaka.runtime.version"
)

//go:embed VERSION
var versionFile string

// buildVersion is set only for release artifacts whose effective runtime
// identity differs from VERSION, currently commit-specific nightly builds.
// Stable and development builds normally leave it empty.
var buildVersion string

// Version returns the exact version expected by both the host CLI and
// vaka-init. VERSION is also the source used by build.sh for image tags.
func Version() string {
	if strings.TrimSpace(buildVersion) != "" {
		return strings.TrimSpace(buildVersion)
	}
	return strings.TrimSpace(versionFile)
}

// ImageTag returns the immutable registry tag for this runtime bundle.
func ImageTag() string {
	return ImageTagPrefix + Version()
}
