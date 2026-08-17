package runtimebundle

import (
	"path"
	"strings"
	"testing"
)

func TestRuntimeMountLayoutPreservesDirectRootBoundary(t *testing.T) {
	if MountPath == "/" || path.Dir(MountPath) != "/" || path.Clean(MountPath) != MountPath {
		t.Fatalf("MountPath = %q, want a clean direct child of /", MountPath)
	}
	if path.IsAbs(ImageSubpath) || path.Clean(ImageSubpath) != ImageSubpath || ImageSubpath == "." {
		t.Fatalf("ImageSubpath = %q, want a clean relative image path", ImageSubpath)
	}
	for _, helper := range []string{InitPath, NftPath} {
		if !strings.HasPrefix(helper, MountPath+"/") || path.Clean(helper) != helper {
			t.Errorf("helper path %q is not contained by runtime mount %q", helper, MountPath)
		}
	}
}
