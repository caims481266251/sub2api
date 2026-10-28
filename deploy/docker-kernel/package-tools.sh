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
df -h "$work" "$output"
host_arch=$(docker info --format '{{.Architecture}}')
case "$host_arch" in x86_64) host_arch=amd64 ;; aarch64) host_arch=arm64 ;; esac
if [ "$host_arch" != "$arch" ]; then
  echo 'Release images must be built and validated on an architecture-matched Docker host.' >&2
  exit 1
fi
unset DOCKER_DEFAULT_PLATFORM
version=$(tr -d '\r\n' < backend/cmd/server/VERSION)
(cd backend && go build -trimpath -ldflags "-s -w -X main.Version=$version" -o "$work/stage/vm-runtime-helper" ./cmd/vm-runtime-helper)
# Only tracked deployment files are shipped; no local credentials or instance state.
git archive HEAD deploy/docker-kernel docs | tar -x -C "$work/stage"
cp LICENSE "$work/stage/"
# Build natively on the architecture-matched runner; users do not compile online.
image_tag="sub2api-docker-kernel:release-$version-$arch"
docker build --platform "linux/$arch" -f deploy/docker-kernel/container/Dockerfile -t "$image_tag" deploy/docker-kernel
image_id=$(docker image inspect "$image_tag" --format '{{.Id}}')
printf 'default\t%s\t%s\n' "$image_tag" "$image_id" > "$work/images.tsv"
python3 - "$arch" > "$work/profiles.tsv" <<'PYPROFILES'
import json,sys
from pathlib import Path
catalog=json.loads(Path('deploy/docker-kernel/container/profiles/profiles.json').read_text())
for name,profile in catalog['profiles'].items():
    if sys.argv[1] in profile['bases']:
        print(name,profile['dockerfile'],profile['bases'][sys.argv[1]],sep='	')
PYPROFILES
image_tags=("$image_tag")
while IFS=$'\t' read -r profile dockerfile base; do
  tag="sub2api-docker-kernel:release-$version-$arch-$profile"
  docker build --platform "linux/$arch" -f "deploy/docker-kernel/container/profiles/$dockerfile" \
    --build-arg "BASE_IMAGE=$base" --build-arg "SYSTEM_PROFILE=$profile" -t "$tag" deploy/docker-kernel
  id=$(docker image inspect "$tag" --format '{{.Id}}')
  docker run --rm --network=none --user=10001:10001 --cap-drop=ALL --security-opt=no-new-privileges \
    --entrypoint=/usr/bin/python3 "$id" /usr/local/lib/sub2-container/check-runtime.py --profile "$profile"
  docker run --rm --network=none --cap-drop=ALL --cap-add=NET_ADMIN --security-opt=no-new-privileges \
    --entrypoint=/usr/bin/python3 "$id" /usr/local/lib/sub2-container/check-runtime.py --profile "$profile" --network-check
  printf '%s\t%s\t%s\n' "$profile" "$tag" "$id" >> "$work/images.tsv"
  image_tags+=("$tag")
done < "$work/profiles.tsv"
docker save "${image_tags[@]}" -o "$work/stage/kernel-image.tar"
python3 - "$work/stage/kernel-image.tar" <<'PYSIZE'
from pathlib import Path
import sys
if Path(sys.argv[1]).stat().st_size > 6*1024*1024*1024-128*1024*1024:
    raise SystemExit('Runtime images exceed updater expanded archive limit')
PYSIZE
python3 - "$work/stage" "$version" "$arch" "$image_id" "$work/images.tsv" <<'PYMANIFEST'
import hashlib,json,sys
from pathlib import Path
root=Path(sys.argv[1])
def sha(name):
    h=hashlib.sha256()
    with (root/name).open('rb') as f:
        for chunk in iter(lambda:f.read(1024*1024),b''):h.update(chunk)
    return h.hexdigest()
images={}
for line in Path(sys.argv[5]).read_text().splitlines():
    profile,tag,image=line.split('	')
    images[profile]={'tag':tag,'image':image}
(root/'runtime-release.json').write_text(json.dumps({'version':sys.argv[2],'arch':sys.argv[3],'image':sys.argv[4],'protocol':1,'manifest_version':2,'minimum_updater':2,'images':images,'files':{n:sha(n) for n in ('vm-runtime-helper','kernel-image.tar')}}))
PYMANIFEST
printf 'version=%s\ncommit=%s\nos=linux\narch=%s\n' "$version" "$(git rev-parse HEAD)" "$arch" > "$work/stage/BUILD.txt"
name="docker-kernel-tools-${version}-linux-${arch}.tar.gz"
tar -czf "$output/$name" -C "$work/stage" .
python3 - "$output/$name" <<'PYSIZE'
from pathlib import Path
import sys
if Path(sys.argv[1]).stat().st_size > 2*1024*1024*1024:
    raise SystemExit('Runtime package exceeds updater download limit')
PYSIZE
(cd "$output" && shasum -a 256 "$name" > "$name.sha256")
# A legacy updater must be bootstrapped before it can understand a multi-image
# release. Ship a small separately checksummed installer for that first step.
bootstrap="docker-kernel-updater-bootstrap-$version-linux-$arch.tar.gz"
tar -czf "$output/$bootstrap" -C "$work/stage" \
  deploy/docker-kernel/host/update_agent.py deploy/docker-kernel/host/install-update-agent.py BUILD.txt
(cd "$output" && shasum -a 256 "$bootstrap" > "$bootstrap.sha256")
df -h "$output"
