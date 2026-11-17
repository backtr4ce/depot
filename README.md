# depot

A Go workspace artifact manager inspired by Google's `gclient`. Dependencies are
declared in `DEPOT.yaml`, selected using conditions, and pinned by SHA-256.

```sh
go build -o depot .
./depot configure
./depot validate
./depot sync --jobs 4
```

## Commands

`configure` validates the root manifest and its existing includes, then saves
variables without downloading anything. Manifests inside artifact destinations
are deferred until those artifacts are synchronized. Repeat `--set NAME=VALUE` to set several
variables. Existing assignments are retained; subsequent invocations need not
repeat them. Assigning `true` or `false` produces a boolean; all other values are
strings. Manifest `vars` blocks supply defaults; `--set` overrides them throughout
the workspace. Only explicit overrides are saved, so editing a manifest default
takes effect on the next invocation. Undefined variables and attempts to override
`host_*` are errors.

`validate` is a dry run of `sync`. It resolves manifests and checks every
active artifact as `sync` would: checksums, required names, conflicting or
locally modified destinations. For each artifact it logs the planned action
(`up to date`, `install from cache` or `download`). It reports all problems
instead of stopping at the first, and exits non-zero if any are found. It never
downloads, extracts, installs or writes anything, including the cache directory,
lock, or saved state. `validate --set NAME=VALUE` applies to that run only.
Manifests shipped inside artifacts that are not installed yet can only be
validated by `sync`.

`sync` uses the saved variables to download and install artifacts. It also works
without configuring first if conditions use built-in variables or manifest defaults. Existing
artifact destinations must either be absent or have been installed by depot;
unmanaged files and locally modified artifacts are never overwritten. Move such
destinations aside explicitly to replace them. Inactive artifacts are not pruned.

All commands accept `--root DIR` (default: current directory), `--verbose`,
`--jobs` (1-256, a sanity cap on concurrent connections and open files), and `--timeout` (per HTTP download, default: 5 minutes).
Operational messages use structured `log/slog` logging on stderr. Cobra provides
argument validation and `--help`.

## Manifest

```yaml
vars:
  use_old_tool: false
  channel: stable

includes:
  - build

artifacts:
  - name: tool
    from: https://example.org/tool.tar.gz
    to: third_party/tool
    sha256: "<64-character SHA-256 from the publisher>"
    condition: "host_cpu == 'x64' and not use_old_tool"
    # archive: tar.gz  # Optional format hint for extensionless URLs.
    # strip_prefix: tool-1.2.3/linux  # Optional subpath of the archive to install.

required:
  - tool
```

All `includes` and artifact `to` paths are relative to the **workspace root**,
including those in nested or downloaded manifests. For example,
`to: third_party/tool` always installs at `<root>/third_party/tool`, never inside the
declaring manifest's directory. Missing parent directories of a
destination are created automatically at any depth, including for transitive
dependencies, so nothing needs to be prepared in advance. Absolute paths, paths escaping the root,
symlink paths, and `.git`/`.cache` destinations are
rejected. Only HTTP(S) sources are supported; HTTPS redirects cannot downgrade
to HTTP. No manifest commands or hooks are executed.

Includes are recursively searched for `DEPOT.yaml`. A missing include is allowed
so it can be populated later. After each artifact installation, depot recursively
discovers further manifests in that artifact. Repeated includes and include cycles
are deduplicated. Artifact names must be globally unique among active definitions;
mutually exclusive definitions of the same name are supported. Each discovered
batch must satisfy its `required` names before that batch is downloaded.

Different artifacts cannot share a destination. Nested destinations are allowed
for manifests shipped inside an ancestor artifact; the parent must not also ship
the child's destination. Changing a parent reinstalls its nested dependencies.
Archive contents are installed as-is unless `strip_prefix` is set.

Built-in variables:

| Variable                  | Value                                                         |
| ------------------------- | ------------------------------------------------------------- |
| `host_os`                 | GN OS name of the host, e.g. `linux`, `mac`, `win`; read-only |
| `host_cpu`                | GN CPU name of the host, e.g. `x64`, `x86`, `arm64`; read-only |
| `target_os`, `target_cpu` | Default to the host; overridable using `--set`                |

Host names follow GN and gclient: Go's `darwin`/`windows` become `mac`/`win`,
`amd64`/`386` become `x64`/`x86`, `mipsle`/`mips64le` become `mipsel`/`mips64el`,
and `ppc64le` becomes `ppc64`. Every other Go platform (`android`, `ios`,
`freebsd`, `riscv64`, `loong64`, `s390x`, ...) keeps its Go name.

