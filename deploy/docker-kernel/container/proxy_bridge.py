#!/usr/bin/python3
"""Account-local SOCKS relay. Retries stop once the upstream tunnel is established."""
import asyncio
import errno
import ipaddress
import itertools
import json
import os
import socket
from pathlib import Path
import sys
import time
from urllib.parse import unquote, urlsplit

LISTEN_HOST, LISTEN_PORT = '127.0.0.1', 1081
HANDSHAKE_TIMEOUT, MAX_ATTEMPTS = 8, 3
RETRY_DELAYS = (0.15, 0.4)
FAIL_BIND = b'\x01\x00\x00\x00\x00\x00\x00'


def diagnostic(event, stage, attempt, reason=None, reply_code=None):
    # Never include proxy addresses, credentials, targets, exception text or payloads.
    fields = dict(event=event, stage=stage, attempt=attempt, at=time.time())
    if reason: fields['reason'] = reason
    if reply_code is not None: fields['reply_code'] = reply_code
    try: print('proxy_bridge ' + json.dumps(fields, separators=(',', ':')), file=sys.stderr, flush=True)
    except OSError: pass


class HandshakeFailure(Exception):
    def __init__(self, reason, retryable=False, code=1):
        self.reason, self.retryable, self.code = reason, retryable, code


async def read_address(reader, atyp=None):
    if atyp is None: atyp = (await reader.readexactly(1))[0]
    if atyp == 1: value = await reader.readexactly(4)
    elif atyp == 4: value = await reader.readexactly(16)
    elif atyp == 3:
        size = await reader.readexactly(1)
        if size == b'\x00': raise HandshakeFailure('invalid_domain')
        value = size + await reader.readexactly(size[0])
    else: raise HandshakeFailure('invalid_address', code=8)
    return bytes([atyp]) + value + await reader.readexactly(2)


async def close_writer(writer):
    if writer is None: return
    writer.close()
    try: await asyncio.wait_for(writer.wait_closed(), 1)
    except (OSError, asyncio.TimeoutError): pass


