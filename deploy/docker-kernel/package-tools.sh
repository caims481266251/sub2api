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
(cd backend && go build -trimpath -ldflags "-s -w -X main.Version=$version" -o "$work/stage/vm-runtime-helper" ./cmd/vm-runtime-helper)
# Only tracked deployment files are shipped; no local credentials or instance state.
git archive HEAD deploy/docker-kernel docs | tar -x -C "$work/stage"
cp LICENSE "$work/stage/"
# Build natively on the architecture-matched runner; users do not compile online.
image_tag="sub2api-docker-kernel:release-$version-$arch"
docker build -f deploy/docker-kernel/container/Dockerfile -t "$image_tag" deploy/docker-kernel
image_id=$(docker image inspect "$image_tag" --format '{{.Id}}')
docker save "$image_tag" -o "$work/stage/kernel-image.tar"
python3 - "$work/stage" "$version" "$arch" "$image_id" <<'PYMANIFEST'
import hashlib,json,sys
from pathlib import Path
root=Path(sys.argv[1])
def sha(name):
    h=hashlib.sha256()
    with (root/name).open('rb') as f:
        for chunk in iter(lambda:f.read(1024*1024),b''):h.update(chunk)
    return h.hexdigest()
(root/'runtime-release.json').write_text(json.dumps({'version':sys.argv[2],'arch':sys.argv[3],'image':sys.argv[4],'protocol':1,'files':{n:sha(n) for n in ('vm-runtime-helper','kernel-image.tar')}}))
PYMANIFEST
printf 'version=%s\ncommit=%s\nos=linux\narch=%s\n' "$version" "$(git rev-parse HEAD)" "$arch" > "$work/stage/BUILD.txt"
name="docker-kernel-tools-${version}-linux-${arch}.tar.gz"
tar -czf "$output/$name" -C "$work/stage" .
(cd "$output" && shasum -a 256 "$name" > "$name.sha256")