Variable names are ASCII identifiers (`[A-Za-z_][A-Za-z0-9_]*`) and cannot be
`true`, `false`, `and`, `or` or `not`. All `host_*` names are read-only.

`vars` is a mapping of variable names to boolean or string defaults. Names
starting with `host_` or `target_` are reserved for built-ins and cannot be
declared there; override `target_os`/`target_cpu` with `--set`. Quote numeric
values to use them as strings, e.g. `version: "17"`. Defaults apply to the declaring
manifest and are inherited by manifests discovered through its includes or
artifacts. Scoping flows downwards only: an included manifest sees its includer's
variables, but variables it declares (or redefines) are invisible to its includer
and siblings. `--set` overrides are global and beat every `vars` default.
Precedence is saved `--set` overrides, current manifest defaults, inherited
defaults, then built-in target defaults. Host variables cannot be declared in
`vars`. A repeatedly included manifest is evaluated only once, using its first
discovery's inherited defaults.

Conditions support booleans, single/double-quoted strings, variable references,
`==`, `!=`, `not`, `and`, `or`, and parentheses. Precedence is comparison, `not`,
`and`, then `or`. Comparisons require matching types and logical operands must be
booleans. All references are validated, including in otherwise short-circuited
branches. There is no implicit environment-variable interpolation.
Parentheses can be nested, for example:
`not (use_old_tool or (target_os == 'win' and target_cpu != 'x64'))`.
Strings may escape only their own quote and backslash. Conditions are limited to
4096 bytes and 64 levels of nesting; errors report the byte offset of the problem.

## Archives and cache

Source formats are inferred from the URL path, ignoring query parameters.
Supported formats and `archive` hints are `none`, `zip`, `tar`, `tar.gz`/`tgz`,
`tar.bz2`/`tbz2`, `tar.xz`/`txz`, `gzip`/`gz`, and `xz`. Unknown extensions default
to raw files; use a hint when necessary. Tar and zip extract to directories;
standalone gzip and xz decompress to the exact destination filename. If `to` has
a recognized archive extension, the verified download is copied without extraction.
Any other file, such as `.txt`, `.mp4` or a binary, is installed byte-for-byte at `to`.

`strip_prefix` selects a slash-separated path inside an extracted tar or zip
archive; only that directory or single file is installed at `to`. Use it for
doubly nested archives (`strip_prefix: pkg-1.0/pkg`) or to take just a subtree
(`strip_prefix: pkg-1.0/bin`). It must be relative and stay inside the archive,
and it is rejected for raw files, standalone gzip/xz, and archives kept unextracted.
A prefix missing from the archive fails before anything is installed, listing the
archive's top-level entries. Changing only `strip_prefix` reinstalls the artifact.

Only `.cache/depot/` is owned by depot; other tools' cache directories are left
alone. It contains:

- `state.json`: saved variables and managed installation integrity records.
- `objects/<sha256>/source`: original verified downloads, compressed or raw.
- `objects/<sha256>/unpacked-*`: extracted trees and integrity metadata.
- `locks/` and `workspace.lock`: cancellable cross-process locks.

Cached downloads are hashed before reuse. Extracted trees have per-tree integrity
records, allowing offline restoration even when their original compressed download
has been removed. Corrupt extracted entries are rebuilt from a verified source;
only an absent or corrupt necessary source requires another download. Integrity
checks cover file contents, file permissions, and the root mode, not intermediate
directory permissions or empty-directory changes.

Downloads and extraction use temporary paths; only checksum-verified content can
be installed. Downloads run in a bounded goroutine pool, cancel on failure or
interrupt, and finish before installation of their batch begins. Installation
uses same-filesystem staging and backups, restoring the old destination on normal
commit failures. A whole recursive sync is not an all-or-nothing transaction;
earlier successful installations remain if a later manifest fails. A forced
process termination during replacement may require recovering the `previous`
directory under a `.depot-install-*` staging directory manually.

For safe extraction, links, device files, path traversal, and duplicate file
entries are rejected. Limits are 4 MiB per manifest, 4 GiB per download, 8 GiB
expanded per archive, and 100,000 archive entries. Archive executable bits are
preserved; ownership and special permission bits are not.

## Development

```sh
go test -race ./...
go vet ./...
```

Integration tests use local HTTP servers; tests do not fetch external artifacts.
