import importlib.util
import pathlib
import socket
import threading
import unittest
import asyncio

MODULE = pathlib.Path(__file__).parents[1] / 'container' / 'proxy_bridge.py'
spec = importlib.util.spec_from_file_location('proxy_bridge', MODULE)
proxy_bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy_bridge)


class FakeSocksProxy:
    def __init__(self, failures=0, auth=True, connect_code=0):
        self.failures = failures
        self.auth = auth
        self.connect_code = connect_code
        self.accepted = 0
        self.tunnels = []
        self.listener = socket.socket()
        self.listener.bind(('127.0.0.1', 0))
        self.listener.listen(20)
        self.port = self.listener.getsockname()[1]
        threading.Thread(target=self._serve, daemon=True).start()

    @staticmethod
    def read(conn, size):
        out = bytearray()
        while len(out) < size:
            chunk = conn.recv(size - len(out))
            if not chunk:
                return b''
            out.extend(chunk)
        return bytes(out)

    def _serve(self):
        while True:
            try:
                conn, _ = self.listener.accept()
            except OSError:
                return
            threading.Thread(target=self._client, args=(conn,), daemon=True).start()

    def _client(self, conn):
        with conn:
            self.accepted += 1
            if self.accepted <= self.failures:
                conn.close()
                return
            head = self.read(conn, 2)
            if not head:
                return
            self.read(conn, head[1])
            conn.sendall(b'\x05\x02')
            auth = self.read(conn, 2)
            if not auth:
                return
            self.read(conn, auth[1])
            plen = self.read(conn, 1)
            if not plen:
                return
            self.read(conn, plen[0])
            conn.sendall(b'\x01' + (b'\x00' if self.auth else b'\x01'))
            if not self.auth:
                return
            req = self.read(conn, 4)
            if len(req) != 4:
                return
            atyp = req[3]
            if atyp == 3:
                size = self.read(conn, 1)
                host = size + self.read(conn, size[0])
            elif atyp == 1:
                host = self.read(conn, 4)
            else:
                host = self.read(conn, 16)
            port = self.read(conn, 2)
            self.tunnels.append((atyp, host, port))
            if self.connect_code:
                conn.sendall(bytes([5, self.connect_code, 0, 1, 127, 0, 0, 1, 0, 80]))
                return
            conn.sendall(b'\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x50')
            while True:
                data = conn.recv(4096)
                if not data:
                    return
                conn.sendall(data)

    def close(self):
        self.listener.close()


class ProxyBridgeTests(unittest.TestCase):
    def test_retries_proxy_handshake_then_establishes_one_tunnel(self):
        proxy = FakeSocksProxy(failures=1)
        try:
            bridge = proxy_bridge.ProxyBridge(f'socks5://user:pass@127.0.0.1:{proxy.port}', ['127.0.0.1'])
            target = b'\x03\x0bchatgpt.com\x01\xbb'
            async def establish_and_close():
                reader, upstream, reply = await bridge.upstream(target)
                self.assertEqual(reply[:2], b'\x05\x00')
                upstream.close(); await upstream.wait_closed()
            asyncio.run(establish_and_close())
            self.assertEqual(proxy.accepted, 2)
            self.assertEqual(len(proxy.tunnels), 1)
            self.assertEqual(proxy.tunnels[0][0], 3)
        finally:
            proxy.close()

    def test_auth_rejection_is_not_retried(self):
        proxy = FakeSocksProxy(auth=False)
        try:
            bridge = proxy_bridge.ProxyBridge(f'socks5://user:pass@127.0.0.1:{proxy.port}', ['127.0.0.1'])
            with self.assertRaises(proxy_bridge.HandshakeFailure) as caught:
                asyncio.run(bridge.upstream(b'\x03\x0bchatgpt.com\x01\xbb'))
            self.assertEqual((caught.exception.code, caught.exception.reason), (2, 'auth_rejected'))
            self.assertEqual(proxy.accepted, 1)
        finally:
            proxy.close()

    def test_does_not_retry_after_tunnel_is_established(self):
        proxy = FakeSocksProxy()
        try:
            bridge = proxy_bridge.ProxyBridge(f'socks5://user:pass@127.0.0.1:{proxy.port}', ['127.0.0.1'])
            async def establish_and_close():
                _, upstream, _ = await bridge.upstream(b'\x03\x0bchatgpt.com\x01\xbb')
                upstream.close(); await upstream.wait_closed()
            asyncio.run(establish_and_close())
            self.assertEqual(proxy.accepted, 1)
        finally:
            proxy.close()

    def test_destination_refused_is_not_retried(self):
        proxy = FakeSocksProxy(connect_code=5)
        try:
            bridge = proxy_bridge.ProxyBridge(f'socks5://user:pass@127.0.0.1:{proxy.port}', ['127.0.0.1'])
            with self.assertRaises(proxy_bridge.HandshakeFailure) as caught:
                asyncio.run(bridge.upstream(b'\x03\x0bchatgpt.com\x01\xbb'))
            self.assertEqual((caught.exception.code, caught.exception.retryable), (5, False))
            self.assertEqual(proxy.accepted, 1)
        finally:
            proxy.close()

    def test_client_payload_is_forwarded_once_after_connect(self):
        proxy = FakeSocksProxy(failures=1)
        try:
            bridge = proxy_bridge.ProxyBridge(f'socks5://user:pass@127.0.0.1:{proxy.port}', ['127.0.0.1'])
            listener = socket.socket()
            listener.bind(('127.0.0.1', 0))
            listener.listen(1)
            async def exercise():
                server = await asyncio.start_server(bridge.handle, '127.0.0.1', 0)
                port = server.sockets[0].getsockname()[1]
                try:
                    reader, writer = await asyncio.open_connection('127.0.0.1', port)
                    writer.write(b'\x05\x01\x00'); await writer.drain()
                    self.assertEqual(await reader.readexactly(2), b'\x05\x00')
                    writer.write(b'\x05\x01\x00\x03\x0bchatgpt.com\x01\xbb'); await writer.drain()
                    self.assertEqual((await reader.readexactly(10))[:2], b'\x05\x00')
                    writer.write(b'one-upstream-request'); await writer.drain()
                    self.assertEqual(await reader.readexactly(20), b'one-upstream-request')
                    writer.close(); await writer.wait_closed()
                finally:
                    server.close(); await server.wait_closed()
            asyncio.run(exercise())
            self.assertEqual(proxy.accepted, 2)
            self.assertEqual(len(proxy.tunnels), 1)
            listener.close()
        finally:
            proxy.close()


if __name__ == '__main__':
    unittest.main()
