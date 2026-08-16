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
	// PolicyEnvironment and PolicyRevisionEnvironment are stored in the
	// immutable container configuration and inherited by every Vaka trampoline.
	PolicyEnvironment         = "AGENT_VAKA_POLICY"
	PolicyRevisionEnvironment = "AGENT_VAKA_POLICY_REVISION"
)

//go:embed VERSION
var versionFile string

// buildVersion is set by build.sh so the CLI and vaka-init share the selected
// effective identity. It differs from VERSION for commit-specific nightlies;
// direct Go builds leave it empty and use the committed stable identity.
var buildVersion string

// Version returns the exact version expected by both the host CLI and
// vaka-init. VERSION is the stable base used by build.sh for image tags.
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
