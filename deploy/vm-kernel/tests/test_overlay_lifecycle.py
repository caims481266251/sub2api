"""Execute the upstream overlay function with a fake qemu-img, without KVM."""
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "upstream" / "kin-kvm-entry"


class OverlayLifecycleTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sub2-overlay-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.slot = self.root / "slot"
        self.slot.mkdir()
        self.calls = self.root / "calls"
        source = SCRIPT.read_text()
        start = source.index("ensure_overlay() {")
        function = source[start:source.index("\nensure_key()", start)]
        # Only Linux UUID input is relocated for portable execution; the
        # lifecycle function, shell branching and writes run unchanged.
        uuid = self.root / "uuid"
        uuid.write_text("test-instance-uuid\n")
        function = function.replace("/proc/sys/kernel/random/uuid", shlex.quote(str(uuid)))
        self.harness = self.root / "harness"
        self.harness.write_text('set -eu\ndie() { echo "$*" >&2; exit 1; }\n' + function + '\nensure_overlay\n')
        fake = self.root / "qemu-img"
        fake.write_text("#!" + sys.executable + "\n" + '''import os, pathlib, sys
with open(os.environ["TEST_CALLS"], "a") as out: out.write("create\\n")
if os.environ.get("TEST_FAIL") == "1": raise SystemExit(9)
pathlib.Path(sys.argv[-2]).write_bytes(b"new-overlay")
''')
        fake.chmod(0o700)

    def run_overlay(self, base="base-a", size="20", fail=False):
        env = dict(os.environ, PATH=str(self.root) + os.pathsep + os.environ.get("PATH", ""),
                   SLOT_KVM=str(self.slot), BASE="/fixture/base.qcow2", BASE_ID=base,
                   KIN_KVM_DISK_GB=size, TEST_CALLS=str(self.calls), TEST_FAIL="1" if fail else "0")
        return subprocess.run(["bash", str(self.harness)], env=env, capture_output=True, timeout=5)

    def snapshot(self):
        return {p.name: p.read_bytes() for p in self.slot.iterdir() if p.is_file()}

    def prepare(self):
        result = self.run_overlay()
        self.assertEqual(result.returncode, 0, result.stderr)
        (self.slot / "disk.qcow2").write_bytes(b"valuable-account-disk\x00\xff")
        (self.slot / "OVMF_VARS.fd").write_bytes(b"persistent-nvram")

    def test_first_create_and_identical_restart_preserve_bytes(self):
        self.prepare()
        before = self.snapshot()
        result = self.run_overlay()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.snapshot(), before)
        self.assertEqual(self.calls.read_text(), "create\n")

    def test_changed_base_or_size_refuses_without_writes(self):
        self.prepare()
        before = self.snapshot()
        for base, size in (("base-b", "20"), ("base-a", "30")):
            with self.subTest(base=base, size=size):
                self.assertNotEqual(self.run_overlay(base, size).returncode, 0)
                self.assertEqual(self.snapshot(), before)
        self.assertEqual(self.calls.read_text(), "create\n")

    def test_missing_metadata_refuses_and_preserves_remaining_files(self):
        self.prepare()
        for name in ("overlay.id", "overlay.size", "instance-id"):
            with self.subTest(name=name):
                path = self.slot / name
                saved = path.read_bytes()
                path.unlink()
                before = self.snapshot()
                self.assertNotEqual(self.run_overlay().returncode, 0)
                self.assertEqual(self.snapshot(), before)
                path.write_bytes(saved)
        self.assertEqual(self.calls.read_text(), "create\n")

    def test_missing_disk_with_existing_identity_refuses(self):
        self.prepare()
        (self.slot / "disk.qcow2").unlink()
        before = self.snapshot()
        self.assertNotEqual(self.run_overlay().returncode, 0)
        self.assertEqual(self.snapshot(), before)
        self.assertEqual(self.calls.read_text(), "create\n")

    def test_untracked_disk_is_retained(self):
        (self.slot / "disk.qcow2").write_bytes(b"untracked-important-data")
        before = self.snapshot()
        self.assertNotEqual(self.run_overlay().returncode, 0)
        self.assertEqual(self.snapshot(), before)
        self.assertFalse(self.calls.exists())

    def test_symlinks_are_rejected_without_touching_state_or_targets(self):
        self.prepare()
        original = self.snapshot()
        calls_before = self.calls.read_bytes()
        for name in ("disk.qcow2", "overlay.id", "overlay.size", "instance-id", "OVMF_VARS.fd"):
            for dangling in (False, True):
                with self.subTest(name=name, dangling=dangling):
                    path = self.slot / name
                    target = self.root / "symlink-target"
                    if not dangling:
                        target.write_bytes(b"external-target-must-not-change\x00\xff")
                    target_before = target.read_bytes() if target.exists() else None
                    path.unlink()
                    path.symlink_to(target)
                    before = self.snapshot()
                    try:
                        result = self.run_overlay()
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn(b"overlay symlink refused", result.stderr)
                        self.assertTrue(path.is_symlink())
                        self.assertEqual(os.readlink(path), str(target))
                        self.assertEqual(self.snapshot(), before)
                        self.assertEqual(target.read_bytes() if target.exists() else None, target_before)
                        self.assertEqual(self.calls.read_bytes(), calls_before)
                    finally:
                        path.unlink()
                        path.write_bytes(original[name])
                        target.unlink(missing_ok=True)
        self.assertEqual(self.snapshot(), original)

    def test_creation_failure_does_not_publish_metadata(self):
        self.assertNotEqual(self.run_overlay(fail=True).returncode, 0)
        self.assertEqual(self.snapshot(), {})


if __name__ == "__main__":
    unittest.main()
