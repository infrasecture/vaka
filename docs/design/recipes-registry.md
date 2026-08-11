# Design: Recipe Registries

Status: implemented. This document describes the current Vaka client behavior;
the source and tests remain authoritative.

## 1. Concept

A recipe is a versioned Compose project that includes a Vaka policy. A registry
publishes recipe metadata and digest-addressed archives. Vaka can:

- discover recipes with `search` and `recipes`;
- install or update files with `get`;
- manage published and Git preview registries; and
- validate every downloaded recipe before exposing it as an installation.

Installing is separate from running. `vaka get` never starts Docker or executes
recipe code. The user reviews the installed files and follows that recipe's
README.

A digest proves which bytes were downloaded; it does not make arbitrary recipe
code trustworthy. Policy and risk summaries help review but are not a safety
certification.

## 2. Authoring And Distribution Formats

The authoring form is a top-level directory in a registry repository:

```text
demo/
├── recipe.yaml
├── compose.yaml
├── vaka.yaml
├── README.md
└── supporting files...
```

`recipe.yaml` and `vaka.yaml` are required. The Compose model must have one
discoverable base file; a normal override file is loaded when present. A README
is strongly recommended but is not required by the client validator.

The distribution form has two parts:

1. `index.yaml`, which lists recipe versions, digests, download URLs, and
   advisory metadata.
2. One gzip-compressed tar archive per published recipe version, containing a
   single top-level directory named after the recipe.

The index is mutable discovery metadata. The archive digest in the index binds
one selected version to its bytes.

## 3. Recipe Manifest

The following values are illustrative, not live registry data:

```yaml
apiVersion: recipes.vaka/v1alpha1
kind: Recipe
name: demo
version: 1.2.3
description: Example service with a restricted egress policy
homepage: https://example.com/demo
tags: [example, gateway]
minVakaVersion: 1.0.0
env:
  - name: DEMO_TOKEN
    required: false
    description: Optional provider token
```

Current validation rules include:

- `name` matches `[a-z0-9-]+` and must match the resolved recipe name;
- `version` is strict `X.Y.Z` SemVer and must match the resolved index entry;
- `description` is non-empty;
- `minVakaVersion`, when set, is strict `X.Y.Z` and is enforced against a
  release build of Vaka;
- every environment entry has a name; and
- unknown top-level fields are rejected.

`riskAcknowledgements` is accepted for registry-side lint metadata. The
reserved `provides` and `requires` fields are rejected because recipe
composability is not implemented.

The manifest inside the verified archive is authoritative for identity and
minimum-version enforcement. A mutable index cannot weaken those checks.

## 4. Registry Catalog

An index uses the same API version:

```yaml
apiVersion: recipes.vaka/v1alpha1
kind: RegistryIndex
generated: "2030-01-02T03:04:05Z"
recipes:
  demo:
    - version: 1.2.3
      description: Example service with a restricted egress policy
      tags: [example, gateway]
      created: "2030-01-02T03:00:00Z"
      digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      urls:
        - https://recipes.example.com/demo-1.2.3.tar.gz
      minVakaVersion: 1.0.0
      env: []
      policy:
        defaultActions: {app: reject}
        riskFlags: []
```

Index decoding tolerates unknown fields so a newer registry can add metadata
without breaking older clients. Identity, digest format, archive contents, and
the staged manifest are still checked during `get`.

The index's environment and policy blocks are advisory discovery data. After a
successful install or update, Vaka recomputes the policy summary and risk flags
from the local Compose and policy files for reporting.

Published indexes may retain only recent versions. Exact-version resolution is
limited to entries still present in the index even when an older archive remains
available elsewhere.

## 5. Configuration, Resolution, And Caching

When no user configuration exists, Vaka uses one built-in registry:

```yaml
apiVersion: recipes.vaka/v1alpha1
kind: RegistriesConfig
registries:
  - name: official
    url: https://infrasecture.github.io/vaka-registry/index.yaml
```

The missing file is not created on read. `registry add`, `registry add-git`, or
`registry remove` writes the strict configuration under the user configuration
directory; `registry list` shows the exact path. Each registry must have exactly
one source: a published index URL or a Git source.

Recipe references use:

```text
[registry/]name[@version]
```

- Registry and recipe names match `[a-z0-9-]+`.
- Versions are exact `X.Y.Z`; omitted means the highest indexed version.
- Constraints and ranges are rejected.
- An unqualified name must be unique across configured published registries.
- Git preview recipes never participate in unqualified resolution.

This uniqueness rule prevents a newly added registry from silently taking over
an existing unqualified name.

Published index caches use ETag revalidation. Browse commands accept a cache
younger than 15 minutes without a network request. They operate best-effort:
unavailable registries are omitted with a warning, and stale caches are accepted
with a warning. `get` revalidates published indexes. If an unqualified install
cannot obtain fresh enough state from every published registry, it fails rather
than weakening the uniqueness proof. A qualified reference needs only its
selected registry and may use a stale cache with a warning.

Shell completion reads cache only and never contacts a registry.

## 6. Install And Update Lifecycle

The supported forms are:

