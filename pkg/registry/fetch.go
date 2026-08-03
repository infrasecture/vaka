package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
	client := &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: httpsOnlyRedirect,
	}
	return client.Do(req)
}

// httpsOnlyRedirect refuses any redirect to a non-https URL, so an https
// endpoint cannot be transparently downgraded to http mid-chain (GitHub
// Pages/Releases legitimately redirect, but always to https). It also caps
// the redirect chain, since setting CheckRedirect overrides the default cap.
func httpsOnlyRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-https URL %q (downgrade)", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}

// Client fetches registry indexes (with an ETag cache) and recipe tarballs.
type Client struct {
	// CacheDir holds per-registry index caches. Empty means
	// os.UserCacheDir()/vaka/registries.
	CacheDir string
	// MaxIndexAge is the published-index freshness window: a cached index
	// younger than this is served without a network round-trip. Zero always
	// revalidates published indexes. Git previews move only via RefreshIndex.
	MaxIndexAge time.Duration
}

// IndexResult is a fetched index plus its provenance.
type IndexResult struct {
	Index *Index
	// Revision is the immutable commit selected by a Git preview refresh. It is
	// empty for published index registries.
	Revision string
	// Stale is true when the network fetch failed and a previously cached
	// index was served instead; callers must surface this loudly.
	Stale bool
	// Age is the cache age when the index was served from cache.
	Age time.Duration
	// FallbackReason explains why a refresh served the previous cache. It is
	// populated only when Stale is true.
	FallbackReason string
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

// CachedIndex returns a registry's index from the local cache only, never
// touching the network. It is for latency-sensitive, best-effort callers like
// shell completion. ok is false when there is no usable cache.
func (c *Client) CachedIndex(reg Registry) (idx *Index, ok bool) {
	u, err := url.Parse(reg.URL)
	if !reg.IsGit() && err == nil && u.Scheme == "file" {
		if data, err := os.ReadFile(u.Path); err == nil {
			if idx, err := ParseIndex(data); err == nil {
				return idx, true
			}
		}
		return nil, false
	}
	dir, err := c.cacheDir()
	if err != nil {
		return nil, false
	}
	data, _ := cachedIndexBytes(dir, reg) // source-bound: nil if the registry was re-pointed
	if data == nil {
		return nil, false
	}
	idx, err = ParseIndex(data)
	if err != nil {
		return nil, false
	}
	return idx, true
}

// CacheAge reports the age of the registry's cached index, if one exists.
func (c *Client) CacheAge(reg Registry) (time.Duration, bool) {
	dir, err := c.cacheDir()
	if err != nil {
		return 0, false
	}
	// Source-bound: a cache written for a different URL/ref under the same name
	// is not this registry's cache.
	data, age := cachedIndexBytes(dir, reg)
	if data == nil {
		return 0, false
	}
	return age, true
}

// CacheRevision returns the immutable Git commit recorded by the current
// cache. It is empty/false for published index registries or a cache miss.
func (c *Client) CacheRevision(reg Registry) (string, bool) {
	if !reg.IsGit() {
		return "", false
	}
	dir, err := c.cacheDir()
	if err != nil {
		return "", false
	}
	env, _, ok := readIndexCache(indexCachePath(dir, reg.Name), reg.sourceIdentity())
	if !ok || env.Revision == "" {
		return "", false
	}
	return env.Revision, true
}

// FetchIndex returns the registry's index, revalidating the cache per
// c.MaxIndexAge. On network failure a cached copy is returned with
// Stale=true; without a cache the failure is fatal.
func (c *Client) FetchIndex(reg Registry) (*IndexResult, error) {
	if err := validateRegistry(reg); err != nil {
		return nil, err
	}
	if reg.IsGit() {
		return c.fetchGitIndexFromCache(reg)
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
	cachePath := indexCachePath(dir, reg.Name)

	env, cacheAge, ok := readIndexCache(cachePath, reg.sourceIdentity())
	var cached []byte
	if ok {
		cached = env.Index
	}
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
	if cached != nil && env.ETag != "" {
		req.Header.Set("If-None-Match", env.ETag)
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
		if err := writeIndexCache(cachePath, indexCache{
			Source: reg.sourceIdentity(),
			ETag:   resp.Header.Get("Etag"),
			Index:  data,
		}); err != nil {
			return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
		}
		return &IndexResult{Index: idx}, nil

	default:
		return staleFallback(reg, cached, cacheAge,
			fmt.Errorf("unexpected HTTP status %s", resp.Status))
	}
}

// RefreshIndex explicitly updates a registry. Published indexes retain their
// normal forced-revalidation behavior; Git previews alone resolve and package
// their mutable ref here.
func (c *Client) RefreshIndex(ctx context.Context, reg Registry) (*IndexResult, error) {
	if reg.IsGit() {
		return c.refreshGitIndex(ctx, reg)
	}
	return c.FetchIndex(reg)
}

func staleFallback(reg Registry, cached []byte, age time.Duration, cause error) (*IndexResult, error) {
	if cached == nil {
		return nil, fmt.Errorf("registry %q: fetch index: %w (and no cached copy exists)", reg.Name, cause)
	}
	idx, err := ParseIndex(cached)
	if err != nil {
		return nil, fmt.Errorf("registry %q: fetch index: %w (and the cached copy is unreadable: %v)", reg.Name, cause, err)
	}
	return &IndexResult{Index: idx, Stale: true, Age: age, FallbackReason: cause.Error()}, nil
}

// indexCache is the per-registry cache envelope: the complete source identity,
// its HTTP ETag or Git commit, and the raw index bytes, written as one
// atomically-renamed file. Binding all
// three together is what makes the cache trust-safe: a reader can never observe
// an index paired with a different registry's URL (a torn write or a concurrent
// URL change under the same name yields either the complete old envelope or the
// complete new one — never a mix), and a URL mismatch is treated as a miss.
// (yaml encodes Index as base64, so arbitrary bytes round-trip.)
type indexCache struct {
	Source string `yaml:"source,omitempty"`
	// URL reads caches written before source identities were introduced.
	URL      string `yaml:"url,omitempty"`
	ETag     string `yaml:"etag,omitempty"`
	Revision string `yaml:"revision,omitempty"`
	Index    []byte `yaml:"index"`
}

// indexCachePath returns the per-registry cache envelope path.
func indexCachePath(dir, name string) string {
	return filepath.Join(dir, name, "cache.yaml")
}

// cachedIndexBytes returns the cached index for reg only when the envelope was
// written for reg's exact URL; otherwise (no cache, corrupt, or a URL change)
// it reports a miss.
func cachedIndexBytes(dir string, reg Registry) ([]byte, time.Duration) {
	env, age, ok := readIndexCache(indexCachePath(dir, reg.Name), reg.sourceIdentity())
	if !ok {
		return nil, 0
	}
	return env.Index, age
}

// readIndexCache loads and source-checks the cache envelope. ok is false on any
// miss: absent, unreadable, malformed, empty, or written for another source.
func readIndexCache(path, wantSource string) (env indexCache, age time.Duration, ok bool) {
	st, err := os.Stat(path)
	if err != nil {
		return indexCache{}, 0, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return indexCache{}, 0, false
	}
	if err := yaml.Unmarshal(data, &env); err != nil {
		return indexCache{}, 0, false
	}
	storedSource := env.Source
	if storedSource == "" && env.URL != "" {
		storedSource = "index\x00" + env.URL
	}
	if storedSource != wantSource || len(env.Index) == 0 {
		return indexCache{}, 0, false // different registry origin, or empty
	}
	return env, time.Since(st.ModTime()), true
}

// writeIndexCache writes the envelope with a single atomic rename, so the
// on-disk cache is always a complete {url, etag, index} triple.
func writeIndexCache(path string, env indexCache) error {
	data, err := yaml.Marshal(env)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cache-*")
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
	return os.Rename(tmp.Name(), path)
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
// Every URL in the index entry is tried in order (mirrors); a URL that fails
// transport or digest verification falls through to the next.
func (c *Client) FetchTarball(reg Registry, name string, entry IndexEntry) (string, error) {
	if !digestRE.MatchString(entry.Digest) {
		return "", fmt.Errorf("%s@%s: index digest %q is not sha256:<64 hex>", name, entry.Version, entry.Digest)
	}
	if len(entry.URLs) == 0 {
		return "", fmt.Errorf("%s@%s: index entry has no download URL", name, entry.Version)
	}

	// A file:// artifact is only trusted from a file:// or Git preview registry. Otherwise a
	// remote (https) index could point vaka at local files, causing local
	// reads or resource exhaustion.
	regIsFile := reg.IsGit()
	if u, err := url.Parse(reg.URL); err == nil && u.Scheme == "file" {
		regIsFile = true
	}

	var failures []string
	for _, raw := range entry.URLs {
		path, err := c.fetchTarballURL(raw, entry.Digest, regIsFile)
		if err == nil {
			return path, nil
		}
		failures = append(failures, fmt.Sprintf("%s: %v", raw, err))
	}
	if len(failures) == 1 {
		return "", fmt.Errorf("%s@%s: download failed: %s", name, entry.Version, failures[0])
	}
	return "", fmt.Errorf("%s@%s: all %d download URLs failed:\n\t%s",
		name, entry.Version, len(entry.URLs), strings.Join(failures, "\n\t"))
}

// fetchTarballURL downloads and digest-verifies a single URL, returning the
// temp file path on success (removed on any failure). A file:// URL is
// permitted only when registryIsFile, so an https index cannot redirect to
// the local filesystem.
func (c *Client) fetchTarballURL(raw, wantDigest string, registryIsFile bool) (string, error) {
	if err := validateIndexURL(raw); err != nil {
		return "", err
	}
	u, _ := url.Parse(raw)

	var body io.ReadCloser
	if u.Scheme == "file" {
		if !registryIsFile {
			return "", fmt.Errorf("file:// artifact URL is only allowed from a file:// registry")
		}
		f, err := os.Open(u.Path)
		if err != nil {
			return "", err
		}
		body = f
	} else {
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			return "", err
		}
		resp, err := httpDo(req)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("unexpected HTTP status %s", resp.Status)
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
		return "", err
	}

	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != wantDigest {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("digest mismatch: index promises %s but the download is %s — refusing the artifact", wantDigest, got)
	}
	return tmp.Name(), nil
}