class ProxyBridge:
    def __init__(self, proxy_url, ips, timeout=HANDSHAKE_TIMEOUT, delays=RETRY_DELAYS):
        parsed = urlsplit(proxy_url)
        if parsed.scheme not in ('socks5', 'socks5h') or not parsed.hostname or not parsed.port:
            raise ValueError('upstream_socks_url_invalid')
        self.host, self.port = parsed.hostname, parsed.port
        self.ips = [str(ipaddress.IPv4Address(ip)) for ip in ips]
        if not self.ips: raise ValueError('upstream_socks_address_missing')
        self.username, self.password = unquote(parsed.username or '').encode(), unquote(parsed.password or '').encode()
        self.auth = parsed.username is not None
        if self.auth and not (1 <= len(self.username) <= 255 and 1 <= len(self.password) <= 255):
            raise ValueError('upstream_socks_credentials_invalid')
        self.timeout, self.delays = timeout, delays
        self._connections = itertools.count()

    async def _connect(self, target, attempt, state):
        state['stage'] = 'proxy_tcp'
        reader, writer = await asyncio.open_connection(self.ips[state['proxy_ip_index']], self.port)
        upstream_socket = writer.get_extra_info('socket')
        if upstream_socket is not None:
            upstream_socket.setsockopt(socket.SOL_SOCKET, socket.SO_KEEPALIVE, 1)
            for option, value in ((getattr(socket, 'TCP_KEEPIDLE', None), 60),
                                  (getattr(socket, 'TCP_KEEPINTVL', None), 20),
                                  (getattr(socket, 'TCP_KEEPCNT', None), 3)):
                if option is not None:
                    upstream_socket.setsockopt(socket.IPPROTO_TCP, option, value)
        state['writer'] = writer
        state['stage'] = 'socks_method'
        method = b'\x02' if self.auth else b'\x00'
        writer.write(b'\x05\x01' + method); await writer.drain()
        if await reader.readexactly(2) != b'\x05' + method:
            raise HandshakeFailure('auth_method_rejected', code=2)
        if self.auth:
            state['stage'] = 'socks_auth'
            writer.write(b'\x01' + bytes([len(self.username)]) + self.username + bytes([len(self.password)]) + self.password)
            await writer.drain()
            if await reader.readexactly(2) != b'\x01\x00': raise HandshakeFailure('auth_rejected', code=2)
        state['stage'] = 'socks_connect'
        writer.write(b'\x05\x01\x00' + target); await writer.drain()
        version, code, reserved, atyp = await reader.readexactly(4)
        if version != 5 or reserved != 0: raise HandshakeFailure('invalid_reply')
        # SOCKS reply 5 means the destination actively refused the connection;
        # repeating it will not repair an intermittent proxy transport failure.
        # General/network/host unreachable replies may be transient while the
        # proxy route is recovering, and are still retried only before payload.
        if code: raise HandshakeFailure('connect_rejected', retryable=code in (1, 3, 4), code=code)
        bound = await read_address(reader, atyp)
        return reader, writer, b'\x05\x00\x00' + bound

    async def upstream(self, target):
        start = next(self._connections) % len(self.ips)
        for attempt in range(1, MAX_ATTEMPTS + 1):
            state = {'stage': 'proxy_tcp', 'writer': None, 'proxy_ip_index': (start + attempt - 1) % len(self.ips)}
            try:
                result = await asyncio.wait_for(self._connect(target, attempt, state), self.timeout)
                if attempt > 1: diagnostic('recovered', state['stage'], attempt)
                return result
            except asyncio.CancelledError:
                await close_writer(state['writer']); raise
            except (HandshakeFailure, OSError, asyncio.IncompleteReadError, asyncio.TimeoutError) as error:
                await close_writer(state['writer'])
                if isinstance(error, HandshakeFailure): failure = error
                elif isinstance(error, asyncio.IncompleteReadError): failure = HandshakeFailure('peer_closed', retryable=True)
                elif isinstance(error, asyncio.TimeoutError): failure = HandshakeFailure('timeout', retryable=True)
                else:
                    reason = {errno.ECONNRESET:'connection_reset', errno.ECONNREFUSED:'connection_refused', errno.EPIPE:'broken_pipe', errno.ENETUNREACH:'network_unreachable', errno.EHOSTUNREACH:'host_unreachable', errno.ETIMEDOUT:'timeout'}.get(error.errno, 'socket_error')
                    failure = HandshakeFailure(reason, retryable=error.errno in (errno.ECONNRESET, errno.ECONNREFUSED, errno.EPIPE, errno.ENETUNREACH, errno.EHOSTUNREACH, errno.ETIMEDOUT, errno.ECONNABORTED))
                retry = failure.retryable and attempt < MAX_ATTEMPTS
                diagnostic('retry' if retry else 'failed', state['stage'], attempt, failure.reason, failure.code)
                if not retry: raise failure from None
                await asyncio.sleep(self.delays[min(attempt - 1, len(self.delays) - 1)])
        raise HandshakeFailure('attempts_exhausted')

    @staticmethod
    async def relay(left_reader, left_writer, right_reader, right_writer):
        async def copy(reader, writer):
            while True:
                data = await reader.read(65536)
                if not data:
                    if writer.can_write_eof(): writer.write_eof(); await writer.drain()
                    return
                writer.write(data); await writer.drain()
        tasks = [asyncio.create_task(copy(left_reader, right_writer)), asyncio.create_task(copy(right_reader, left_writer))]
        try: await asyncio.gather(*tasks)
        except OSError: diagnostic('closed', 'tunnel', 1, 'socket_error_no_replay')
        finally:
            for task in tasks: task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)

    async def handle(self, reader, writer):
        remote = None
        try:
            async def negotiate():
                version, count = await reader.readexactly(2)
                if version != 5 or count == 0: return None
                methods = await reader.readexactly(count)
                if 0 not in methods: writer.write(b'\x05\xff'); await writer.drain(); return None
                writer.write(b'\x05\x00'); await writer.drain()
                version, command, reserved = await reader.readexactly(3)
                if version != 5 or reserved != 0 or command != 1: raise HandshakeFailure('unsupported_command', code=7)
                return await read_address(reader)
            target = await asyncio.wait_for(negotiate(), self.timeout)
            if target is None: return
            remote_reader, remote, reply = await self.upstream(target)
            writer.write(reply); await writer.drain()
            # Only an established SOCKS tunnel gets application bytes. Never retry past this line.
            await self.relay(reader, writer, remote_reader, remote)
        except HandshakeFailure as error:
            writer.write(b'\x05' + bytes([error.code]) + b'\x00' + FAIL_BIND)
            try: await writer.drain()
            except OSError: pass
        except (OSError, asyncio.IncompleteReadError, asyncio.TimeoutError): pass
        finally:
            await close_writer(writer); await close_writer(remote)

    async def serve(self, ready_path):
        server = await asyncio.start_server(self.handle, LISTEN_HOST, LISTEN_PORT)
        Path(ready_path).write_text(str(os.getpid()), encoding='ascii')
        Path('/tmp/sub2-proxy-bridge.ready').write_text('ready', encoding='ascii')
        async with server: await server.serve_forever()


def main():
    if len(sys.argv) != 2: raise ValueError('proxy_bridge_usage')
    config = json.loads(Path('/slot/credentials/proxy.json').read_text())
    bridge = ProxyBridge(config['url'], config['ips'])
    if os.geteuid() == 0: os.setgroups([]); os.setgid(10001); os.setuid(10001)
    asyncio.run(bridge.serve(sys.argv[1]))


if __name__ == '__main__':
    try: main()
    except Exception as error: raise SystemExit('proxy_bridge_start_failed kind=' + type(error).__name__)
