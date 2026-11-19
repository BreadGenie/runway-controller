#!/bin/bash
# Requires GNU tar, Go at the go.mod version, and gzip. Output defaults to cwd.
set -euo pipefail
umask 022
export LC_ALL=C
repo=$(cd "$(dirname "$0")/.." && pwd)
out=${1:-$PWD/runway-cache.tar.gz}
out=$(realpath -m "$out")
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$repo"
mkdir -p "$work/runway-cache/bin" "$work/runway-cache/systemd"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOFLAGS='' go build -trimpath -buildvcs=false -ldflags=-buildid= -o "$work/runway-cache/bin/runway-controller" .
install -m 0644 deploy/regional-cache/systemd/* "$work/runway-cache/systemd/"
git rev-parse HEAD > "$work/runway-cache/SOURCE_COMMIT"
(cd "$work/runway-cache"; find . -type f -printf '%P\n' | LC_ALL=C sort | xargs sha256sum) > "$work/SHA256SUMS"
mv "$work/SHA256SUMS" "$work/runway-cache/SHA256SUMS"
# Archive only the release files.
(cd "$work"; find runway-cache -type f | LC_ALL=C sort | tar --no-recursion --format=ustar --mtime=@0 --owner=0 --group=0 --numeric-owner -cf - -T -) | gzip -n > "$out"
sha256sum "$out"
