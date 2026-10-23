#!/usr/bin/env bash
# Supplemental host tools; intentionally excludes the application and kernel payload.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"
arch=${1:?usage: package-tools.sh amd64|arm64 OUTPUT_DIRECTORY}
output=${2:?output directory required}
case "$arch" in amd64|arm64) ;; *) exit 2 ;; esac
mkdir -p "$output"
output=$(cd "$output" && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-tools.XXXXXX")
trap 'rm -rf -- "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir -p "$work/cache" "$work/tmp" "$work/stage"
export GOCACHE="$work/cache" GOTMPDIR="$work/tmp"
export CGO_ENABLED=0 GOOS=linux GOARCH="$arch"
version=$(tr -d '\r\n' < backend/cmd/server/VERSION)
(cd backend && go build -trimpath -ldflags '-s -w' -o "$work/stage/vm-runtime-helper" ./cmd/vm-runtime-helper)
# Only tracked deployment files are shipped; no local credentials or instance state.
git archive HEAD deploy/docker-kernel docs | tar -x -C "$work/stage"
cp LICENSE "$work/stage/"
printf 'version=%s\ncommit=%s\nos=linux\narch=%s\n' "$version" "$(git rev-parse HEAD)" "$arch" > "$work/stage/BUILD.txt"
name="docker-kernel-tools-${version}-linux-${arch}.tar.gz"
tar -czf "$output/$name" -C "$work/stage" .
(cd "$output" && shasum -a 256 "$name" > "$name.sha256")
