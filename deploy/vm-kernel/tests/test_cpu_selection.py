"""Execute the production CPU-selection shell function without starting QEMU."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "upstream" / "kin-kvm-entry"


class CPUSelectionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="sub2-cpu-")
        self.addCleanup(temporary.cleanup)
        source = SCRIPT.read_text()
        start = source.index("select_cpu_model() {")
        selection = source[start:source.index("\nfind_ovmf()", start)]
        self.harness = Path(temporary.name) / "selection.sh"
        self.harness.write_text(
            'set -eu\nlog() { printf "%s\\n" "$*" >&2; }\n'
            'die() { printf "%s\\n" "$*" >&2; exit 1; }\n'
            + selection + '\nprintf "%s\\n%s\\n" "$CPU_MODEL" "$ACCEL_ARG"\n')

    def select(self, accel, requested):
        return subprocess.run(["bash", str(self.harness)],
                              env=dict(os.environ, KIN_KVM_ACCEL=accel, KIN_KVM_CPU_MODEL=requested),
                              capture_output=True, text=True, timeout=5)

    def test_fixed_payload_compatibility_alias(self):
        for accel, requested, effective, reason in (
            ("tcg", "qemu64", "max", "fixed_payload_qemu64_compatibility"),
            ("tcg", "host", "max", "tcg_host_unavailable"),
            ("kvm", "qemu64", "host", "fixed_payload_qemu64_compatibility"),
        ):
            with self.subTest(accel=accel, requested=requested):
                result = self.select(accel, requested)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.splitlines(), [effective, "tcg,thread=multi" if accel == "tcg" else "kvm"])
                self.assertEqual(result.stderr.strip(),
                                 f"cpu selection requested={requested} effective={effective} accel={accel} reason={reason}")

    def test_other_explicit_models_are_preserved_exactly(self):
        for accel in ("tcg", "kvm"):
            for requested in ("max", "host", "Haswell-noTSX-IBRS", "qemu64,+sse4.2", "unknown-model"):
                if accel == "tcg" and requested == "host":
                    continue
                with self.subTest(accel=accel, requested=requested):
                    result = self.select(accel, requested)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.splitlines()[0], requested)
                    self.assertEqual(len(result.stderr.splitlines()), 1)
                    self.assertIn("reason=explicit_model_preserved", result.stderr)

    def test_unknown_accelerator_is_rejected(self):
        result = self.select("unsupported", "qemu64")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertIn("unsupported CPU accelerator", result.stderr)


if __name__ == "__main__":
    unittest.main()
