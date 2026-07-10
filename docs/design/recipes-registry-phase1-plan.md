# Phase 1 Implementation Plan: Recipes Registry

Companion to [recipes-registry.md](recipes-registry.md) (the accepted design;
normative on any conflict). Scope: read-only registry consumption in vaka —
`vaka get`, `vaka search`, `vaka recipes list|info`, `vaka registry list` —
against the live official registry
(`https://infrasecture.github.io/vaka-registry/index.yaml`, serving
`codex@0.1.0`). Out of scope (Phase 2+): `registry add|remove|refresh`,
`minVakaVersion` *enforcement*, signed indexes, digest pinning,
`recipes verify`, recipe-name shell completion.

Branch: `feature/vaka-registry` (contains the compose-namespace restructure it
depends on). Estimated shape: six commits, each independently reviewable and
tested.

## Decisions pinned here (implementation-level; design stays normative)

| Topic | Decision |
|---|---|
| Config path | `os.UserConfigDir()/vaka/registries.yaml`; absent file ≡ built-in default (`official` → the Pages URL). File is hand-editable for multi-registry in Phase 1. |
| Cache path | `os.UserCacheDir()/vaka/registries/<name>/index.yaml` + sidecar `etag` |
| Index revalidation | `get` always revalidates (ETag; 304 is one cheap round-trip). `search`/`list`/`info` revalidate when the cache is older than 15 minutes. Network failure → use stale cache with a loud age warning; no cache → hard error. |
| URL schemes | `https://` and `file://` (testing, air-gap). Plain `http://` rejected. |
| Strictness | YAML vaka *owns* (registries config, lock, journal) decodes strictly (`yaml.v3` `KnownFields`). The *index* tolerates unknown fields — registries may be newer than the client. `recipe.yaml` is not parsed by vaka in Phase 1 (the index carries its metadata). |
| Ref grammar | `[registry/]name[@version]`; registry and name `[a-z0-9-]+`, version strict `X.Y.Z` exact (constraints are rejected with a "Phase 1 supports exact versions" error). |
| Extraction limits | 50 MiB unpacked total, 2 000 entries, 20 MiB per file. Constants in one place; tarball must contain exactly one top-level directory named `<recipe>`, which is stripped on extraction. |
| minVakaVersion | Recorded and displayed, not enforced (Phase 2, with the `version=dev` skip). |
| New dependency | `github.com/Masterminds/semver/v3` only. Everything else is stdlib (`os.Root`, `archive/tar`, `compress/gzip`, `net/http`, `crypto/sha256`) + existing deps. flock via existing `golang.org/x/sys/unix`. |

## Commit 1 — `pkg/registry`: config, index model, fetch/cache, resolution

New package; no cmd wiring yet.

- `config.go`: `RegistriesConfig` (`kind: RegistriesConfig`), `Registry{Name, URL}`.
  `LoadConfig()` returns the built-in default when the file is absent; validates
  name charset/uniqueness and URL schemes.
- `index.go`: `RegistryIndex` (`kind: RegistryIndex`), `IndexEntry{Version,
  Description, Tags, Created, Digest, URLs, MinVakaVersion, Env, Policy}` —
  lenient decode, but `apiVersion`/`kind` must match.
- `fetch.go`: index fetch with ETag revalidation and cache write; tarball fetch
  streaming to a temp file while hashing sha256 (verify-before-use). One test
  seam in the style of `execDockerComposeFn`:
  `var httpDo = (*http.Client).Do` (or a tiny `fetcher` func var) so every
  command is testable offline.
- `resolve.go`: `Resolve(cfg, indexes, ref)` → `(registry, name, entry)`.
  Unqualified names resolve **only if unique across all configured
  registries**; ambiguity errors list the qualified candidates. Version
  selection: exact match or highest (Masterminds sort).
- Tests: fixture indexes (incl. one served via `file://` and one via
  `httptest` exercising ETag/304), uniqueness conflicts, unknown-registry /
  unknown-recipe / unknown-version errors, scheme rejection, stale-cache
  warning path.

## Commit 2 — `pkg/recipe`: safe I/O primitive + hardened extractor + digest

- `saferoot.go`: the shared confinement layer over `os.Root` — open/create
  (`O_EXCL`), rename, remove, lstat, readlink, walk, all beneath-root with
  symlinks never followed; `syncFile`/`syncDir` helpers behind a small
  interface so transaction tests can record call order.
- `extract.go`: tar.gz → directory via saferoot. Rules from design §7: reject
  `..`/absolute paths, hardlinks, device/special files, absolute or escaping
  symlink targets, entries under `.vaka-*`; enforce the three limits; strip
  the single required `<name>/` top-level dir; preserve only the exec bit.
- `hash.go`: lock entry state of a path — `sha256:<hex>`, `sha256:<hex>+x`,
  or `link:<target>` — one function used by install, pre-check, and journal.
- Tests: adversarial archives (traversal, absolute path, escaping link,
  hardlink, device node, decompression bomb, `.vaka-*` entry, zero/two
  top-level dirs), exec-bit and symlink round-trip, digest mismatch aborts
  before extraction.

## Commit 3 — `pkg/recipe`: lock/journal models + fresh install

- `lock.go`: `RecipeLock` (`kind: RecipeLock`; registry, name, version,
  digest, fetched, `Files map[string]string`, `Deviations []Deviation`)
  strict-decoded; `Deviation{Path, Kind}` with kinds `skipped-collision`,
  `kept-user-copy`.
- `journal.go`: `RecipeLockPending` (`kind: RecipeLockPending`;
  `Target{Registry, Name, Version, Digest, URLs}`,
  `Plan map[path]{Accepted []string, Final string}` with `Final: absent`
  sentinel, embedded `FinalLock RecipeLock`).
