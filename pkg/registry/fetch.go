package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const (
	// maxIndexBytes caps an index download; the design's growth mitigations
	// keep real indexes far below this.
	maxIndexBytes = 32 << 20
)

// maxTarballBytes caps a recipe tarball download (var so tests can lower it).
var maxTarballBytes int64 = 256 << 20

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// httpDo is the transport seam: tests replace it to run fully offline.
var httpDo = func(req *http.Request) (*http.Response, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	return client.Do(req)
}

// Client fetches registry indexes (with an ETag cache) and recipe tarballs.
type Client struct {
	// CacheDir holds per-registry index caches. Empty means
	// os.UserCacheDir()/vaka/registries.
	CacheDir string
	// MaxIndexAge is the freshness window: a cached index younger than this
	// is served without any network round-trip. Zero always revalidates.
	MaxIndexAge time.Duration
}

// IndexResult is a fetched index plus its provenance.
type IndexResult struct {
	Index *Index
	// Stale is true when the network fetch failed and a previously cached
	// index was served instead; callers must surface this loudly.
	Stale bool
	// Age is the cache age when the index was served from cache.
	Age time.Duration
}

func (c *Client) cacheDir() (string, error) {
	if c.CacheDir != "" {
		return c.CacheDir, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(dir, "vaka", "registries"), nil
}

func validateIndexURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "https", "file":
		return nil
	case "http":
		return fmt.Errorf("plain http URLs are not allowed (%q); registries require https (file:// is allowed for local testing)", raw)
	default:
		return fmt.Errorf("unsupported URL scheme %q in %q (https and file only)", u.Scheme, raw)
	}
}

// FetchIndex returns the registry's index, revalidating the cache per
// c.MaxIndexAge. On network failure a cached copy is returned with
// Stale=true; without a cache the failure is fatal.
func (c *Client) FetchIndex(reg Registry) (*IndexResult, error) {
	if err := validateIndexURL(reg.URL); err != nil {
		return nil, err
	}
	u, _ := url.Parse(reg.URL)
	if u.Scheme == "file" {
		data, err := os.ReadFile(u.Path)
		if err != nil {
			return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
		}
		idx, err := ParseIndex(data)
		if err != nil {
			return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
		}
		return &IndexResult{Index: idx}, nil
	}

	dir, err := c.cacheDir()
	if err != nil {
		return nil, err
	}
	cachePath := filepath.Join(dir, reg.Name, "index.yaml")
	etagPath := filepath.Join(dir, reg.Name, "etag")

	cached, cacheAge := readCache(cachePath)
	if cached != nil && c.MaxIndexAge > 0 && cacheAge < c.MaxIndexAge {
		idx, err := ParseIndex(cached)
		if err == nil {
			return &IndexResult{Index: idx, Age: cacheAge}, nil
		}
		// A corrupt cache falls through to a fresh fetch.
	}

	req, err := http.NewRequest(http.MethodGet, reg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
	}
	if etag, err := os.ReadFile(etagPath); err == nil && cached != nil {
		req.Header.Set("If-None-Match", string(etag))
	}

	resp, err := httpDo(req)
	if err != nil {
		return staleFallback(reg, cached, cacheAge, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified && cached != nil:
		idx, err := ParseIndex(cached)
		if err != nil {
			return nil, fmt.Errorf("registry %q: cached index: %w", reg.Name, err)
		}
		now := time.Now()
		_ = os.Chtimes(cachePath, now, now)
		return &IndexResult{Index: idx}, nil

	case resp.StatusCode == http.StatusOK:
		data, err := readLimited(resp.Body, maxIndexBytes)
		if err != nil {
			return staleFallback(reg, cached, cacheAge, err)
		}
		idx, err := ParseIndex(data)
		if err != nil {
			// A malformed index is a registry bug, not a transport blip:
			// do not overwrite a previously good cache with it.
			return staleFallback(reg, cached, cacheAge, err)
		}
		if err := writeCache(cachePath, etagPath, data, resp.Header.Get("Etag")); err != nil {
			return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
		}
		return &IndexResult{Index: idx}, nil

	default:
		return staleFallback(reg, cached, cacheAge,
			fmt.Errorf("unexpected HTTP status %s", resp.Status))
	}
}

func staleFallback(reg Registry, cached []byte, age time.Duration, cause error) (*IndexResult, error) {
	if cached == nil {
		return nil, fmt.Errorf("registry %q: fetch index: %w (and no cached copy exists)", reg.Name, cause)
	}
	idx, err := ParseIndex(cached)
	if err != nil {
		return nil, fmt.Errorf("registry %q: fetch index: %w (and the cached copy is unreadable: %v)", reg.Name, cause, err)
	}
	return &IndexResult{Index: idx, Stale: true, Age: age}, nil
}

func readCache(path string) ([]byte, time.Duration) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0
	}
	return data, time.Since(st.ModTime())
}

func writeCache(cachePath, etagPath string, data []byte, etag string) error {
	dir := filepath.Dir(cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".index-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), cachePath); err != nil {
		return err
	}
	if etag != "" {
		return os.WriteFile(etagPath, []byte(etag), 0o644)
	}
	// No validator from the server: drop any old ETag so the next
	// revalidation is an unconditional refetch rather than a false 304.
	if err := os.Remove(etagPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds the %d byte limit", limit)
	}
	return data, nil
}

// FetchTarball downloads a recipe tarball into a temporary file, verifying
// its sha256 digest before returning. The caller removes the returned path.
func (c *Client) FetchTarball(reg Registry, name string, entry IndexEntry) (string, error) {
	if !digestRE.MatchString(entry.Digest) {
		return "", fmt.Errorf("%s@%s: index digest %q is not sha256:<64 hex>", name, entry.Version, entry.Digest)
	}
	if len(entry.URLs) == 0 {
		return "", fmt.Errorf("%s@%s: index entry has no download URL", name, entry.Version)
	}
	raw := entry.URLs[0]
	if err := validateIndexURL(raw); err != nil {
		return "", fmt.Errorf("%s@%s: %w", name, entry.Version, err)
	}
	u, _ := url.Parse(raw)

	var body io.ReadCloser
	if u.Scheme == "file" {
		f, err := os.Open(u.Path)
		if err != nil {
			return "", fmt.Errorf("%s@%s: %w", name, entry.Version, err)
		}
		body = f
	} else {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			return "", fmt.Errorf("%s@%s: %w", name, entry.Version, err)
		}
		resp, err := httpDo(req)
		if err != nil {
			return "", fmt.Errorf("%s@%s: download: %w", name, entry.Version, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("%s@%s: download: unexpected HTTP status %s", name, entry.Version, resp.Status)
		}
		body = resp.Body
	}
	defer body.Close()

	tmp, err := os.CreateTemp("", "vaka-recipe-*.tar.gz")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(body, maxTarballBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxTarballBytes {
		err = fmt.Errorf("tarball exceeds the %d byte limit", maxTarballBytes)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("%s@%s: download: %w", name, entry.Version, err)
	}

	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != entry.Digest {
		os.Remove(tmp.Name())
		return "", fmt.Errorf(
			"%s@%s: digest mismatch: index promises %s but the download is %s — refusing the artifact",
			name, entry.Version, entry.Digest, got)
	}
	return tmp.Name(), nil
}
