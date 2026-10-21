#!/usr/bin/env bash
#
# kubectl-plugin-archive.sh builds kubectl-joulie for one platform and packs
# the archive Krew installs it from.
#
# Usage: scripts/kubectl-plugin-archive.sh <tag> <goos> <goarch> [outdir]
#
# Writes to outdir (default dist):
#   kubectl-joulie_<tag>_<goos>_<goarch>.tar.gz   the binary and LICENSE, the
#                                                 archive cmd/kubectl-joulie/krew.yaml
#                                                 names (Krew wants the license in it)
#   kubectl-joulie-<goos>-<goarch>[.exe]          the bare binary, for a direct download
# and prints the archive's path. The binary is kubectl-joulie.exe on Windows.
# The archive is reproducible: the same binary gives the same sha256.
set -euo pipefail
if [ $# -lt 3 ] || [ $# -gt 4 ]; then
	echo "usage: kubectl-plugin-archive.sh <tag> <goos> <goarch> [outdir]" >&2
	exit 2
fi
tag=$1 goos=$2 goarch=$3 out=${4:-dist}
root=$(cd "$(dirname "$0")/.." && pwd)
go=${GO:-go}

exe=""
[ "$goos" = windows ] && exe=.exe
mkdir -p "$out"
out=$(cd "$out" && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

(cd "$root" && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch "$go" build -trimpath \
	-ldflags="-s -w -X main.version=${tag}" -o "$stage/kubectl-joulie$exe" ./cmd/kubectl-joulie)
cp "$root/LICENSE" "$stage/LICENSE"
cp "$stage/kubectl-joulie$exe" "$out/kubectl-joulie-$goos-$goarch$exe"

archive="$out/kubectl-joulie_${tag}_${goos}_${goarch}.tar.gz"
# Fixed order, owner and time, and no name or time in the gzip header.
tar -C "$stage" --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 \
	-cf - "kubectl-joulie$exe" LICENSE | gzip -9n >"$archive"
echo "$archive"