```text
vaka get <[registry/]name>[@version] [dir]
vaka get
vaka get @<version>
vaka get @<version> <dir>
```

### Fresh Install

The target's parent must exist and the target must not. Vaka will not adopt an
empty or populated ordinary directory.

The install sequence is:

1. Resolve the registry, recipe, and version.
2. Download the archive with a size limit and verify its SHA-256 digest.
3. Extract into a hidden sibling staging directory through a confined
   filesystem root.
4. Validate `recipe.yaml`, the controlled Compose model, and `vaka.yaml`.
5. Compute file states and the provenance lock.
6. Sync the staged tree and publish it with a no-replace atomic rename.

A normal failure removes staging. A partial target is never exposed.

After the file transaction commits, the CLI computes a local policy and risk
summary, compares it with the registry's advisory metadata, and reports required
environment variables declared by the catalog but absent from the current
process. A lint-reporting failure is a warning and does not roll back the
installation.

### Provenance Lock

Every managed installation contains `.vaka-recipe.lock`. It records:

- registry, recipe name, selected version, and artifact digest;
- the immutable source commit for a Git preview;
- a random installation-generation identifier and fetch time;
- hashes, executable bits, or symlink targets for shipped files; and
- any preserved deviations from the published recipe.

The lock is strict, bounded, and read through the confined recipe root. A
missing lock means the directory is not managed. A malformed or incompatible
lock blocks updates and directs the user to reinstall or restore it.

### Update

Bare `vaka get` reads identity from the current directory's lock. The
`@version` form selects an exact version; a directory argument allows the same
operation without changing directories. A named reference can also update an
existing target, but its registry and recipe identity must match the lock.

One updater holds `.vaka-recipe.update.lock` for the full transaction. Incoming
files are staged and validated before visible files change. The decision matrix
is:

| Local path state | Result |
| --- | --- |
| Tracked and unchanged | Replace or delete to match the new archive. |
| Tracked, locally deleted, still shipped | Restore it. |
| Tracked, locally edited, still shipped | Abort the complete update. |
| Tracked, locally edited, dropped upstream | Keep it and record `kept-user-copy`. |
| Untracked path identical to a new file | Adopt it as tracked recipe content. |
| Different untracked collision with a new file | Keep it and record `skipped-collision`. |
| Other untracked path | Leave it untouched. |

Before mutation, Vaka writes a durable `.vaka-recipe.lock.new` journal bound to
the installed lock generation and the final lock. A later `vaka get` either
finishes or cleans up an interrupted transaction. Stale or copied journals with
the wrong generation are refused.

After files commit, Vaka removes the journal and staging directory. It does not
start or recreate containers; applying an update is recipe-specific.

## 7. Security Boundaries

Registry handling is designed to make fetched bytes and local file ownership
predictable, not to certify third-party code.

Published transport and extraction controls include:

- HTTPS indexes and archives, with downgrade redirects rejected;
- `file://` indexes for explicit local use;
- index, archive, extracted-file, and extracted-total size limits;
- SHA-256 verification before extraction;
- one expected top-level recipe directory;
- rejection of absolute paths, traversal, escaping symlinks, hardlinks,
  devices, FIFOs, sockets, and reserved `.vaka-*` state; and
- no-replace publication of a fresh target.

Staged validation loads Compose with a controlled interpolation environment. It
does not honor ambient `COMPOSE_FILE`, the caller's environment, the top-level
recipe `.env`, or a parent project. Compose-go may load an included project's
in-tree `.env` when an `include:` has no `env_file`; all referenced Compose
files, configs, secrets, and env files must still remain inside the recipe tree.

Local lint reports service-qualified risks including:

- privileged mode, broad capabilities, host namespaces, or Docker socket use;
- broad bind mounts;
- default-accept egress or services without policy;
- disabling Vaka's runtime injection; and
- images not pinned by digest.

Risk flags are advisory and do not make unsafe recipe code safe. The user makes
the execution decision only after reviewing the installed files and running the
recipe's documented command.

## 8. Git Preview Registries

`registry add-git` is an opt-in development path. It accepts HTTPS, SSH,
scp-style SSH, and absolute `file://` repositories plus a required branch name
or full Git ref. Tags use `refs/tags/<tag>`. It rejects plain HTTP, `git://`,
custom remote helpers, embedded HTTPS credentials, and unsafe ref syntax.

The ref is resolved to one immutable commit before recipe discovery. Vaka reads
tree and blob objects directly, so hooks, submodules, worktree files,
`.gitattributes` export rules, and user archive configuration cannot affect the
artifact. Only tracked files in top-level recipe directories are packaged.

The generated catalog and artifacts are replaced atomically. A failed refresh
retains the previous complete snapshot. The lock records the source commit, and
preview recipe names always remain registry-qualified.

## 9. Deliberately Unsupported

The current client does not implement:

- SemVer ranges or constraints;
- digest references in the command grammar;
- a command that verifies an installation without fetching an update;
- recipe composition through `provides` and `requires`;
- OCI registry transport; or
- registry publication tooling in the Vaka CLI.

These are non-goals of the current command surface, not implied promises or
scheduled phases.
