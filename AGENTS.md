# AGENTS.md

`davdrive` mounts a WebDAV share (Infomaniak kDrive or any other) as a local drive on macOS and Linux: rclone's VFS
and WebDAV backend, used as a Go library, served as NFSv4 on `127.0.0.1` and mounted with the OS's own NFS client.
[README.md](README.md) describes the behaviour, [CONTRIBUTING.md](CONTRIBUTING.md) the layout, build, release and
smoke-test procedures. This file holds what an agent needs on top of them.

## Invariants

Do not weaken these without the user's explicit agreement:

- The NFS server listens on `127.0.0.1` only, and answers only calls carrying the mounting user's uid or root's
  (`internal/server/auth.go`).
- The password reaches the background server through a pipe, never through arguments or the environment.
- `url` must be `https://`; `http://` is allowed only for `localhost`, `127.0.0.1` and `::1`.
- The mount is unmounted before its server stops: a mount without a server hangs every process that touches it.
- `._*` and `.DS_Store` files stay in memory and are never uploaded.
- Calls to one file are serialised (`internal/fsys` path locks): overlapping open-write-close cycles on one file
  lose writes in rclone's VFS cache.

## The vendored go-nfs

`third_party/go-nfs` is upstream go-nfs plus the patch files beside it. Never edit its Go files by hand: change or
add a patch, rebuild the directory as [go-nfs-UPSTREAM.md](third_party/go-nfs-UPSTREAM.md) describes, and record
the patch there with its upstream pull request text.

## Commands

Tools are pinned in `mise.toml`; Go comes from `go.mod`. Use the mise tasks rather than calling `go` or
`golangci-lint` directly, so local runs match CI.

- `mise run all`: lint, race-enabled tests, govulncheck and build, the same gates as CI. Run it before calling a
  change done.
- `mise run test-short` for a quick loop. `mise run test` adds `TestConcurrentWrites` (about 20 s).
- `mise run lint` lints for both `GOOS=darwin` and `GOOS=linux`. A bare `golangci-lint run` rewrites files, since
  `.golangci.yml` sets `fix: true`.
- Markdown and whitespace: `markdownlint-cli2 README.md CONTRIBUTING.md AGENTS.md third_party/go-nfs-UPSTREAM.md`
  and `editorconfig-checker`. Markdown lines stay within 120 characters.

## Code

- Package dependencies: `main` (`main.go`, `cmd_*.go`) uses `internal/config`, `internal/platform` and
  `internal/server`; `internal/server` uses `internal/config` and `internal/fsys`; those three import no other
  internal package. Keep it that way.
- `internal/platform` has a darwin and a linux implementation, and each build sees only one. Change both, and lint
  both.
- A `//nolint` names the linter and gives a reason (`nolintlint` enforces it).
- Comments state what the code does and why, not the history of the change. A comment that disagrees with the code
  is a bug.

## Testing

- Unit tests cannot reach the kernel NFS client. Changes to `internal/server`, `internal/fsys`, `internal/platform`
  or the vendored go-nfs need the smoke test in CONTRIBUTING.md against a real mount, on Linux too (a privileged
  Ubuntu container) when the change can differ there.
- A bug fix comes with a test that fails without the fix.
- `TestConcurrentWrites` skips itself under `-race`, because it reaches a data race inside rclone
  ([#2](https://github.com/ivuorinen/davdrive/issues/2)); `mise run test-race` runs it again without `-race`.

## Commits

- Conventional Commits with a scope where one fits: `fix(go-nfs):`, `test(fsys):`, `docs(audit):`. The subject says
  what changed; the body is `- ` bullets saying why.
- One concern per commit. Stage by hunk when a file holds changes for more than one commit.
- `docs/audit/findings/` is the nitpicker findings store: change it through its tools, not by hand. `INDEX.md` is
  generated.
- Releases are SemVer tags made by `mise run release <patch|minor|major>`; see CONTRIBUTING.md.
