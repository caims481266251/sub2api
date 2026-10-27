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


class BridgeLifecycleTests(unittest.IsolatedAsyncioTestCase):
    def bridge(self, **kwargs):
        return proxy_bridge.ProxyBridge('socks5://127.0.0.1:1080', ['127.0.0.1'], **kwargs)

    async def test_client_eof_cancels_in_progress_handshake(self):
        bridge = self.bridge()
        started, canceled = asyncio.Event(), asyncio.Event()
        async def connect(target):
            started.set()
            try:
                await asyncio.Future()
            finally:
                canceled.set()
        bridge.upstream = connect
        reader = asyncio.StreamReader()
        work = asyncio.create_task(bridge.connect_for_client(reader, b'target'))
        await started.wait()
        reader.feed_eof()
        self.assertIsNone(await asyncio.wait_for(work, 1))
        self.assertTrue(canceled.is_set())

    async def test_cancellation_during_retry_delay_stops_more_attempts(self):
        bridge = self.bridge(delays=(30, 30))
        failed = asyncio.Event()
        calls = []
        async def connect(target, attempt, state):
            calls.append(attempt)
            failed.set()
            raise proxy_bridge.HandshakeFailure('peer_closed', retryable=True)
        bridge._connect = connect
        reader = asyncio.StreamReader()
        work = asyncio.create_task(bridge.connect_for_client(reader, b'target'))
        await failed.wait()
        reader.feed_eof()
        self.assertIsNone(await asyncio.wait_for(work, 1))
        self.assertEqual(calls, [1])

    async def test_connect_success_racing_client_eof_closes_remote(self):
        from unittest.mock import AsyncMock, Mock
        bridge = self.bridge()
        writer = Mock()
        writer.wait_closed = AsyncMock()
        bridge.upstream = AsyncMock(return_value=(object(), writer, b'reply'))
        reader = asyncio.StreamReader()
        reader.feed_eof()
        self.assertIsNone(await bridge.connect_for_client(reader, b'target'))
        writer.close.assert_called_once()

    async def test_task_cancellation_closes_new_remote(self):
        from unittest.mock import AsyncMock, Mock
        bridge = self.bridge()
        writer = Mock()
        writer.wait_closed = AsyncMock()
        bridge.upstream = AsyncMock(return_value=(object(), writer, b'reply'))
        reader = asyncio.StreamReader()
        work = asyncio.create_task(bridge.connect_for_client(reader, b'target'))
        # Allow connect to complete, then cancel before ownership is transferred.
        await asyncio.sleep(0)
        await asyncio.sleep(0)
        work.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await work
        writer.close.assert_called_once()

    async def test_socket_configuration_failure_closes_connection(self):
        from unittest.mock import AsyncMock, Mock, patch
        import errno
        writer = Mock()
        writer.wait_closed = AsyncMock()
        writer.get_extra_info.return_value.setsockopt.side_effect = OSError(errno.EINVAL, 'fixture')
        bridge = self.bridge()
        with patch.object(proxy_bridge.asyncio, 'open_connection', AsyncMock(return_value=(object(), writer))):
            with self.assertRaises(proxy_bridge.HandshakeFailure):
                await bridge.upstream(b'target')
        writer.close.assert_called_once()

    async def test_retry_budget_is_three(self):
        from unittest.mock import AsyncMock
        bridge = self.bridge(delays=(0, 0))
        bridge._connect = AsyncMock(side_effect=proxy_bridge.HandshakeFailure('peer_closed', retryable=True))
        with self.assertRaises(proxy_bridge.HandshakeFailure):
            await bridge.upstream(b'target')
        self.assertEqual(bridge._connect.await_count, 3)

    async def test_pipelined_payload_and_half_close_preserve_response(self):
        handlers = set()
        received = []
        async def upstream(reader, writer):
            task = asyncio.current_task(); handlers.add(task)
            try:
                received.append(await reader.read())
                # Half-close must leave downstream response delivery intact.
                await asyncio.sleep(.02)
                writer.write(b'complete-response'); await writer.drain()
            finally:
                await proxy_bridge.close_writer(writer)
                handlers.discard(task)
        server = await asyncio.start_server(upstream, '127.0.0.1', 0)
        bridge = self.bridge()
        async def connect(target):
            r, w = await asyncio.open_connection('127.0.0.1', server.sockets[0].getsockname()[1])
            return r, w, b'\x05\x00\x00\x01\x00\x00\x00\x00\x00\x00'
        bridge.upstream = connect
        async def handle(r, w):
            task = asyncio.current_task(); handlers.add(task)
            try: await bridge.handle(r, w)
            finally: handlers.discard(task)
        local = await asyncio.start_server(handle, '127.0.0.1', 0)
        try:
            r, w = await asyncio.open_connection('127.0.0.1', local.sockets[0].getsockname()[1])
            w.write(b'\x05\x01\x00'); await w.drain()
            self.assertEqual(await r.readexactly(2), b'\x05\x00')
            w.write(b'\x05\x01\x00\x03\x0bchatgpt.com\x01\xbb' + b'payload-once')
            await w.drain()
            self.assertEqual((await r.readexactly(10))[:2], b'\x05\x00')
            w.write_eof()
            self.assertEqual(await asyncio.wait_for(r.read(), 1), b'complete-response')
            self.assertEqual(received, [b'payload-once'])
            await proxy_bridge.close_writer(w)
        finally:
            local.close(); server.close()
            await local.wait_closed(); await server.wait_closed()
            if handlers: await asyncio.wait_for(asyncio.gather(*handlers), 1)

    async def test_reset_cancels_other_relay_direction_and_does_not_leak_diagnostics(self):
        from unittest.mock import AsyncMock, Mock, patch
        import errno
        failed = AsyncMock()
        failed.read.side_effect = OSError(errno.ECONNRESET, 'socks5://secret:password@private.example')
        waiting = asyncio.StreamReader()
        with patch.object(proxy_bridge, 'diagnostic') as log:
            await asyncio.wait_for(self.bridge().relay(waiting, Mock(), failed, Mock()), 1)
        log.assert_called_once_with('closed', 'tunnel_download', 1, 'connection_reset')

    async def test_kernel_eof_bounds_silent_proxy_drain(self):
        from unittest.mock import AsyncMock, Mock, patch
        left, right = asyncio.StreamReader(), asyncio.StreamReader()
        left.feed_eof()
        local, remote = Mock(), Mock()
        local.drain = AsyncMock(); remote.drain = AsyncMock()
        with patch.object(proxy_bridge, 'diagnostic') as log:
            await asyncio.wait_for(self.bridge().relay(left, local, right, remote,
                                                       half_close_timeout=.05), 1)
        remote.write_eof.assert_called_once()
        log.assert_called_once_with('closed', 'tunnel_drain', 1, 'kernel_eof_idle_timeout')

    async def test_open_quiet_stream_has_no_half_close_deadline(self):
        from unittest.mock import AsyncMock, Mock, patch
        left, right = asyncio.StreamReader(), asyncio.StreamReader()
        local, remote = Mock(), Mock()
        local.drain = AsyncMock(); remote.drain = AsyncMock()
        with patch.object(proxy_bridge, 'diagnostic') as log:
            work = asyncio.create_task(self.bridge().relay(left, local, right, remote,
                                                           half_close_timeout=.03))
            try:
                await asyncio.sleep(.1)
                self.assertFalse(work.done())
                right.feed_data(b'late-response'); right.feed_eof(); left.feed_eof()
                await asyncio.wait_for(work, 1)
            finally:
                work.cancel(); await asyncio.gather(work, return_exceptions=True)
        local.write.assert_called_once_with(b'late-response')
        log.assert_not_called()

    async def test_half_closed_stream_keeps_draining_while_data_is_delivered(self):
        from unittest.mock import AsyncMock, Mock, patch
        left, right = asyncio.StreamReader(), asyncio.StreamReader()
        left.feed_eof()
        local, remote = Mock(), Mock()
        local.drain = AsyncMock(); remote.drain = AsyncMock()
        with patch.object(proxy_bridge, 'diagnostic') as log:
            work = asyncio.create_task(self.bridge().relay(left, local, right, remote,
                                                           half_close_timeout=.1))
            try:
                for i in range(6):
                    await asyncio.sleep(.04)
                    self.assertFalse(work.done())
                    right.feed_data(bytes([i]))
                right.feed_eof()
                await asyncio.wait_for(work, 1)
            finally:
                work.cancel(); await asyncio.gather(work, return_exceptions=True)
        self.assertEqual(b''.join(call.args[0] for call in local.write.call_args_list), bytes(range(6)))
        log.assert_not_called()

    async def test_kernel_eof_bounds_blocked_download_write(self):
        from unittest.mock import AsyncMock, Mock, patch
        left, right = asyncio.StreamReader(), asyncio.StreamReader()
        left.feed_eof(); right.feed_data(b'undeliverable-tail')
        local, remote = Mock(), Mock()
        canceled = asyncio.Event()
        async def blocked():
            try: await asyncio.Future()
            finally: canceled.set()
        local.drain = blocked; remote.drain = AsyncMock()
        with patch.object(proxy_bridge, 'diagnostic') as log:
            await asyncio.wait_for(self.bridge().relay(left, local, right, remote,
                                                       half_close_timeout=.05), 1)
        self.assertTrue(canceled.is_set())
        log.assert_called_once_with('closed', 'tunnel_drain', 1, 'kernel_eof_idle_timeout')

    async def test_relay_cancellation_cleans_up_deadline_and_both_directions(self):
        from unittest.mock import AsyncMock, Mock
        before = asyncio.all_tasks()
        local, remote = Mock(), Mock()
        local.drain = AsyncMock(); remote.drain = AsyncMock()
        work = asyncio.create_task(self.bridge().relay(asyncio.StreamReader(), local,
                                                       asyncio.StreamReader(), remote))
        await asyncio.sleep(0)
        work.cancel()
        with self.assertRaises(asyncio.CancelledError): await work
        self.assertEqual(asyncio.all_tasks() - before, set())

    async def test_close_writer_aborts_backpressured_socket(self):
        accepted = asyncio.get_running_loop().create_future()
        def accept(reader, writer):
            # Keep the peer open without consuming its StreamReader buffer.
            accepted.set_result((reader, writer))
        server = await asyncio.start_server(accept, '127.0.0.1', 0, limit=1024)
        reader, writer = await asyncio.open_connection('127.0.0.1', server.sockets[0].getsockname()[1])
        _, peer = await accepted
        sock = writer.get_extra_info('socket')
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 4096)
        peer.get_extra_info('socket').setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
        try:
            writer.write(b'x' * (4 << 20))
            self.assertGreater(writer.transport.get_write_buffer_size(), 0)
            await asyncio.wait_for(proxy_bridge.close_writer(writer), 2)
            await asyncio.sleep(0)
            self.assertEqual(sock.fileno(), -1, 'timed-out close retained the TCP socket')
        finally:
            writer.transport.abort(); peer.transport.abort()
            server.close(); await server.wait_closed()


if __name__ == '__main__':
    unittest.main()
