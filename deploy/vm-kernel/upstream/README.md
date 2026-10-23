# Native VM runner tools, x86_64 guest

This package adapts the vm2api v1.3.206 runner at the commit recorded in
`source.json`. `LICENSE.vm2api` preserves its upstream license, including its
non-commercial-use terms. The runner scripts, guest scripts, and attribution
are packaged together.

Build from this directory or any working directory with:

```sh
python3 build-native.py --context YOUR_DOCKER_CONTEXT --tag YOUR_RUNNER_TAG
```

The script reads the **Docker server** architecture and explicitly builds
`linux/arm64` or `linux/amd64`. It overrides `DOCKER_DEFAULT_PLATFORM` only in
its child processes; it does not modify your shell, Docker configuration,
contexts, or business containers. It uses the configured Docker builder,
creates no additional builder, and checks the resulting image architecture.
Build intermediates use `--rm --force-rm`; Docker layer/build caches can still
remain. Nothing is automatically pruned or started.

QEMU and runner utilities are native to that server. The copied cloud disk,
OVMF firmware, and inference payload remain **x86_64**. On ARM64, x86_64 guest
execution requires TCG; native ARM64 QEMU removes an extra runner emulation
layer but does not provide x86 KVM acceleration. On a compatible x86 Linux
server, KVM requires usable `/dev/kvm`. The build does not select or silently
fall back between these runtime modes.

The runner logs the requested and effective CPU model. The exact `qemu64`
compatibility alias becomes `max` for TCG and `host` for KVM. TCG also maps
`host` to `max`; other explicit model settings are retained. Existing disk,
identity, or firmware state is never silently discarded when configuration
changes. Recover or migrate that state explicitly.

## Source provenance and rebuild limits

`source.json.source_sha256` records the **original upstream file bytes**, not
hashes of the locally patched files. `local_patches` describes the intentional
changes to the entry/exec scripts and guest engine/service configuration. Do not use the original hashes
as a verification manifest for this modified package.

The Debian base and upstream guest image are digest-pinned. The guest-image
stage supplies data only: its amd64 QEMU binaries are not copied or executed.
QEMU and OVMF package versions are explicit. APT repositories and the remaining
dependencies are not snapshot-pinned, so rebuilding is not guaranteed to be
bit-for-bit identical or to work after those versions leave the repositories.
The `TOOLCHAIN_BASE` build argument is an explicit operator override when
building the Dockerfile directly; the normal script uses its pinned default.

Inference payloads are mounted separately; this image does not contain account
credentials. The build context allowlist includes only runner/guest scripts,
Dockerfile, license and provenance documentation. Initial guest setup may
contact distribution repositories for missing packages or 9p modules.

## Guest engine selection

`KIN_GUEST_ENGINE=claude` is the default. Set `codex` explicitly to start
`/usr/local/bin/kin-codex-kernel /run/kin/codex-kernel.json` inside the guest.
Provide the fixed Codex payload under the read-only payload share's
`usr/local/bin/kin-codex-kernel`. Its config must use guest-visible paths,
including `/run/kin-guest/kernel.sock` for the existing relay. A missing binary,
missing config or unsupported engine fails; it never falls back to Claude.
Inherited HTTP/HTTPS/ALL/NO_PROXY variables are removed for Codex; use the
explicit proxy config contract instead. Credential and proxy enforcement still
require runtime verification. This package is not yet wired into the Go helper.

Kernel service configuration is stored separately at `/run/kin-guest-config/slot.env`
(root:root, directory 0700, file 0600). `kin-kernel.service` requires this file;
its disposable `/run/kin-guest` directory contains the socket only. This keeps
engine selection across service restart and stop/start without retaining stale
service sockets. Existing guest disks install bootstrap scripts only on first
boot: replacing the runner image alone does not upgrade those installed scripts.
An explicit, drained guest bootstrap update is required for existing disks; do
not regenerate identity or delete a disk to apply a script update.
