"""Resolver projection survives cloud-init rewriting and proxy rotation."""
import importlib.machinery
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1] / 'upstream/guest'
loader = importlib.machinery.SourceFileLoader('guest_network', str(ROOT / 'sub2-vm-network'))
spec = importlib.util.spec_from_loader(loader.name, loader)
network = importlib.util.module_from_spec(spec)
loader.exec_module(network)


class ProxyHostsTests(unittest.TestCase):
    def test_cloud_init_overwrite_is_detected_and_reapplied(self):
        with tempfile.TemporaryDirectory() as root:
            hosts = Path(root) / 'hosts'
            original = '127.0.0.1 localhost\n127.0.1.1 fixture-guest\n'
            hosts.write_text(original)
            proxy = {'url': 'http://proxy.invalid:8080', 'ips': ['192.0.2.10']}
            network.project_proxy_hosts(proxy, hosts)
            network.project_proxy_hosts(proxy, hosts, check=True)
            hosts.write_text(original)  # cloud-init manage_etc_hosts rewrite
            with self.assertRaisesRegex(ValueError, 'proxy_hosts_unconfirmed'):
                network.project_proxy_hosts(proxy, hosts, check=True)
            self.assertEqual(hosts.read_text(), original, 'observe must not mutate')
            network.project_proxy_hosts(proxy, hosts)
            network.project_proxy_hosts(proxy, hosts, check=True)
            self.assertIn(original, hosts.read_text())

    def test_rotation_removes_old_proxy_mapping(self):
        with tempfile.TemporaryDirectory() as root:
            hosts = Path(root) / 'hosts'
            hosts.write_text('127.0.0.1 localhost\n')
            network.project_proxy_hosts({'url': 'http://old.invalid:8080', 'ips': ['192.0.2.10']}, hosts)
            current = {'url': 'http://new.invalid:8080', 'ips': ['192.0.2.11', '192.0.2.12']}
            network.project_proxy_hosts(current, hosts)
            network.project_proxy_hosts(current, hosts, check=True)
            self.assertNotIn('old.invalid', hosts.read_text())
            self.assertEqual(hosts.read_text().count('# sub2-proxy'), 2)

    def test_consistent_apply_and_observe_do_not_run_system_commands(self):
        class ProxyFile:
            def read_text(self):
                return '{"url":"http://proxy.invalid:8080","ips":["192.0.2.10"]}'
        original = network.Path
        def path(value):
            return ProxyFile() if str(value) == '/run/sub2-credentials/proxy.json' else original(value)
        for args in ([], ['--check']):
            with patch.object(network.sys, 'argv', ['network'] + args), patch.object(network, 'Path', side_effect=path), patch.object(network, 'check_network'), patch.object(network, 'project_claude_credentials'), patch.object(network.subprocess, 'run') as run:
                network.main()
                run.assert_not_called()

    def test_boot_order_and_cloud_init_do_not_overwrite_projection(self):
        self.assertIn('manage_etc_hosts: false', (ROOT / 'user-data').read_text())
        after = next(line for line in (ROOT / 'kin-guest-setup.service').read_text().splitlines() if line.startswith('After='))
        self.assertIn('cloud-init.service', after)


if __name__ == '__main__':
    unittest.main()
