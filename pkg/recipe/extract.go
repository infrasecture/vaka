package recipe

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
)

// Extraction limits (decompression-bomb defense). Vars so tests can lower
// them; production values come from the Phase 1 plan.
var (
	maxUnpackedBytes int64 = 50 << 20
	maxEntries             = 2000
	maxFileBytes     int64 = 20 << 20
)

// reservedPrefix is vaka's own state namespace inside a recipe directory.
// Archives shipping such paths could forge provenance and are refused.
const reservedPrefix = ".vaka-"

// validRecipePath reports whether p is a legal recipe-relative path key for a
// lock or journal: a canonical, slash-separated relative path that stays inside
// the recipe tree and never names vaka's reserved .vaka-* state. Enforcing this
// on lock.Files, deviations, and journal.Plan keys stops a corrupt-but-
// syntactically-valid document from steering the updater to unlink or overwrite
// its own reserved state (e.g. the held update lock) or a path outside the tree.
func validRecipePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q must be relative", p)
	}
	if clean := path.Clean(p); clean != p {
		return fmt.Errorf("path %q is not canonical (want %q)", p, clean)
	}
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("path %q escapes the recipe directory", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, reservedPrefix) {
			return fmt.Errorf("path %q is inside the reserved %s* namespace", p, reservedPrefix)
		}
	}
	return nil
}

// ExtractRecipe extracts a digest-verified recipe tarball into root (a
// confinement root over an existing, empty directory). The archive must
// contain exactly one top-level directory named recipeName, which is
// stripped. Passing the caller's own SafeRoot keeps every write beneath the
// confinement it already holds — no path is re-resolved from the filesystem
// root.
//
// Hardening (design §7): path traversal, absolute paths, hardlinks, and
// device/special entries are rejected; symlinks must have relative,
// in-tree targets; reserved `.vaka-*` paths are refused; only the
// executable bit survives from archive modes; size, file-size, and
// entry-count limits bound extraction.
func ExtractRecipe(tarball io.Reader, recipeName string, root *SafeRoot) error {
	gz, err := gzip.NewReader(tarball)
	if err != nil {
		return fmt.Errorf("recipe archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var (
		entries    int
		totalBytes int64
		files      int
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("recipe archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		entries++
		if entries > maxEntries {
			return fmt.Errorf("recipe archive: more than %d entries", maxEntries)
		}

		rel, err := stripRecipeRoot(hdr.Name, recipeName)
		if err != nil {
			return err
		}
		if rel == "" {
			continue // the top-level directory itself
		}
		for _, seg := range strings.Split(rel, "/") {
			if strings.HasPrefix(seg, reservedPrefix) {
				return fmt.Errorf("recipe archive: entry %q is inside the reserved %s* namespace", hdr.Name, reservedPrefix)
			}
		}

		// Create the entry's parent directories independently of whether the
		// archive included explicit directory entries (tar producers vary);
		// every path segment was already reserved-namespace-checked above.
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeSymlink {
			if dir := path.Dir(rel); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("recipe archive: %s: %w", rel, err)
				}
			}
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(rel, 0o755); err != nil {
				return fmt.Errorf("recipe archive: %s: %w", rel, err)
			}

		case tar.TypeReg:
			if hdr.Size > maxFileBytes {
				return fmt.Errorf("recipe archive: %s exceeds the %d byte file limit", rel, maxFileBytes)
			}
			totalBytes += hdr.Size
			if totalBytes > maxUnpackedBytes {
				return fmt.Errorf("recipe archive: unpacked size exceeds the %d byte limit", maxUnpackedBytes)
			}
			perm := fs.FileMode(0o644)
			if hdr.FileInfo().Mode().Perm()&0o111 != 0 {
				perm = 0o755
			}
			f, err := root.CreateExcl(rel, perm)
			if err != nil {
				return fmt.Errorf("recipe archive: %s: %w", rel, err)
			}
			n, err := io.Copy(f, io.LimitReader(tr, maxFileBytes+1))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("recipe archive: %s: %w", rel, err)
			}
			if n > maxFileBytes {
				return fmt.Errorf("recipe archive: %s exceeds the %d byte file limit", rel, maxFileBytes)
			}

		case tar.TypeSymlink:
			if err := validateLinkTarget(rel, hdr.Linkname); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, rel); err != nil {
				return fmt.Errorf("recipe archive: %s: %w", rel, err)
			}

		case tar.TypeLink:
			return fmt.Errorf("recipe archive: %s: hardlinks are not allowed", rel)

		default:
			return fmt.Errorf("recipe archive: %s: entry type %q is not allowed", rel, hdr.Typeflag)
		}
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeSymlink {
			files++
		}
	}

	if files == 0 {
		return fmt.Errorf("recipe archive: no files under top-level directory %q", recipeName)
	}
	return nil
}

// stripRecipeRoot validates an archive entry name against the single
// required top-level directory and returns the in-recipe relative path
// ("" for the top-level directory entry itself).
func stripRecipeRoot(name, recipeName string) (string, error) {
	clean := strings.TrimSuffix(name, "/")
	if clean == recipeName {
		return "", nil
	}
	rel, ok := strings.CutPrefix(clean, recipeName+"/")
	if !ok {
		return "", fmt.Errorf(
			"recipe archive: entry %q is outside the required top-level directory %q", name, recipeName)
	}
	if !fs.ValidPath(rel) || strings.Contains(rel, `\`) {
		return "", fmt.Errorf("recipe archive: entry %q has an unsafe path", name)
	}
	return rel, nil
}

// validateLinkTarget enforces the symlink policy: relative targets only,
// resolving inside the recipe directory.
func validateLinkTarget(rel, target string) error {
	if target == "" {
		return fmt.Errorf("recipe archive: %s: empty symlink target", rel)
	}
	if path.IsAbs(target) || strings.HasPrefix(target, "/") {
		return fmt.Errorf("recipe archive: %s: absolute symlink target %q is not allowed", rel, target)
	}
	if strings.Contains(target, `\`) {
		return fmt.Errorf("recipe archive: %s: unsafe symlink target %q", rel, target)
	}
	resolved := path.Join(path.Dir(rel), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("recipe archive: %s: symlink target %q escapes the recipe directory", rel, target)
	}
	return nil
}
