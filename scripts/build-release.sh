#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (c) 2026 TrendVidia, LLC.

# Builds caliper's release binaries for every supported platform into a
# directory, with a checksums.txt beside them. The release workflow runs
# this; running it by hand gives the same files.
#
#   scripts/build-release.sh v1.6.1 dist
#
# The binaries refuse a go command of a newer minor release than the Go
# that built them (caliper #35), so build with the newest Go caliper
# supports, not go.mod's minimum.
set -euo pipefail

version=${1:?usage: build-release.sh VERSION OUTDIR}
out=${2:?usage: build-release.sh VERSION OUTDIR}

mkdir -p "$out"
for platform in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/arm64 windows/amd64; do
  goos=${platform%/*}
  goarch=${platform#*/}
  ext=""
  [ "$goos" = windows ] && ext=".exe"
  for cmd in caliper-mcp:caliper caliper-analyze:caliper-analyze; do
    pkg=${cmd%%:*}
    name=${cmd#*:}
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath \
      -ldflags "-X main.Version=$version" \
      -o "$out/$name-$goos-$goarch$ext" "./cmd/$pkg"
  done
done

(cd "$out" && shasum -a 256 caliper-* > checksums.txt)
