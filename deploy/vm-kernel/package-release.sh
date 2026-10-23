#!/usr/bin/env bash
# Run after building frontend/internal web assets. Caller owns GOCACHE/GOTMPDIR.
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"
version=$(cat backend/cmd/server/VERSION)
arch=${1:?usage: package-release.sh amd64|arm64 OUTPUT_DIRECTORY}
output=${2:?output directory required}
case "$arch" in amd64|arm64) ;; *) exit 2 ;; esac
mkdir -p "$output"
output=$(cd "$output" && pwd)
stage=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-package.XXXXXX")
trap 'python3 -c "import shutil,sys; shutil.rmtree(sys.argv[1])" "$stage"' EXIT
commit=$(git rev-parse HEAD)
date=$(git show -s --format=%cI HEAD)
export CGO_ENABLED=0 GOOS=linux GOARCH="$arch"
(
  cd backend
  go build -trimpath -tags embed -ldflags "-s -w -X main.Commit=$commit -X main.Date=$date -X main.BuildType=release" -o "$stage/sub2api" ./cmd/server
  go build -trimpath -ldflags '-s -w' -o "$stage/vm-runtime-helper" ./cmd/vm-runtime-helper
)
# Export only versioned deployment files; never include local payloads or state.
git archive HEAD deploy/vm-kernel | tar -x -C "$stage"
cp LICENSE "$stage/"
cp docs/vm-kernel-release-v2.15.0-vm.1.md "$stage/RELEASE.md"
printf 'version=%s\ncommit=%s\nos=linux\narch=%s\n' "$version" "$commit" "$arch" > "$stage/BUILD.txt"
tar -czf "$output/sub2api-vm_${version}_linux_${arch}.tar.gz" -C "$stage" .
