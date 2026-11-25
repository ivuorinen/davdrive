# davdrive

Mounts a WebDAV share, such as an [Infomaniak kDrive](https://www.infomaniak.com/en/ksuite/kdrive), as a local
drive on macOS and Linux. It needs no FUSE or kernel extension.

`davdrive` serves the share as NFSv4 on `127.0.0.1` and mounts that with the operating system's own NFS client.
Behind the NFS server sits [rclone](https://rclone.org)'s VFS and WebDAV backend, used as a Go library, so caching,
write-back and retries are rclone's.

Why not plain `rclone nfsmount`?

- `rclone nfsmount` speaks NFSv3. Its server logs `failing create to indicate lack of support for 'exclusive' mode`
  on every `O_EXCL` create, and macOS stores extended attributes on NFSv3 as AppleDouble `._*` files, which rclone
  uploads.
- `davdrive` speaks NFSv4, where exclusive creates work.
- It keeps `._*` and `.DS_Store` files in memory and never uploads them. Extended attributes work while the share
  is mounted and are dropped on unmount; kDrive cannot store them anyway. Sidecar files an earlier client uploaded
  are hidden.

## Install

Download the binary for your platform (`davdrive_darwin-arm64`, `davdrive_darwin-amd64`, `davdrive_linux-amd64`,
`davdrive_linux-arm64`) from the [releases](https://github.com/ivuorinen/davdrive/releases), make it executable and
put it on your `PATH`:

```sh
install -m 0755 davdrive_darwin-arm64 ~/.local/bin/davdrive
```

Each release's checksums file is signed with [cosign](https://docs.sigstore.dev/) keyless signing from the release
workflow. To verify a download, fetch `davdrive_<version>_checksums.txt` and its `.bundle` next to the binary:

```sh
cosign verify-blob \
  --bundle davdrive_<version>_checksums.txt.bundle \
  --certificate-identity-regexp '^https://github.com/ivuorinen/davdrive/\.github/workflows/release\.yml@refs/tags/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  davdrive_<version>_checksums.txt
shasum -a 256 --ignore-missing -c davdrive_<version>_checksums.txt
```

The release also carries `LICENSE` and `THIRD_PARTY_NOTICES.txt`, the licences of the packages linked into the
binary.

To build from source instead (Go 1.27):

```sh
go build -o ~/.local/bin/davdrive .
```

`go install …@latest` does not work yet: `go.mod` replaces go-nfs with a patched copy (see
[`third_party/go-nfs-UPSTREAM.md`](third_party/go-nfs-UPSTREAM.md)), and `go install` refuses replace directives.

Platform requirements:

- **macOS**: nothing extra. `mount_nfs` is built in, and a user can mount on a directory they own without root.
- **Linux** (Ubuntu, Pop!_OS): `sudo apt install nfs-common`. Linux allows NFS mounts only for root, so `mount`
  and `unmount` run `mount`/`umount` through `sudo`, which prompts for your password. The server itself runs as
  you.

## Configure

`~/.config/davdrive/config.toml` (or `$XDG_CONFIG_HOME/davdrive/config.toml`; override with `--config` or
`$DAVDRIVE_CONFIG`):

```toml
[mounts.kdrive]
# https://<kDrive ID>.connect.kdrive.infomaniak.com; the ID is the number in the web app's URL.
url = "https://123456.connect.kdrive.infomaniak.com"
user = "you@example.com"
# Prints the password. With two-factor authentication on, use an application password.
password_command = "security find-generic-password -s kdrive -a you@example.com -w"  # macOS Keychain
# password_command = "secret-tool lookup service kdrive"                             # GNOME Keyring
mountpoint = "~/kDrive"
# mount_options = []  # extra -o options for mount_nfs (macOS) or mount -t nfs (Linux)

# Optional rclone VFS options, by their rclone config names (rclone help flags vfs).
[mounts.kdrive.vfs]
vfs_write_back = "30s"   # wait longer before uploading: rapid saves become fewer kDrive versions
# vfs_cache_max_size = "2G"
# vfs_cache_max_age = "1h"
# dir_cache_time = "5m"
```

The password comes from `$DAVDRIVE_PASSWORD` first, then `password_command`, then a prompt when run in a
terminal. It reaches the background server through a pipe, never through arguments or the environment.

`url` must use `https://`. `http://` is accepted only for a server on this machine (`localhost`, `127.0.0.1`,
`::1`), because Basic auth over http sends the password in cleartext. `mountpoint` must be absolute or start with
`~/`.

rclone's own environment variables apply to the server too: `RCLONE_WEBDAV_*` for the WebDAV backend, and for each
VFS option `RCLONE_` plus its name in capitals (`RCLONE_VFS_WRITE_BACK`, `RCLONE_DIR_CACHE_TIME`,
`RCLONE_READ_ONLY`). The config file takes precedence for `url`, `user`, the password and the values in
`[mounts.<name>.vfs]`, and `davdrive` always sets the cache mode, maximum age and maximum size; everything else
can come from the environment. A leftover `RCLONE_READ_ONLY=true`, for example, gives a read-only mount.
Bandwidth limits (`RCLONE_BWLIMIT`) do nothing: rclone starts its limiter in its own command, which `davdrive`
does not run.

Several `[mounts.<name>]` tables may be defined. Commands take the name as an argument, which can be left out when
only one mount is configured.

## Use

Output from a test mount (`test`, served from a local WebDAV server), with one file still waiting to upload:

```console
$ davdrive mount
Mounted test at /tmp/davdrive-test/mnt (log: /tmp/davdrive-test/state/davdrive/test.log)

$ davdrive status
test: mounted at /tmp/davdrive-test/mnt
  remote:  http://127.0.0.1:18080
  server:  pid 30065, port 59129, up 0s
  uploads: 0 in progress, 1 queued, 0 failed files
  cache:   0 B in 1 files, limit 2 GiB
  log:     /tmp/davdrive-test/state/davdrive/test.log

$ davdrive unmount
Waiting for 1 upload(s) to finish...
Unmounted test
```

| Command                      | What it does                                                                  |
|------------------------------|-------------------------------------------------------------------------------|
| `mount [name]`               | Starts the server in the background and mounts it; no-op if already mounted.  |
| `mount -f [name]`            | Runs in the foreground and logs to stderr; Ctrl-C unmounts.                   |
| `unmount [name]`             | Unmounts, then waits until pending uploads finish and the server exits.       |
| `unmount --force [name]`     | Unmounts even while files are open; also clears a mount whose server died.    |
| `status [name...]`           | Shows mount state, server, upload queue and cache use for each share.         |
| `completion bash\|zsh\|fish` | Prints a shell completion script.                                             |

`-v` logs more; `-vv` logs debug output, including each NFS operation that fails.

Ctrl-C while `unmount` waits stops the waiting, not the uploads: the server finishes them in the background. If the
server is killed with uploads pending, the changed files stay in the cache and upload on the next `mount`.

## How caching works

`davdrive` uses rclone's `full` VFS cache mode:

- **Reads** download only the parts read, read ahead while reading sequentially, and keep them in the cache.
- **Writes** go to the cache and upload `vfs_write_back` (default 5 s) after the file was last closed.
- **Limits**: data unused for `vfs_cache_max_age` (default 1 h) is evicted. When the cache grows past
  `vfs_cache_max_size` (default 2 GiB), the least recently used files are evicted, checked every
  `vfs_cache_poll_interval` (default 1 min). Open files and files waiting to upload are never evicted.

Full mode is needed for speed, not just writes. The NFS server opens and closes the file for every NFS `READ` and
`WRITE` call, and only the cache keeps the download stream open between calls.

Files (`$XDG_STATE_HOME` defaults to `~/.local/state`, `$XDG_CACHE_HOME` to `~/.cache`):

| Path                                    | Contents                                      |
|-----------------------------------------|-----------------------------------------------|
| `$XDG_STATE_HOME/davdrive/<name>.sock`  | Control socket the CLI talks to the server on |
| `$XDG_STATE_HOME/davdrive/<name>.lock`  | Held while a server runs; stops a second one  |
| `$XDG_STATE_HOME/davdrive/<name>.log`   | Server log, rotated at 10 MiB, 3 kept         |
| `$XDG_CACHE_HOME/davdrive/`             | rclone VFS cache                              |

## Limitations

- Changes made elsewhere (web app, phone) show up after `dir_cache_time` (default 5 min). WebDAV has no change
  notifications.
- Modes and owners are not stored: every file belongs to the mounting user, with modes from your umask (`0644` for
  files and `0755` for directories under the usual `022`). Set `umask` in `[mounts.<name>.vfs]` to override.
- Symlinks and hard links are not supported.
- An `O_EXCL` create is handled as a guarded create: it fails if the file exists, as it should. The server keeps no
  create verifier, though, so a lost reply followed by a retransmitted request fails with `EEXIST`. Over loopback
  TCP this does not happen in practice.
- Calls to one file are handled one at a time, across all NFS connections. Overlapping open-write-close cycles on
  one file lose writes in rclone's VFS cache, so `davdrive` serialises them. Different files are unaffected.
- kDrive keeps a version of the old content on every overwrite, and versions count against the quota. A longer
  `vfs_write_back` merges rapid saves into fewer uploads.
- The NFS server listens on a `127.0.0.1` port that every account on the machine can reach. It answers only calls
  carrying the mounting user's uid or root's, and on Linux only connections from reserved ports, which only root,
  and so only the kernel NFS client, can open. macOS lets any user bind those ports, so there a local program that
  speaks NFS itself and claims your uid gets in. On a Mac shared with untrusted accounts, unmount when not in use.
- Linux: mounting and unmounting need `sudo`. A server stopped by a signal (logout, `kill`) unmounts first; when
  that needs a `sudo` password it cannot ask for, the server keeps running and logs the command to run, rather than
  leave a hung mount behind. If the server dies, `unmount --force` clears the mount.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT, see [LICENSE](LICENSE). `third_party/go-nfs` is a patched copy of go-nfs under the Apache License 2.0
([its licence](third_party/go-nfs/LICENSE), [what was changed](third_party/go-nfs-UPSTREAM.md)).
