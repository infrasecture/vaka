package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const indexV1 = `apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
recipes:
  demo:
  - version: 1.0.0
    digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
    urls: [https://example.com/demo-1.0.0.tar.gz]
`

const indexV2 = `apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
recipes:
  demo:
  - version: 2.0.0
    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
    urls: [https://example.com/demo-2.0.0.tar.gz]
`

// indexServer serves an index over TLS with ETag semantics and counts hits.
type indexServer struct {
	*httptest.Server
	body atomic.Value // string
	etag atomic.Value // string
	hits atomic.Int64
}

func newIndexServer(t *testing.T) *indexServer {
	t.Helper()
	s := &indexServer{}
	s.body.Store(indexV1)
	s.etag.Store(`"v1"`)
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		etag := s.etag.Load().(string)
		if r.Header.Get("If-None-Match") == etag && etag != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if etag != "" {
			w.Header().Set("Etag", etag)
		}
		fmt.Fprint(w, s.body.Load().(string))
	}))
	t.Cleanup(s.Close)

	old := httpDo
	httpDo = s.Client().Do
	t.Cleanup(func() { httpDo = old })
	return s
}

func (s *indexServer) registry() Registry {
	return Registry{Name: "testreg", URL: s.URL + "/index.yaml"}
}

func demoVersion(t *testing.T, res *IndexResult) string {
	t.Helper()
	entries := res.Index.Recipes["demo"]
	if len(entries) == 0 {
		t.Fatal("no demo entries in index")
	}
	return entries[0].Version
}

func TestFetchIndexCachingLifecycle(t *testing.T) {
	srv := newIndexServer(t)
	c := &Client{CacheDir: t.TempDir()}

	// 1. Cold fetch: full download, cache written.
	res, err := c.FetchIndex(srv.registry())
	if err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	if res.Stale || demoVersion(t, res) != "1.0.0" || srv.hits.Load() != 1 {
		t.Fatalf("cold fetch: stale=%v version=%s hits=%d", res.Stale, demoVersion(t, res), srv.hits.Load())
	}

	// 2. Fresh cache within MaxIndexAge: no network round-trip at all.
	c.MaxIndexAge = time.Hour
	res, err = c.FetchIndex(srv.registry())
	if err != nil {
		t.Fatalf("fresh-cache fetch: %v", err)
	}
	if srv.hits.Load() != 1 {
		t.Fatalf("fresh-cache fetch hit the network (hits=%d)", srv.hits.Load())
	}

	// 3. Forced revalidation: conditional request answered by 304.
	c.MaxIndexAge = 0
	res, err = c.FetchIndex(srv.registry())
	if err != nil {
		t.Fatalf("revalidate: %v", err)
	}
	if res.Stale || demoVersion(t, res) != "1.0.0" || srv.hits.Load() != 2 {
		t.Fatalf("revalidate: stale=%v version=%s hits=%d", res.Stale, demoVersion(t, res), srv.hits.Load())
	}

	// 4. Upstream changed: revalidation downloads the new content.
	srv.body.Store(indexV2)
	srv.etag.Store(`"v2"`)
	res, err = c.FetchIndex(srv.registry())
	if err != nil {
		t.Fatalf("changed fetch: %v", err)
	}
	if demoVersion(t, res) != "2.0.0" {
		t.Fatalf("changed fetch served %s, want 2.0.0", demoVersion(t, res))
	}

	// 5. Network gone: stale cache is served and flagged.
	srv.Close()
	res, err = c.FetchIndex(srv.registry())
	if err != nil {
		t.Fatalf("offline fetch: %v", err)
	}
	if !res.Stale || demoVersion(t, res) != "2.0.0" {
		t.Fatalf("offline fetch: stale=%v version=%s", res.Stale, demoVersion(t, res))
	}
}

func TestFetchIndexOfflineWithoutCacheFails(t *testing.T) {
	srv := newIndexServer(t)
	srv.Close()
	c := &Client{CacheDir: t.TempDir()}
	_, err := c.FetchIndex(srv.registry())
	if err == nil || !strings.Contains(err.Error(), "no cached copy exists") {
		t.Fatalf("err = %v, want no-cached-copy failure", err)
	}
}

