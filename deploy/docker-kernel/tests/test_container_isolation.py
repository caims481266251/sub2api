import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("container_common", Path(__file__).resolve().parents[1] / "container" / "common.py")
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)


class KernelIsolationTest(unittest.TestCase):
    def setUp(self):
        self.status = "Uid:\t10001 10001 10001 10001\nGid:\t10001 10001 10001 10001\nNoNewPrivs:\t1\n" + "".join(name + ":\t0000000000000000\n" for name in ("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"))
        self.state = {"client_identity": {"device_id": "device-a", "user_agent": "codex_cli_rs/0.164.0 (Mac OS 15.1; arm64)"}, "instance_id": "instance-a", "identity": {"machine_id": "machine-a", "hostname": "host-a"}}
        self.config = {"user_agent": "codex_cli_rs/0.164.0 (Mac OS 15.1; arm64)", "device_id": "device-a", "vm_id": "instance-a", "credential_path": "/slot/credentials/credential.json", "socket_path": "/slot/run/kernel.sock", "proxy_required": True}

    def check(self):
        common.verify_kernel_isolation(self.status, self.config, self.state, "machinea\n", "host-a")

    def test_matching_identity_and_dropped_privileges(self):
        self.check()

    def test_root_or_retained_capability_rejected(self):
        original = self.status
        for old, new in (("10001 10001 10001 10001", "0 0 0 0"), ("CapEff:\t0000000000000000", "CapEff:\t0000000000000001"), ("NoNewPrivs:\t1", "NoNewPrivs:\t0")):
            self.status = original.replace(old, new)
            with self.assertRaises(ValueError):
                self.check()

    def test_cross_account_identity_or_proxy_bypass_rejected(self):
        for key, value in (("user_agent", "caller-UA"), ("device_id", "device-b"), ("vm_id", "instance-b"), ("credential_path", "/other/credential.json"), ("proxy_required", False)):
            original = self.config.copy()
            self.config[key] = value
            with self.assertRaises(ValueError):
                self.check()
            self.config = original

    def test_kernel_must_report_loaded_identity(self):
        common.verify_kernel_identity_receipt({"device_id": "device-a", "vm_id": "instance-a"}, self.config)
        for health in ({}, {"device_id": "device-b", "vm_id": "instance-a"}, {"device_id": "device-a", "vm_id": "instance-b"}):
            with self.assertRaises(ValueError):
                common.verify_kernel_identity_receipt(health, self.config)
