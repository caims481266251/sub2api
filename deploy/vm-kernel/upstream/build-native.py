#!/usr/bin/env python3
"""Build runner tools for the Docker server CPU; never start a container."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys


def build(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", help="Docker context (otherwise use Docker's current context)")
    parser.add_argument("--tag", required=True, help="Tag for the resulting local runner image")
    args = parser.parse_args(argv)
    if not args.tag.strip() or (args.context is not None and not args.context.strip()):
        parser.error("tag and context must not be blank")
    docker = ["docker"]
    if args.context:
        docker += ["--context", args.context]
    env = os.environ.copy()
    # The control plane can default to amd64 for payload execution. That default
    # must not select the architecture of the QEMU executable we build here.
    env.pop("DOCKER_DEFAULT_PLATFORM", None)

    def query(arguments):
        result = subprocess.run(docker + arguments, env=env, text=True, capture_output=True)
        if result.returncode:
            detail = result.stderr.strip() or result.stdout.strip()
            raise RuntimeError(f"docker {' '.join(arguments[:2])} failed ({result.returncode}): {detail}")
        try:
            return json.loads(result.stdout)
        except ValueError as error:
            raise RuntimeError("Docker returned invalid JSON metadata") from error

    info = query(["info", "--format", "{{json .}}"])
    if not isinstance(info, dict):
        raise RuntimeError("Docker server info must be an object")
    architecture = str(info.get("Architecture", "")).lower()
    normalized = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64", "amd64": "amd64"}.get(architecture)
    if info.get("OSType") != "linux" or normalized is None:
        raise RuntimeError(f"unsupported Docker server: {info.get('OSType')}/{architecture}; require Linux arm64 or amd64")
    platform = "linux/" + normalized
    env["DOCKER_DEFAULT_PLATFORM"] = platform
    source = Path(__file__).resolve().parent
    print(f"Building {args.tag} for Docker server {platform}; guest remains amd64", flush=True)
    result = subprocess.run(docker + ["build", "--platform", platform, "--rm", "--force-rm", "--tag", args.tag, str(source)], env=env)
    if result.returncode:
        raise RuntimeError(f"runner image build failed (exit {result.returncode}); inspect build output above")
    image = query(["image", "inspect", args.tag, "--format", "{{json .}}"])
    if not isinstance(image, dict):
        raise RuntimeError("Docker image inspect must return an object")
    if image.get("Os") != "linux" or image.get("Architecture") != normalized:
        raise RuntimeError(f"built image architecture mismatch: expected {platform}, got {image.get('Os')}/{image.get('Architecture')}; do not deploy {args.tag}")
    print(f"Verified {args.tag}: {platform}. No container was started.")


if __name__ == "__main__":
    try:
        build()
    except (RuntimeError, OSError) as error:
        print(f"build-native: {error}", file=sys.stderr)
        sys.exit(1)
