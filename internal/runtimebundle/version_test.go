package runtimebundle

import (
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
)

func TestVersionIsCanonicalSemVer(t *testing.T) {
	got := Version()
	parsed, err := semver.StrictNewVersion(strings.TrimPrefix(got, "v"))
	if err != nil {
		t.Fatalf("Version() = %q, want strict SemVer: %v", got, err)
	}
	if "v"+parsed.String() != got {
		t.Fatalf("Version() = %q, want canonical v-prefixed SemVer", got)
	}
}

func TestImageTagHasDedicatedNamespace(t *testing.T) {
	if got, want := ImageTag(), "runtime-"+Version(); got != want {
		t.Fatalf("ImageTag() = %q, want %q", got, want)
	}
}

func TestBuildVersionOverridesEmbeddedVersion(t *testing.T) {
	original := buildVersion
	t.Cleanup(func() { buildVersion = original })

	buildVersion = "v0.1.0-nightly.0123456789ab"
	if got := Version(); got != buildVersion {
		t.Fatalf("Version() = %q, want build override %q", got, buildVersion)
	}
}
