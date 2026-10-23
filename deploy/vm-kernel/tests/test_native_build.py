import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "upstream" / "build-native.py"
FAKE = '''#!/usr/bin/env python3
import json,os,sys
args=sys.argv[1:]
with open(os.environ["FAKE_LOG"],"a") as f:
 f.write(json.dumps({"args":args,"platform":os.environ.get("DOCKER_DEFAULT_PLATFORM")})+"\\n")
if args[:1]==["--context"]: args=args[2:]
if args[0]=="info": print(json.dumps({"OSType":"linux","Architecture":os.environ["FAKE_ARCH"]}))
elif args[0]=="build": sys.exit(int(os.environ.get("FAKE_BUILD_EXIT","0")))
elif args[:2]==["image","inspect"]: print(json.dumps({"Os":"linux","Architecture":os.environ["FAKE_IMAGE_ARCH"]}))
else: sys.exit(98)
'''


class NativeBuildTests(unittest.TestCase):
    def run_build(self, arch, image_arch, build_exit=0):
        with tempfile.TemporaryDirectory(prefix="native-build-test-") as temporary:
            root = Path(temporary)
            docker = root / "docker"
            docker.write_text(FAKE)
            docker.chmod(0o755)
            log = root / "calls.jsonl"
            env = os.environ.copy()
            env.update(PATH=str(root) + os.pathsep + env["PATH"], FAKE_LOG=str(log),
                       FAKE_ARCH=arch, FAKE_IMAGE_ARCH=image_arch,
                       FAKE_BUILD_EXIT=str(build_exit), DOCKER_DEFAULT_PLATFORM="linux/amd64")
            result = subprocess.run([sys.executable, str(SCRIPT), "--context", "fixture-context",
                                     "--tag", "fixture:runner"], env=env, text=True, capture_output=True)
            calls = [json.loads(line) for line in log.read_text().splitlines()]
            return result, calls

    def test_arm64_overrides_control_plane_default(self):
        result, calls = self.run_build("aarch64", "arm64")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 3)
        build = calls[1]
        self.assertEqual(build["platform"], "linux/arm64")
        self.assertEqual(build["args"][:5], ["--context", "fixture-context", "build", "--platform", "linux/arm64"])
        self.assertIn("--rm", build["args"])
        self.assertIn("--force-rm", build["args"])
        self.assertEqual(calls[2]["args"][:5], ["--context", "fixture-context", "image", "inspect", "fixture:runner"])
        self.assertIsNone(calls[0]["platform"])

    def test_amd64(self):
        result, calls = self.run_build("x86_64", "amd64")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[1]["platform"], "linux/amd64")

    def test_unknown_architecture_never_builds(self):
        result, calls = self.run_build("riscv64", "amd64")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsupported Docker server", result.stderr)
        self.assertEqual(len(calls), 1)

    def test_wrong_image_architecture_fails_verification(self):
        result, calls = self.run_build("aarch64", "amd64")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("architecture mismatch", result.stderr)
        self.assertEqual(len(calls), 3)

    def test_build_failure_does_not_inspect_old_tag(self):
        result, calls = self.run_build("aarch64", "arm64", 7)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("exit 7", result.stderr)
        self.assertEqual(len(calls), 2)


if __name__ == "__main__":
    unittest.main()
