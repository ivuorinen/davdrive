#!/bin/sh
# Writes build/THIRD_PARTY_NOTICES.txt: the licence of every package linked into
# davdrive, as MIT, BSD and Apache-2.0 require for binary distribution. Run by
# GoReleaser's before hook for releases and by `mise run notices` locally.
set -eu

out=build/THIRD_PARTY_NOTICES.txt
mkdir -p "$(dirname "$out")"
# go-licenses is pinned here rather than in mise.toml: the release job has Go
# but not mise, and `go run pkg@version` needs no install step.
#
# Licence texts are copied verbatim. One of them, go-nfs-client's
# LICENSE_BSD-2.txt, is Windows-1252 with CRLF line endings upstream, so the
# output is not pure UTF-8; re-encoding a mixed file here would risk corrupting
# the rest of it.
go run github.com/google/go-licenses/v2@v2.0.1 report . \
  --ignore github.com/ivuorinen/davdrive \
  --template tools/notices.tpl >"$out"
echo "wrote $out"