func TestFetchIndexMalformedBodyKeepsGoodCache(t *testing.T) {
	srv := newIndexServer(t)
	c := &Client{CacheDir: t.TempDir()}
	if _, err := c.FetchIndex(srv.registry()); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}

	srv.body.Store("kind: Garbage\n")
	srv.etag.Store(`"v-garbage"`)
	res, err := c.FetchIndex(srv.registry())
	if err != nil {
		t.Fatalf("garbage fetch: %v", err)
	}
	if !res.Stale || demoVersion(t, res) != "1.0.0" {
		t.Fatalf("garbage fetch: stale=%v version=%s, want stale 1.0.0 from cache", res.Stale, demoVersion(t, res))
	}
}

func TestFetchIndexNoEtagAlwaysRefetches(t *testing.T) {
	srv := newIndexServer(t)
	srv.etag.Store("") // server offers no validator
	c := &Client{CacheDir: t.TempDir()}

	for i := 1; i <= 2; i++ {
		if _, err := c.FetchIndex(srv.registry()); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if srv.hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 full fetches without ETag", srv.hits.Load())
	}
}

// TestCacheNotReusedAcrossURLs verifies the disk cache is bound to the
// registry URL: re-pointing a registry name at a different URL must not serve
// the previous URL's cached index (which would let a name change silently
// return a stale, unrelated catalog).
func TestCacheNotReusedAcrossURLs(t *testing.T) {
	srv := newIndexServer(t)
	c := &Client{CacheDir: t.TempDir(), MaxIndexAge: time.Hour}

	// Prime the cache under name "testreg" pointing at the server.
	if _, err := c.FetchIndex(srv.registry()); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}
	if _, ok := c.CachedIndex(srv.registry()); !ok {
		t.Fatal("CachedIndex miss for the URL that was just cached")
	}

	// Same name, different URL: the cache belongs to the old URL and must not
	// be served.
	rebound := Registry{Name: "testreg", URL: srv.URL + "/other-index.yaml"}
	if _, ok := c.CachedIndex(rebound); ok {
		t.Fatal("CachedIndex served a cache written for a different URL")
	}
	if _, ok := c.CacheAge(rebound); ok {
		t.Fatal("CacheAge reported an age for a different URL's cache")
	}
	// The original URL still hits.
	if _, ok := c.CachedIndex(srv.registry()); !ok {
		t.Fatal("original URL cache lost after a rebound lookup")
	}
}

// TestCacheEnvelopeAtomicity verifies the cache is a single {url, etag, index}
// envelope: the URL binding lives inside the same atomically-written file (no
// separate sidecar that could diverge from the index), and an unparseable
// envelope is a clean miss rather than a mispaired index.
func TestCacheEnvelopeAtomicity(t *testing.T) {
	srv := newIndexServer(t)
	dir := t.TempDir()
	c := &Client{CacheDir: dir, MaxIndexAge: time.Hour}

	if _, err := c.FetchIndex(srv.registry()); err != nil {
		t.Fatalf("cold fetch: %v", err)
	}

	// The cache is exactly one file — no etag/url sidecars that a torn write
	// could leave inconsistent with the index.
	entries, err := os.ReadDir(filepath.Join(dir, "testreg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "cache.yaml" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("cache dir = %v, want exactly [cache.yaml]", names)
	}

	// The single file carries the URL binding internally.
	cachePath := indexCachePath(dir, "testreg")
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache envelope missing: %v", err)
	}
	if !strings.Contains(string(raw), srv.URL) {
		t.Fatal("envelope does not record its registry URL")
	}

	// A corrupt envelope reads as a miss, never as an index paired with a URL.
	if err := os.WriteFile(cachePath, []byte("\x00 not a valid envelope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.CachedIndex(srv.registry()); ok {
		t.Fatal("a corrupt cache envelope was accepted")
	}
}

func TestHTTPSOnlyRedirect(t *testing.T) {
	mkReq := func(scheme string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, scheme+"://example.test/x", nil)
		return r
	}
	if err := httpsOnlyRedirect(mkReq("https"), nil); err != nil {
		t.Fatalf("https redirect rejected: %v", err)
	}
	if err := httpsOnlyRedirect(mkReq("http"), nil); err == nil ||
		!strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("http redirect err = %v, want downgrade refusal", err)
	}
	via := make([]*http.Request, 10)
	if err := httpsOnlyRedirect(mkReq("https"), via); err == nil ||
		!strings.Contains(err.Error(), "10 redirects") {
		t.Fatalf("redirect cap err = %v", err)
	}
}

func TestFetchIndexFileScheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.yaml")
	if err := os.WriteFile(path, []byte(indexV1), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Client{CacheDir: t.TempDir()}
	res, err := c.FetchIndex(Registry{Name: "local", URL: "file://" + path})
	if err != nil {
		t.Fatalf("file fetch: %v", err)
	}
	if demoVersion(t, res) != "1.0.0" {
		t.Fatalf("file fetch served %s", demoVersion(t, res))
	}
}

func TestFetchTarball(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("not really a tarball, digest is what matters here")
	path := filepath.Join(dir, "demo-1.0.0.tar.gz")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	goodDigest := "sha256:" + hex.EncodeToString(sum[:])
	reg := Registry{Name: "local", URL: "file:///unused/index.yaml"}
	c := &Client{CacheDir: t.TempDir()}

	t.Run("digest match", func(t *testing.T) {
		got, err := c.FetchTarball(reg, "demo", IndexEntry{
			Version: "1.0.0", Digest: goodDigest, URLs: []string{"file://" + path},
		})
		if err != nil {
			t.Fatalf("FetchTarball: %v", err)
		}
		defer os.Remove(got)
		data, err := os.ReadFile(got)
		if err != nil || string(data) != string(payload) {
			t.Fatalf("fetched payload mismatch (err=%v)", err)
		}
	})

	t.Run("digest mismatch refused", func(t *testing.T) {
		_, err := c.FetchTarball(reg, "demo", IndexEntry{
			Version: "1.0.0",
			Digest:  "sha256:" + strings.Repeat("ab", 32),
			URLs:    []string{"file://" + path},
		})
		if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("err = %v, want digest mismatch", err)
		}
	})

	t.Run("malformed digest refused", func(t *testing.T) {
		_, err := c.FetchTarball(reg, "demo", IndexEntry{
			Version: "1.0.0", Digest: "md5:abc", URLs: []string{"file://" + path},
		})
		if err == nil || !strings.Contains(err.Error(), "sha256:<64 hex>") {
			t.Fatalf("err = %v, want digest format error", err)
		}
	})

	t.Run("missing URL refused", func(t *testing.T) {
		_, err := c.FetchTarball(reg, "demo", IndexEntry{Version: "1.0.0", Digest: goodDigest})
		if err == nil || !strings.Contains(err.Error(), "no download URL") {
			t.Fatalf("err = %v, want missing URL error", err)
		}
	})

	t.Run("size cap enforced", func(t *testing.T) {
		old := maxTarballBytes
		maxTarballBytes = int64(len(payload) - 1)
		defer func() { maxTarballBytes = old }()
		_, err := c.FetchTarball(reg, "demo", IndexEntry{
			Version: "1.0.0", Digest: goodDigest, URLs: []string{"file://" + path},
		})
		if err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("err = %v, want size cap error", err)
		}
	})

	t.Run("file:// artifact rejected from an https registry", func(t *testing.T) {
		httpsReg := Registry{Name: "r", URL: "https://recipes.example/index.yaml"}
		_, err := c.FetchTarball(httpsReg, "demo", IndexEntry{
			Version: "1.0.0", Digest: goodDigest, URLs: []string{"file://" + path},
		})
		if err == nil || !strings.Contains(err.Error(), "only allowed from a file://") {
			t.Fatalf("err = %v, want file:// scheme refusal", err)
		}
	})

	t.Run("falls through to a working mirror", func(t *testing.T) {
		got, err := c.FetchTarball(reg, "demo", IndexEntry{
			Version: "1.0.0", Digest: goodDigest,
			URLs: []string{
				"file:///vaka/does/not/exist.tar.gz", // dead primary
				"file://" + path,                     // healthy mirror
			},
		})
		if err != nil {
			t.Fatalf("FetchTarball with mirror: %v", err)
		}
		os.Remove(got)
	})

	t.Run("all URLs failing reports each", func(t *testing.T) {
		_, err := c.FetchTarball(reg, "demo", IndexEntry{
			Version: "1.0.0", Digest: goodDigest,
			URLs: []string{
				"file:///vaka/nope-a.tar.gz",
				"file:///vaka/nope-b.tar.gz",
			},
		})
		if err == nil || !strings.Contains(err.Error(), "all 2 download URLs failed") {
			t.Fatalf("err = %v, want all-failed summary", err)
		}
	})
}
