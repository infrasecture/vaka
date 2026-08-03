package registry

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type gitTreeEntry struct {
	path     string
	mode     string
	typeName string
	oid      string
}

func gitTreeContainsPath(raw []byte, want string) bool {
	for _, record := range strings.Split(string(raw), "\x00") {
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if ok && name == want && len(fields) == 3 && fields[1] == "blob" {
			return true
		}
	}
	return false
}

// listGitRecipeTree reads the immutable tree directly. Unlike git archive,
// this does not apply export-ignore, export-subst, tar.umask, or any other
// repository/user archive configuration.
func listGitRecipeTree(ctx context.Context, repoDir, commit, name string) ([]gitTreeEntry, error) {
	raw, err := runGit(ctx, repoDir, "ls-tree", "-r", "-t", "-z", commit+":"+name)
	if err != nil {
		return nil, fmt.Errorf("list recipe tree: %w", err)
	}
	entries := []gitTreeEntry{{path: name, mode: "040000", typeName: "tree"}}
	for _, record := range strings.Split(string(raw), "\x00") {
		if record == "" {
			continue
		}
		meta, rel, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("Git returned a malformed tree record")
		}
		mode, typeName, oid := fields[0], fields[1], fields[2]
		if !utf8.ValidString(rel) || !fs.ValidPath(rel) || strings.Contains(rel, `\`) {
			return nil, fmt.Errorf("Git recipe contains unsafe path %q", rel)
		}
		if !gitCommitRE.MatchString(oid) {
			return nil, fmt.Errorf("Git returned an invalid object ID for %q", rel)
		}
		switch {
		case mode == "040000" && typeName == "tree":
		case (mode == "100644" || mode == "100755" || mode == "120000") && typeName == "blob":
		case mode == "160000" && typeName == "commit":
			return nil, fmt.Errorf("Git recipe path %q is a submodule; submodules are not allowed", rel)
		default:
			return nil, fmt.Errorf("Git recipe path %q has unsupported mode/type %s %s", rel, mode, typeName)
		}
		entries = append(entries, gitTreeEntry{
			path: name + "/" + rel, mode: mode, typeName: typeName, oid: oid,
		})
		if len(entries) > maxGitRecipeEntries {
			return nil, fmt.Errorf("Git recipe contains more than %d entries", maxGitRecipeEntries)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	return entries, nil
}

// packageGitRecipe creates a canonical artifact from tree/blob objects only.
// Content, executable bits, and symlink targets affect the digest; commit
// metadata and unrelated repository content do not.
func packageGitRecipe(ctx context.Context, repoDir, commit, name, regDir string) (path, digest string, err error) {
	entries, err := listGitRecipeTree(ctx, repoDir, commit, name)
	if err != nil {
		return "", "", err
	}
	objects, err := startGitBlobReader(ctx, repoDir)
	if err != nil {
		return "", "", err
	}
	objectsOpen := true
	defer func() {
		if objectsOpen {
			objects.abort()
		}
	}()

	tmp, err := os.CreateTemp(regDir, ".git-artifact-*.tar.gz")
	if err != nil {
		return "", "", err
	}
	path = tmp.Name()
	complete := false
	defer func() {
		if !complete {
			_ = tmp.Close()
			_ = os.Remove(path)
		}
	}()

	h := sha256.New()
	limited := &maxWriter{w: io.MultiWriter(tmp, h), remaining: maxTarballBytes}
	gz := gzip.NewWriter(limited)
	gz.Header.ModTime = time.Time{}
	tw := tar.NewWriter(gz)
	var total int64
	for _, entry := range entries {
		hdr := &tar.Header{
			Name:       entry.path,
			ModTime:    time.Unix(0, 0).UTC(),
			AccessTime: time.Time{},
			ChangeTime: time.Time{},
			Format:     tar.FormatPAX,
		}
		switch entry.mode {
		case "040000":
			hdr.Name += "/"
			hdr.Mode = 0o755
			hdr.Typeflag = tar.TypeDir
			if err := tw.WriteHeader(hdr); err != nil {
				return "", "", fmt.Errorf("write recipe directory %q: %w", entry.path, err)
			}

		case "100644", "100755":
			err := objects.readBlob(entry.oid, func(size int64, body io.Reader) error {
				if size > maxGitRecipeFileBytes {
					return fmt.Errorf("Git recipe file %q exceeds the %d byte limit", entry.path, maxGitRecipeFileBytes)
				}
				total += size
				if total > maxGitRecipeBytes {
					return fmt.Errorf("Git recipe exceeds the %d byte unpacked limit", maxGitRecipeBytes)
				}
				hdr.Mode = 0o644
				if entry.mode == "100755" {
					hdr.Mode = 0o755
				}
				hdr.Typeflag = tar.TypeReg
				hdr.Size = size
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				_, err := io.CopyN(tw, body, size)
				return err
			})
			if err != nil {
				return "", "", fmt.Errorf("read Git recipe file %q: %w", entry.path, err)
			}

		case "120000":
			err := objects.readBlob(entry.oid, func(size int64, body io.Reader) error {
				if size > maxGitRecipeFileBytes {
					return fmt.Errorf("Git recipe symlink %q exceeds the %d byte limit", entry.path, maxGitRecipeFileBytes)
				}
				total += size
				if total > maxGitRecipeBytes {
					return fmt.Errorf("Git recipe exceeds the %d byte unpacked limit", maxGitRecipeBytes)
				}
				target, err := io.ReadAll(body)
				if err != nil {
					return err
				}
				hdr.Mode = 0o777
				hdr.Typeflag = tar.TypeSymlink
				hdr.Linkname = string(target)
				return tw.WriteHeader(hdr)
			})
			if err != nil {
				return "", "", fmt.Errorf("read Git recipe symlink %q: %w", entry.path, err)
			}
		}
	}
	if err := objects.close(); err != nil {
		return "", "", err
	}
	objectsOpen = false

	tarErr := tw.Close()
	gzipErr := gz.Close()
	fileErr := tmp.Close()
	if limited.err != nil {
		return "", "", limited.err
	}
	if tarErr != nil {
		return "", "", tarErr
	}
	if gzipErr != nil {
		return "", "", gzipErr
	}
	if fileErr != nil {
		return "", "", fileErr
	}
	digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	complete = true
	return path, digest, nil
}

type gitBlobReader struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  *cappedBuffer
	repoDir string
	closed  bool
}

func startGitBlobReader(ctx context.Context, repoDir string) (*gitBlobReader, error) {
	cmd := gitCommand(ctx, repoDir, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr := &cappedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, gitError(err, stderr.String())
	}
	return &gitBlobReader{
		cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr, repoDir: repoDir,
	}, nil
}

func (r *gitBlobReader) readBlob(oid string, consume func(size int64, body io.Reader) error) error {
	if r.closed {
		return fmt.Errorf("Git object reader is closed")
	}
	if _, err := io.WriteString(r.stdin, oid+"\n"); err != nil {
		return gitError(err, r.stderr.String())
	}
	header, err := r.stdout.ReadString('\n')
	if err != nil {
		return gitError(err, r.stderr.String())
	}
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[1] == "missing" {
		return fmt.Errorf("Git object %s is missing", oid)
	}
	if len(fields) != 3 || !gitCommitRE.MatchString(fields[0]) || fields[1] != "blob" {
		return fmt.Errorf("Git returned malformed blob header %q", strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		return fmt.Errorf("Git returned invalid blob size %q", fields[2])
	}
	body := &io.LimitedReader{R: r.stdout, N: size}
	if err := consume(size, body); err != nil {
		return err
	}
	if body.N != 0 {
		return fmt.Errorf("Git blob consumer left %d unread bytes", body.N)
	}
	terminator, err := r.stdout.ReadByte()
	if err != nil {
		return gitError(err, r.stderr.String())
	}
	if terminator != '\n' {
		return fmt.Errorf("Git returned a malformed blob terminator")
	}
	return checkGitObjectStore(r.repoDir)
}

func (r *gitBlobReader) close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	_ = r.stdin.Close()
	err := r.cmd.Wait()
	if r.stderr.exceeded {
		return fmt.Errorf("Git output exceeds the %d byte limit", maxGitOutputBytes)
	}
	if err != nil {
		return gitError(err, r.stderr.String())
	}
	return nil
}

func (r *gitBlobReader) abort() {
	if r.closed {
		return
	}
	r.closed = true
	_ = r.stdin.Close()
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
	}
	_ = r.cmd.Wait()
}

func runGitWithStoreLimit(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommand(commandCtx, repoDir, args...)
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, gitError(err, stderr.String())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if sizeErr := checkGitObjectStore(repoDir); sizeErr != nil {
				return nil, sizeErr
			}
			if stdout.exceeded || stderr.exceeded {
				return nil, fmt.Errorf("Git output exceeds the %d byte limit", maxGitOutputBytes)
			}
			if err != nil {
				return nil, gitError(err, stderr.String())
			}
			return stdout.Bytes(), nil

		case <-ticker.C:
			if sizeErr := checkGitObjectStore(repoDir); sizeErr != nil {
				cancel()
				<-done
				return nil, sizeErr
			}
		}
	}
}

func checkGitObjectStore(repoDir string) error {
	_, err := directorySizeAtMost(repoDir, maxGitObjectStoreBytes)
	if err != nil {
		return fmt.Errorf("temporary Git object store: %w", err)
	}
	return nil
}

func directorySizeAtMost(root string, limit int64) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		if total > limit {
			return fmt.Errorf("exceeds the %d byte aggregate limit", limit)
		}
		return nil
	})
	return total, err
}

type maxWriter struct {
	w         io.Writer
	remaining int64
	err       error
}

func (w *maxWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.err = fmt.Errorf("generated artifact exceeds the %d byte limit", maxTarballBytes)
		return 0, w.err
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	if err != nil {
		w.err = err
	}
	return n, err
}
