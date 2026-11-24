# Patched copy of go-nfs

`third_party/go-nfs` is [willscott/go-nfs](https://github.com/willscott/go-nfs) at commit
`19d471546678bc0ab58ac2de14b043228a4d2468` (master, 2026-11-24, untagged: NFSv4 support is not in a release yet)
with the two patches below applied; the patch files sit in that directory too. Tests, examples, CI files,
upstream's own docs and the unused `helpers/memfs` package are left out. `go.mod` at the repository root points
there with a `replace` directive.

Delete `third_party/go-nfs`, this file and the `replace` once upstream releases a version with both fixes, then
require that version.

## Updating to a newer upstream commit

From the repository root, with upstream checked out at `<upstream>`:

```sh
rsync -a --delete --filter='P *.patch' \
  --exclude .git --exclude .github --exclude example --exclude '*_test.go' --exclude .golangci.yml \
  --exclude README.md --exclude CONTRIBUTING.md --exclude SECURITY.md --exclude helpers/memfs \
  <upstream>/ third_party/go-nfs/
rm -rf third_party/go-nfs/helpers/memfs
git apply --directory=third_party/go-nfs third_party/go-nfs/hidden-system-attrs.patch third_party/go-nfs/rpc-auth.patch
```

`--directory` is required: run inside `third_party/go-nfs`, `git apply` resolves the patch paths from the
repository root and silently skips them. Run upstream's tests on `<upstream>` with both patches applied before
copying.

Each modified file carries a `(Changed for davdrive; see ../go-nfs-UPSTREAM.md.)` comment, as the Apache-2.0
licence requires. Leave those lines out of upstream pull requests.

## `hidden-system-attrs.patch`: stop advertising `hidden` and `system`

`nfs4_attrs.go` lists `hidden` (25) and `system` (46) in `supported_attrs`, but `nfs4DecodeSetAttrs` accepts only
`size`, `mode`, `time_access_set` and `time_modify_set`. The macOS NFSv4 client sends `hidden` with every
`CREATE` and `OPEN`-create of a server that lists it, so every new file and directory fails with
`NFS4ERR_ATTRNOTSUPP` (10032), which applications see as `EIO`. The patch drops both attributes from
`nfs4SupportedAttrs` and from the `GETATTR` encoder.

Upstream pull request text:

> **nfs4: stop advertising hidden and system, which SETATTR rejects**
>
> `supported_attrs` lists `hidden` (25) and `system` (46), but `nfs4DecodeSetAttrs` only accepts `size`, `mode`,
> `time_access_set` and `time_modify_set`. The macOS client (macOS 27.2, `mount_nfs -o vers=4`) includes `hidden`
> in the `createattrs` of every `CREATE` and `OPEN` with `OPEN4_CREATE` once the server lists it, so creating any
> file or directory fails with `NFS4ERR_ATTRNOTSUPP` and the application gets `EIO`. Seen in the request as
> attribute mask `[25 33]` (`hidden`, `mode`).
>
> billy has no place to keep either flag, so this removes both from `supported_attrs` and from the `GETATTR`
> encoder; clients then stop sending them. With the change, creates, writes, renames and removes work from macOS,
> and `go test ./...` passes.

## `rpc-auth.patch`: an `Authorize` hook, and well-formed denied replies

`Server.Authorize`, when set, sees the procedure number and the credential (flavor and opaque body) of every call
before dispatch; a refusal is answered with `MSG_DENIED` / `AUTH_ERROR` (`AUTH_TOOWEAK`). davdrive uses it to
accept only the mounting user's AUTH_SYS uid (and root), because the server listens on a TCP port every local
account can reach.

Denials exposed two encoding bugs in every `MSG_DENIED` reply, found with a raw RPC client:

- `writeHeader` writes the internal `ResponseCode` (`ResponseCodeAuthError` = 7) where RFC 5531 puts
  `reject_stat`, whose only values are `RPC_MISMATCH` = 0 and `AUTH_ERROR` = 1. The patch writes `reject_stat`.
- `AuthError.MarshalBinary` and `RPCMismatchError.MarshalBinary` write little-endian, while XDR (RFC 4506) is
  big-endian, so `AUTH_TOOWEAK` (5) arrives as `0x05000000`. The patch encodes both big-endian, like
  `NFSStatusError` already does.

Upstream pull request text:

> **rpc: add Server.Authorize; fix the encoding of denied replies**
>
> A server reachable by more than its intended client has no way to refuse calls by credential: `Handler.Mount`
> covers only NFSv3 MOUNT, and NFSv4 never calls it. `Server.Authorize func(proc, flavor uint32, body []byte) bool`
> is checked in `conn.handle` before the handler runs; a false result answers `MSG_DENIED` / `AUTH_ERROR`
> (`AUTH_TOOWEAK`). Nil keeps today's behaviour.
>
> Denied replies were malformed in two ways. `writeHeader` wrote the `ResponseCode` value (7 for an auth error) as
> `reject_stat`, which RFC 5531 defines as `RPC_MISMATCH` = 0 or `AUTH_ERROR` = 1. And
> `AuthError.MarshalBinary` / `RPCMismatchError.MarshalBinary` used `binary.LittleEndian`, so clients decoded
> `AUTH_TOOWEAK` as `0x05000000` and version ranges byte-swapped. Both are fixed; `go test ./...` passes.
