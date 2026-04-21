//go:build linux

package main

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"vaka.dev/vaka/pkg/policy"
)

var (
	readMountInfoFn = readMountInfo
	lchownFn        = os.Lchown
)

type mountInfoEntry struct {
	MountPoint string
	FSType     string
	MountOpts  map[string]bool
}

func (m mountInfoEntry) isReadOnly() bool {
	return m.MountOpts["ro"]
}

func applyChownActions(actions []policy.ChownAction, defaultOwner *execIdentity, passwd, group string) error {
	if len(actions) == 0 {
		return nil
	}

	mounts, err := readMountInfoFn("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}

	for i, action := range actions {
		idx := fmt.Sprintf("runtime.chown[%d]", i)

		path := filepath.Clean(strings.TrimSpace(action.Path))
		if path == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("%s.path: must be an absolute path, got %q", idx, action.Path)
		}
		if _, err := os.Lstat(path); err != nil {
			return fmt.Errorf("%s.path %q: %w", idx, path, err)
		}

		mount := findMount(path, mounts)
		if mount == nil {
			return fmt.Errorf("%s.path %q: no matching mount found", idx, path)
		}
		if mount.MountPoint == "/" {
			return fmt.Errorf("%s.path %q: path is on root filesystem mount \"/\" (runtime.chown is volumes-only)", idx, path)
		}
		if mount.isReadOnly() {
			return fmt.Errorf("%s.path %q: mount %q is read-only", idx, path, mount.MountPoint)
		}

		owner, err := resolveChownOwner(action.Owner, defaultOwner, passwd, group)
		if err != nil {
			return fmt.Errorf("%s: %w", idx, err)
		}
		if err := chownPath(path, owner.UID, owner.GID, action.Recursive); err != nil {
			return fmt.Errorf("%s.path %q: %w", idx, path, err)
		}
	}
	return nil
}

func resolveChownOwner(spec string, defaultOwner *execIdentity, passwd, group string) (*execIdentity, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		if defaultOwner == nil {
			return nil, fmt.Errorf("owner omitted but generated services.<name>.user is empty")
		}
		return defaultOwner, nil
	}
	owner, err := resolveExecUser(trimmed, passwd, group)
	if err != nil {
		return nil, fmt.Errorf("resolve owner %q: %w", spec, err)
	}
	if owner == nil {
		return nil, fmt.Errorf("resolve owner %q: empty owner is not allowed", spec)
	}
	return owner, nil
}

func chownPath(path string, uid, gid int, recursive bool) error {
	if !recursive {
		if err := lchownFn(path, uid, gid); err != nil {
			return fmt.Errorf("lchown: %w", err)
		}
		return nil
	}
	return filepath.WalkDir(path, func(walkPath string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := lchownFn(walkPath, uid, gid); err != nil {
			return fmt.Errorf("lchown %q: %w", walkPath, err)
		}
		return nil
	})
}

func readMountInfo(path string) ([]mountInfoEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountInfo(f)
}

func parseMountInfo(r io.Reader) ([]mountInfoEntry, error) {
	scanner := bufio.NewScanner(r)
	var out []mountInfoEntry
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		entry, err := parseMountInfoLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNum, err)
		}
		out = append(out, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseMountInfoLine(line string) (mountInfoEntry, error) {
	parts := strings.SplitN(line, " - ", 2)
	if len(parts) != 2 {
		return mountInfoEntry{}, fmt.Errorf("invalid mountinfo line: missing separator")
	}
	left := strings.Fields(parts[0])
	right := strings.Fields(parts[1])
	if len(left) < 6 {
		return mountInfoEntry{}, fmt.Errorf("invalid mountinfo line: expected at least 6 fields before separator")
	}
	if len(right) < 1 {
		return mountInfoEntry{}, fmt.Errorf("invalid mountinfo line: expected filesystem type after separator")
	}

	mountPoint := decodeMountInfoField(left[4])
	mountPoint = filepath.Clean(mountPoint)

	return mountInfoEntry{
		MountPoint: mountPoint,
		FSType:     right[0],
		MountOpts:  parseMountOpts(left[5]),
	}, nil
}

func parseMountOpts(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		opt := strings.TrimSpace(part)
		if opt == "" {
			continue
		}
		out[opt] = true
	}
	return out
}

func findMount(path string, mounts []mountInfoEntry) *mountInfoEntry {
	cleaned := filepath.Clean(path)
	var best *mountInfoEntry
	bestLen := -1
	for i := range mounts {
		mp := filepath.Clean(mounts[i].MountPoint)
		if !pathWithinMount(cleaned, mp) {
			continue
		}
		if len(mp) > bestLen {
			best = &mounts[i]
			bestLen = len(mp)
		}
	}
	return best
}

func pathWithinMount(path, mountPoint string) bool {
	if mountPoint == "/" {
		return filepath.IsAbs(path)
	}
	if path == mountPoint {
		return true
	}
	return strings.HasPrefix(path, mountPoint+"/")
}

func decodeMountInfoField(in string) string {
	var b strings.Builder
	b.Grow(len(in))
	for i := 0; i < len(in); i++ {
		if in[i] == '\\' && i+3 < len(in) &&
			isOctalDigit(in[i+1]) &&
			isOctalDigit(in[i+2]) &&
			isOctalDigit(in[i+3]) {
			v := (int(in[i+1]-'0') << 6) | (int(in[i+2]-'0') << 3) | int(in[i+3]-'0')
			b.WriteByte(byte(v))
			i += 3
			continue
		}
		b.WriteByte(in[i])
	}
	return b.String()
}

func isOctalDigit(b byte) bool {
	return b >= '0' && b <= '7'
}
