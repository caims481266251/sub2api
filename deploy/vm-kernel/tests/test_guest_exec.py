"""Exercise the real shell helper with fake local ssh/timeout executables."""
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "upstream" / "kin-guest-exec"


class GuestExecTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="sub2-guest-exec-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.events = self.root / "events.jsonl"
        key = self.root / "test-key"
        key.write_text("fixture-not-a-real-key")
        key.chmod(0o600)
        # Relocate only the fixed key path in a temporary script copy. Production
        # keeps its fixed /slot path and gains no environment override/test hook.
        source = SCRIPT.read_text()
        self.assertEqual(source.count("KEY=/slot/kvm/id_ed25519"), 1)
        self.script = self.root / "kin-guest-exec"
        self.script.write_text(source.replace("KEY=/slot/kvm/id_ed25519", "KEY=" + shlex.quote(str(key))))
        for name in ("ssh", "timeout"):
            fake = self.root / name
            fake.write_text("#!" + sys.executable + "\n" + '''import json, os, pathlib, subprocess, sys
name = pathlib.Path(sys.argv[0]).name
with open(os.environ["TEST_EVENTS"], "a") as output:
    output.write(json.dumps({"name": name, "args": sys.argv[1:]}) + "\\n")
if name == "ssh":
    raise SystemExit(int(os.environ["TEST_SSH_STATUS"]))
forced = os.environ.get("TEST_TIMEOUT_STATUS")
if forced:
    raise SystemExit(int(forced))
raise SystemExit(subprocess.call(sys.argv[2:]))
''')
            fake.chmod(0o700)

    def run_helper(self, flags=(), ssh_status=0, timeout_status=None):
        env = dict(os.environ, PATH=str(self.root) + os.pathsep + os.environ.get("PATH", ""),
                   TEST_EVENTS=str(self.events), TEST_SSH_STATUS=str(ssh_status))
        env.pop("TEST_TIMEOUT_STATUS", None)
        if timeout_status is not None:
            env["TEST_TIMEOUT_STATUS"] = str(timeout_status)
        self.events.unlink(missing_ok=True)
        result = subprocess.run(["/bin/sh", str(self.script), *flags, "--", "printf", "hello world"],
                                env=env, text=True, capture_output=True, timeout=5)
        events = [json.loads(line) for line in self.events.read_text().splitlines()]
        return result, events

    def test_success_and_remote_failure_statuses(self):
        for status in (0, 2, 7, 42, 127):
            with self.subTest(status=status):
                result, events = self.run_helper(ssh_status=status)
                self.assertEqual(result.returncode, status, result.stderr)
                self.assertEqual([event["name"] for event in events], ["timeout", "ssh"])
                self.assertEqual(events[0]["args"][:2], ["8", "ssh"])
                self.assertNotIn("guest ssh unavailable", result.stderr)

    def test_ssh_255_maps_to_unavailable(self):
        for flags in ((), ("-i",), ("-d",)):
            with self.subTest(flags=flags):
                result, _ = self.run_helper(flags, ssh_status=255)
                self.assertEqual(result.returncode, 1)
                self.assertIn("guest ssh unavailable", result.stderr)

    def test_timeout_124_maps_to_unavailable(self):
        result, events = self.run_helper(timeout_status=124)
        self.assertEqual(result.returncode, 1)
        self.assertIn("guest ssh unavailable", result.stderr)
        self.assertEqual([event["name"] for event in events], ["timeout"])

    def test_interactive_skips_timeout_and_preserves_failure(self):
        result, events = self.run_helper(("-i", "-t"), ssh_status=23)
        self.assertEqual(result.returncode, 23)
        self.assertEqual([event["name"] for event in events], ["ssh"])
        self.assertIn("-tt", events[0]["args"])
        self.assertIn("RequestTTY=auto", events[0]["args"])

    def test_detach_remains_bounded_and_reports_launch_status(self):
        for status in (0, 19):
            with self.subTest(status=status):
                result, events = self.run_helper(("-d", "-i", "-t"), ssh_status=status)
                self.assertEqual(result.returncode, status)
                self.assertEqual([event["name"] for event in events], ["timeout", "ssh"])
                args = events[-1]["args"]
                self.assertIn("-n", args)
                self.assertNotIn("-tt", args)
                self.assertNotIn("RequestTTY=auto", args)
                self.assertIn("setsid -f sh -c", args[-1])
                self.assertIn("</dev/null >/dev/null 2>&1", args[-1])


if __name__ == "__main__":
    unittest.main()
