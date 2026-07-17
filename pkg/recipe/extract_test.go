package recipe

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name     string
	typeflag byte
	content  string
	mode     int64
	linkname string
}

func makeTarGz(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
			if e.typeflag == tar.TypeDir {
				mode = 0o755
			}
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     mode,
			Size:     int64(len(e.content)),
			Linkname: e.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.content != "" {
			if _, err := tw.Write([]byte(e.content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// goodRecipe mirrors the codex layout: files, a subdir, an executable, and
// an in-tree symlink.
func goodRecipe(t *testing.T) *bytes.Buffer {
	return makeTarGz(t, []tarEntry{
		{name: "demo/", typeflag: tar.TypeDir},
		{name: "demo/recipe.yaml", typeflag: tar.TypeReg, content: validRecipeManifest},
		{name: "demo/compose.yaml", typeflag: tar.TypeReg, content: "services:\n  app:\n    image: alpine:3.20\n"},
		{name: "demo/docker-compose.yaml", typeflag: tar.TypeSymlink, linkname: "compose.yaml"},
		{name: "demo/vaka.yaml", typeflag: tar.TypeReg, content: validVakaYAML},
		{name: "demo/run.sh", typeflag: tar.TypeReg, content: "#!/bin/sh\n", mode: 0o755},
		{name: "demo/conf/", typeflag: tar.TypeDir},
		{name: "demo/conf/app.yaml", typeflag: tar.TypeReg, content: "x: 1\n"},
	})
}

// validVakaYAML is a minimal ServicePolicy that passes policy.ValidateHost.
const validVakaYAML = `apiVersion: agent.vaka/v1alpha1
kind: ServicePolicy
services:
  app:
    network:
      egress:
        defaultAction: reject
`

// validRecipeManifest is a well-formed recipe.yaml (content is not parsed by
// ValidateStaged, only its presence is required).
const validRecipeManifest = `apiVersion: recipes.vaka/v1alpha1
kind: Recipe
name: demo
version: 1.0.0
description: test recipe fixture
`

func extractInto(t *testing.T, buf *bytes.Buffer) (string, error) {
	t.Helper()
	dest := t.TempDir()
	root, err := OpenSafeRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	return dest, ExtractRecipe(buf, "demo", root)
}

func TestExtractRecipeHappyPath(t *testing.T) {
	dest, err := extractInto(t, goodRecipe(t))
	if err != nil {
		t.Fatalf("ExtractRecipe: %v", err)
	}

	root, err := OpenSafeRoot(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	state, err := EntryState(root, "compose.yaml")
	if err != nil || !strings.HasPrefix(state, "sha256:") || strings.HasSuffix(state, "+x") {
		t.Fatalf("compose.yaml state = %q, %v", state, err)
	}
	state, err = EntryState(root, "run.sh")
	if err != nil || !strings.HasSuffix(state, "+x") {
		t.Fatalf("run.sh state = %q, %v (exec bit must survive)", state, err)
	}
	state, err = EntryState(root, "docker-compose.yaml")
	if err != nil || state != "link:compose.yaml" {
		t.Fatalf("symlink state = %q, %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "conf", "app.yaml")); err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
}

func TestExtractRecipeAdversarialArchives(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{
			name:    "path traversal after strip",
			entries: []tarEntry{{name: "demo/../evil", typeflag: tar.TypeReg, content: "x"}},
			want:    "unsafe path",
		},
		{
			name:    "deep traversal",
			entries: []tarEntry{{name: "demo/sub/../../../evil", typeflag: tar.TypeReg, content: "x"}},
			want:    "unsafe path",
		},
		{
			name:    "absolute path",
			entries: []tarEntry{{name: "/etc/passwd", typeflag: tar.TypeReg, content: "x"}},
			want:    "outside the required top-level directory",
		},
		{
			name:    "second top-level directory",
			entries: []tarEntry{{name: "demo/a", typeflag: tar.TypeReg, content: "x"}, {name: "other/b", typeflag: tar.TypeReg, content: "x"}},
			want:    "outside the required top-level directory",
		},
		{
			name:    "wrong top-level directory only",
			entries: []tarEntry{{name: "other/a", typeflag: tar.TypeReg, content: "x"}},
			want:    "outside the required top-level directory",
		},
		{
			name:    "escaping symlink",
			entries: []tarEntry{{name: "demo/link", typeflag: tar.TypeSymlink, linkname: "../../outside"}},
			want:    "escapes the recipe directory",
		},
		{
			name:    "absolute symlink",
			entries: []tarEntry{{name: "demo/link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
			want:    "absolute symlink target",
		},
		{
			name:    "hardlink",
			entries: []tarEntry{{name: "demo/a", typeflag: tar.TypeReg, content: "x"}, {name: "demo/b", typeflag: tar.TypeLink, linkname: "demo/a"}},
			want:    "hardlinks are not allowed",
		},
		{
			name:    "character device",
			entries: []tarEntry{{name: "demo/dev", typeflag: tar.TypeChar}},
			want:    "not allowed",
		},
		{
			name:    "fifo",
			entries: []tarEntry{{name: "demo/pipe", typeflag: tar.TypeFifo}},
			want:    "not allowed",
		},
		{
			name:    "reserved lock file",
			entries: []tarEntry{{name: "demo/.vaka-recipe.lock", typeflag: tar.TypeReg, content: "forged"}},
			want:    "reserved .vaka-* namespace",
		},
		{
			name:    "reserved nested staging dir",
			entries: []tarEntry{{name: "demo/sub/.vaka-staging/x", typeflag: tar.TypeReg, content: "x"}},
			want:    "reserved .vaka-* namespace",
		},
		{
			name:    "duplicate file entry",
			entries: []tarEntry{{name: "demo/a", typeflag: tar.TypeReg, content: "x"}, {name: "demo/a", typeflag: tar.TypeReg, content: "y"}},
			want:    "file exists",
		},
		{
			name:    "empty archive",
			entries: nil,
			want:    "no files under top-level directory",
		},
		{
			name:    "directories only",
			entries: []tarEntry{{name: "demo/", typeflag: tar.TypeDir}},
			want:    "no files under top-level directory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dest, err := extractInto(t, makeTarGz(t, tc.entries))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
			// Nothing outside dest may have been created; dest itself may
			// hold partial content — install (commit 3) stages and discards.
			if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); !os.IsNotExist(err) {
				t.Fatalf("traversal escaped the destination: %v", err)
			}
		})
	}
}

func TestExtractRecipeLimits(t *testing.T) {
	t.Run("entry count", func(t *testing.T) {
		old := maxEntries
		maxEntries = 3
		defer func() { maxEntries = old }()
		entries := []tarEntry{{name: "demo/", typeflag: tar.TypeDir}}
		for _, n := range []string{"a", "b", "c"} {
			entries = append(entries, tarEntry{name: "demo/" + n, typeflag: tar.TypeReg, content: "x"})
		}
		_, err := extractInto(t, makeTarGz(t, entries))
		if err == nil || !strings.Contains(err.Error(), "more than 3 entries") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("single file size", func(t *testing.T) {
		old := maxFileBytes
		maxFileBytes = 8
		defer func() { maxFileBytes = old }()
		_, err := extractInto(t, makeTarGz(t, []tarEntry{
			{name: "demo/big", typeflag: tar.TypeReg, content: "123456789"},
		}))
		if err == nil || !strings.Contains(err.Error(), "byte file limit") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("total size", func(t *testing.T) {
		old := maxUnpackedBytes
		maxUnpackedBytes = 10
		defer func() { maxUnpackedBytes = old }()
		_, err := extractInto(t, makeTarGz(t, []tarEntry{
			{name: "demo/a", typeflag: tar.TypeReg, content: "123456"},
			{name: "demo/b", typeflag: tar.TypeReg, content: "123456"},
		}))
		if err == nil || !strings.Contains(err.Error(), "unpacked size exceeds") {
			t.Fatalf("err = %v", err)
		}
	})
}
