import importlib.machinery
import importlib.util
from pathlib import Path
import sys
import re
import unittest
from unittest.mock import MagicMock, patch

spec = importlib.util.spec_from_file_location("container_common", Path(__file__).resolve().parents[1] / "container" / "common.py")
common = importlib.util.module_from_spec(spec)
spec.loader.exec_module(common)

entry_loader = importlib.machinery.SourceFileLoader("container_entry", str(Path(__file__).resolve().parents[1] / "container" / "entry"))
entry_spec = importlib.util.spec_from_loader(entry_loader.name, entry_loader)
entry = importlib.util.module_from_spec(entry_spec)
with patch.dict(sys.modules, {"common": common}):
    entry_loader.exec_module(entry)


class KernelIsolationTest(unittest.TestCase):
    def setUp(self):
        self.status = "Uid:\t10001 10001 10001 10001\nGid:\t10001 10001 10001 10001\nNoNewPrivs:\t1\n" + "".join(name + ":\t0000000000000000\n" for name in ("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"))
        self.state = {"client_identity": {"device_id": "device-a"}, "instance_id": "instance-a", "identity": {"machine_id": "machine-a", "hostname": "host-a"}}
        self.config = entry.build_kernel_config(self.state, "device-a", "fixture-token", "socks5h://127.0.0.1:1081")

    def check(self):
        common.verify_kernel_isolation(self.status, self.config, self.state, "machinea\n", "host-a")

    def test_matching_identity_and_dropped_privileges(self):
        self.check()

    def test_request_and_all_profile_startup_user_agents_match(self):
        root = Path(__file__).resolve().parents[3]
        source = (root / 'backend/internal/vmkernel/codex_identity.go').read_text()
        request_ua = re.search(r'const DefaultCodexUserAgent = "([^"]+)"', source).group(1)
        self.assertEqual(self.config['user_agent'], request_ua)
        container = root / 'deploy/docker-kernel/container'
        for dockerfile in [container / 'Dockerfile', *sorted((container / 'profiles').glob('Dockerfile.*'))]:
            text = dockerfile.read_text()
            self.assertIn('container/entry', text)
            self.assertIn('container/common.py', text)

    def test_default_user_agent_config_ignores_legacy_manifest(self):
        self.state["client_identity"].update({
            "user_agent": "codex_cli_rs/0.164.0 (Mac OS 15.1; arm64)",
            "user_agent_template": "desktop-v1/0.164.0/Mac OS/15.1/arm64",
        })
        self.config = entry.build_kernel_config(self.state, "device-a", "fixture-token", "socks5h://127.0.0.1:1081")
        self.assertEqual(self.config["user_agent"], common.DEFAULT_CODEX_USER_AGENT)
        self.assertEqual(self.config["device_id"], "device-a")
        self.assertEqual(self.config["vm_id"], "instance-a")
        self.assertEqual(self.config["upstream_transport"], "prefer_websocket")
        self.assertEqual(self.config["proxy_url"], "socks5h://127.0.0.1:1081")
        self.check()

    def test_stale_startup_user_agent_override_rejected(self):
        self.config["user_agent"] = "codex_cli_rs/0.164.0 (Mac OS 15.1; arm64)"
        with self.assertRaisesRegex(ValueError, "kernel_identity_mismatch"):
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


class DirectProxyStartupTest(unittest.TestCase):
    def test_bound_proxy_is_passed_to_resident_kernel_without_a_bridge(self):
        for scheme in ('socks5h', 'http', 'https'):
            with self.subTest(scheme=scheme):
                url = scheme + '://fixture-user:fixture-password@proxy.example:1080'
                state = {'spec': {'platform': 'codex'}, 'instance_id': 'instance-a'}
                proxy = {'url': url, 'ips': ['192.0.2.10']}
                receipt = {'device_id': '11111111-1111-4111-8111-111111111111'}
                paths = {}
                def path(value):
                    if value not in paths:
                        result = MagicMock()
                        result.is_symlink.return_value = False
                        result.exists.return_value = False
                        result.is_socket.return_value = False
                        result.read_bytes.return_value = b'fixture-config'
                        result.read_text.return_value = 'a' * 64 if value == '/slot/run/internal.token' else '127.0.0.1 localhost'
                        paths[value] = result
                    return paths[value]
                with patch.object(entry, 'projection', return_value=(state, proxy, receipt)), patch.object(entry, 'Path', side_effect=path), patch.object(entry.subprocess, 'run') as run, patch.object(entry.subprocess, 'Popen') as child, patch.object(entry.os, 'chown'), patch.object(entry.os, 'chmod'), patch.dict(entry.os.environ, {'HTTP_PROXY': 'wrong-exit', 'https_proxy': 'wrong-exit', 'ALL_PROXY': 'wrong-exit', 'NO_PROXY': '*'}), patch.object(entry.os, 'execve') as execute:
                    entry.main()
                child.assert_not_called()
                run.assert_called_once()
                self.assertEqual(run.call_args.args[0], ['/usr/local/lib/sub2-container/firewall'])
                import json
                config = json.loads(paths['/slot/run/codex-kernel.json'].write_text.call_args.args[0])
                self.assertEqual(config['proxy_url'], url)
                self.assertTrue(config['proxy_required'])
                self.assertEqual(config['device_id'], receipt['device_id'])
                binary, argv, env = execute.call_args.args
                self.assertEqual(binary, '/usr/bin/setpriv')
                self.assertIn('/opt/kin-payload/usr/local/bin/kin-codex-kernel', argv)
                self.assertIn('--no-new-privs', argv)
                self.assertFalse(any(k.lower() in ('http_proxy', 'https_proxy', 'all_proxy', 'no_proxy') for k in env))

    def test_all_images_ship_only_the_direct_proxy_runtime(self):
        container = Path(__file__).resolve().parents[1] / 'container'
        for dockerfile in [container / 'Dockerfile', *sorted((container / 'profiles').glob('Dockerfile.*'))]:
            self.assertIn('container/entry', dockerfile.read_text())
        self.assertNotIn('check_proxy_bridge', (container / 'observe').read_text())
        self.assertNotIn('proxy_bridge.py', (container / 'entry').read_text())
        marker = (container / 'proxy_bridge.py').read_text()
        self.assertIn('proxy_bridge_retired', marker)
        self.assertNotIn('import socket', marker)
        self.assertNotIn('import asyncio', marker)
