import os
from pathlib import Path
import runpy
import sys
import unittest
from unittest.mock import MagicMock, patch

CONTAINER = Path(__file__).parents[1] / 'container'
sys.path.insert(0, str(CONTAINER))
observe = runpy.run_path(str(CONTAINER / 'observe'), run_name='test_observe')


class BridgeHealthTest(unittest.TestCase):
    def check(self, argv=None, replies=(b'\x05\x00', b'\x05\x08'), ready=True):
        if argv is None:
            argv = b'/usr/bin/python3\0/usr/local/lib/sub2-container/proxy_bridge.py\0'
        probe = MagicMock()
        probe.__enter__.return_value = probe
        probe.recv.side_effect = replies
        # The production root health check has no CAP_KILL and the bridge runs
        # as UID 10001. Signal permission is not evidence of process health.
        with patch.object(os, 'kill', side_effect=PermissionError()) as kill, \
             patch.object(Path, 'read_text', return_value='17'), \
             patch.object(Path, 'read_bytes', return_value=argv), \
             patch.object(Path, 'is_file', return_value=ready), \
             patch('socket.create_connection', return_value=probe):
            observe['check_proxy_bridge']()
            kill.assert_not_called()

    def test_healthy_bridge_without_signal_permission(self):
        self.check()

    def test_reused_pid_is_not_a_healthy_bridge(self):
        with self.assertRaisesRegex(ValueError, 'proxy_bridge_not_ready'):
            self.check(argv=b'/usr/bin/python3\0/unrelated.py\0')

    def test_missing_ready_marker(self):
        with self.assertRaisesRegex(ValueError, 'proxy_bridge_not_ready'):
            self.check(ready=False)

    def test_unresponsive_or_wrong_listener(self):
        for replies in [(b'',), (b'NO',), (b'\x05\x00', b'NO')]:
            with self.subTest(replies=replies), self.assertRaisesRegex(ValueError, 'proxy_bridge_not_ready'):
                self.check(replies=replies)


if __name__ == '__main__':
    unittest.main()