- `install.go`: fresh install per design — target must not exist (any existing
  path, incl. empty dir, refused; no override); extract + write lock into a
  dot-prefixed staging sibling in the target's parent (same filesystem), then
  one atomic rename; failure removes the staging sibling.
- Tests: happy path (lock records every file incl. `link:` and `+x` entries),
  refusal for file/dir/empty-dir/symlink targets, crash-simulation leaves
  either nothing or a complete tree, no state without a lock.

## Commit 4 — `pkg/recipe`: the update transaction

The §6 engine, structured as **explicit named steps** with an
`afterStep func(step) error` test hook so interruption tests can abort at
every boundary rather than relying on process-kill heroics.

- `update.go`:
  1. flock `.vaka-recipe.update.lock` (`LOCK_EX|LOCK_NB`; busy → fail fast).
  2. Stale-journal cleanup (journal target == committed lock → remove);
     staging removal whenever no apply is in progress.
  3. Pre-check: decision matrix with accepted-state union (committed lock ∪
     dangling journal accepted ∪ dangling journal target ∪ incoming version);
     the single blocking case; deviations computed here.
  4. Journal write (staging → fsync → rename → dir fsync) inheriting the
     accepted-state chain.
  5. Apply (staged per-file write-then-rename, deletions, batched dir fsyncs).
  6. Commit (embedded finalLock staged + renamed over `.vaka-recipe.lock`).
  7. Cleanup (journal unlink, staging removal, dir fsync).
- Tests (§9 of the design, in full): every matrix row; abort at every step
  boundary then re-run converges — same and newer target, including the
  A→B(interrupted)→C chain; genuine user edits (content, chmod, repoint,
  retype) still block; deviations recorded and dropped after resolution;
  stale journal removed; concurrent updater refused; fsync/rename order
  asserted via the recording sync interface.

## Commit 5 — local risk lint + policy summary

- `lint.go` in `pkg/recipe`: the same flag set as the registry CI
  (`privileged`, `cap-add-broad`, `docker-socket-mount`, `host-network`,
  `host-pid`, `host-ipc`, `broad-bind-mount`, `egress-default-accept`,
  `no-policy-for-service`, `disables-vaka-init`) computed from the recipe's
  actual files: compose project loaded with `compose-go` (same default
  discovery as the compose path, but self-contained — `cmd/vaka`'s
  `resolveComposeInput` is package `main` and stays untouched), `vaka.yaml`
  via `pkg/policy`.
- Index `policy` block comparison per design §4: absent → fine; different →
  loud "registry metadata is stale/inaccurate for <name>@<version>" warning,
  never a failure. Local result is what gets printed.
- Tests: fixture recipes per flag; mismatch-warns-not-fails; parity check
  against `scripts/validate_recipe.py` semantics via shared fixtures.

## Commit 6 — CLI wiring + docs

- `cmd/vaka/get.go`: `vaka get <[registry/]name>[@version] [dir]` — resolve →
  fetch+verify → install or update → print: `name@version`, digest, required
  env vars currently unset (checked against the process env; `.env` is never
  read), local policy summary + risk flags, deviations, and next steps.
- `cmd/vaka/search.go`: substring match over name/description/tags of all
  cached indexes; columns NAME (qualified when >1 registry), VERSION (latest),
  DESCRIPTION, RISK (flag count from the index, advisory).
- `cmd/vaka/recipes.go`: `recipes list` (catalog across registries; explicitly
  no filesystem scanning), `recipes info <ref>` (description, tags, versions,
  env table, policy summary, minVakaVersion — all from the index, labeled
  advisory).
- `cmd/vaka/registry.go`: `registry list` (name, URL, cache age/staleness).
- `cmd/vaka/root.go`: new `Recipe Commands:` group; all four are plain cobra
  commands (flag parsing on) — no pre-parser changes; names are already
  collision-free under the compose namespace.
- Render-verb deviation notice: `runComposeCLI` checks for
  `.vaka-recipe.lock` in the resolved project directory before `verbRender`
  dispatch and prints the one-line notice when `deviations` is non-empty.
- Docs: `docs/cli.md` (four new command sections + `registries.yaml`
  reference), README one-liner (`vaka get codex` in the quickstart path),
  `docs/quickstart.md` pointer. Design doc amended only where implementation
  forced a deviation (each amendment called out in the commit message).

## Verification

- `go build ./... && go test ./...` green per commit; extractor and update
  tests run with `-race`.
- Offline integration test: a `file://` registry built in `t.TempDir()` from
  the same packaging recipe as CI (deterministic tar of a fixture recipe),
  exercising get → modify → get (reject) → resolve → get (converge).
- Live end-to-end (manual, against the real registry):
  `vaka registry list`, `vaka search codex`, `vaka recipes info codex`,
  `vaka get codex` into a temp dir (digest must match
  `codex-0.1.0`'s release digest), `vaka get codex` again (no-op update),
  edit `compose.yaml` → `vaka get codex` (single-case rejection), lock file
  inspection (symlink `link:` entry, `myCodex` `+x` entry), `cd codex && vaka
  up` deviation-free notice absence, and `vaka up` on a dir with a fabricated
  deviation (notice appears).

## Risks / watch items

- `os.Root` API coverage: confirm rename/remove/readlink coverage on Go 1.25
  in commit 2; if a primitive is missing, fall back to `unix.Openat2`-based
  helpers inside `saferoot.go` without changing its interface.
- ETag behavior of GitHub Pages is well-defined, but the fetcher must treat
  *any* 2xx without ETag as cacheable-but-always-refetched (correctness over
  optimization).
- The codex tarball wraps content in `codex/` (CI packaging); the extractor's
  strip-one-component rule is pinned to that — the offline fixture registry
  must package identically so drift is caught by tests, not users.
