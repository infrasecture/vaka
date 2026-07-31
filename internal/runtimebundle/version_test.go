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
