import contextlib
import hashlib
import io
import json
from pathlib import Path
import runpy
import sys
import unittest
from unittest.mock import MagicMock, patch

CONTAINER = Path(__file__).parents[1] / 'container'
sys.path.insert(0, str(CONTAINER))
observe = runpy.run_path(str(CONTAINER / 'observe'), run_name='test_observe')


class DirectProxyHealthTest(unittest.TestCase):
    def check(self, proxy='socks5h://proxy.invalid:1080', healthy=True, applied=True):
        config_bytes = b'fixed kernel configuration'
        proof = {'config_digest': hashlib.sha256(config_bytes).hexdigest(),
                 'generation': 1, 'credential_revision': 2, 'proxy_revision': 3}
        config = {'proxy_url': 'socks5h://proxy.invalid:1080'}
        values = {'/run/sub2-container-applied.json': proof,
                  '/slot/run/codex-kernel.json': config,
                  '/slot/credentials/proxy.json': {'url': proxy},
                  '/slot/run/instance.json': {}}
        response = MagicMock(status=200)
        response.read.return_value = json.dumps({'healthy': healthy}).encode()
        conn = MagicMock()
        conn.getresponse.return_value = response
        projection = MagicMock(return_value=(None, None, {'proxy_revision': 3 if applied else 4}))
        output = io.StringIO()
        bindings = {'read': lambda name: values[name], 'LocalHTTP': MagicMock(return_value=conn),
                    'process_identity': lambda: 'stable-kernel-process', 'projection': projection,
                    'verify_kernel_isolation': MagicMock(), 'verify_kernel_identity_receipt': MagicMock()}
        with patch.dict(observe['main'].__globals__, bindings), \
             patch.object(Path, 'read_bytes', return_value=config_bytes), \
             patch.object(Path, 'read_text', return_value='fixture'), \
             patch('subprocess.run') as firewall, \
             patch('socket.create_connection', side_effect=AssertionError('health must stay local')), \
             contextlib.redirect_stdout(output):
            observe['main']()
        firewall.assert_called_once()
        conn.close.assert_called_once()
        return json.loads(output.getvalue())

    def test_healthy_kernel_needs_no_proxy_bridge_or_upstream_probe(self):
        result = self.check()
        self.assertTrue(result['kernel_ready'])
        self.assertTrue(result['configuration_applied'])

    def test_bound_proxy_mismatch_is_not_ready(self):
        with self.assertRaisesRegex(ValueError, 'kernel_proxy_mismatch'):
            self.check(proxy='socks5h://other.invalid:1080')

    def test_unhealthy_kernel_is_not_ready(self):
        with self.assertRaisesRegex(ValueError, 'kernel_not_ready'):
            self.check(healthy=False)

    def test_pending_projection_does_not_mark_kernel_unhealthy(self):
        result = self.check(applied=False)
        self.assertTrue(result['kernel_ready'])
        self.assertFalse(result['configuration_applied'])


if __name__ == '__main__':
    unittest.main()
