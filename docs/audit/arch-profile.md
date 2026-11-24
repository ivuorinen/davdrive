# Architecture Profile

Generated: 2026-11-24

Confidence: none — manual review required

## Detected Patterns

Detected: none. No catalogued pattern matches with at least Medium confidence.

- Layered / N-Tier was considered and rejected (Low): there are no `presentation/`, `service/` or `data/` layers,
  only a CLI in package `main` over four `internal/` packages.
- Hexagonal was considered and rejected (Low): `internal/fsys` adapts rclone's VFS to go-nfs's billy interface and
  `internal/platform` wraps OS mount tools, but nothing declares ports, and the packages are named by subsystem,
  not by role.

Observed structure, for reference:

- `main` (`main.go`, `cmd_*.go`) imports `internal/config`, `internal/platform`, `internal/server`.
- `internal/server` imports `internal/config` and `internal/fsys`.
- `internal/fsys`, `internal/config` and `internal/platform` import no other internal package.
- `third_party/go-nfs` is a vendored, patched upstream module behind a `replace` directive.

## Detected Combination

none

## Inferred Structural Rules

none

## Ambiguities & Contradictions

none
