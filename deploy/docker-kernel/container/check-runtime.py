#!/usr/bin/python3
"""Offline image/payload contract. Never logs credentials or contacts a provider."""
import argparse
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import time

EXPECTED = {
    'centos-stream-9': ('centos', '9'),
    'ubuntu-24.04': ('ubuntu', '24.04'),
    'debian-12': ('debian', '12'),
    'archlinux': ('arch', None),
    'fedora-44': ('fedora', '44'),
}


def check_ca_trust():
    if ssl.create_default_context().get_ca_certs():
        return
    # OpenSSL loads capath entries lazily. Fedora can have a complete hashed
    # default store while get_ca_certs() is empty before the first handshake.
    capath = ssl.get_default_verify_paths().capath
    if capath:
        for path in Path(capath).iterdir():
            if not re.fullmatch(r'[0-9a-fA-F]{8}\.[0-9]+', path.name) or not path.is_file():
                continue
            try:
                probe = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
                probe.load_verify_locations(cafile=str(path))
                if probe.get_ca_certs():
                    return
            except (OSError, ssl.SSLError):
                continue
    raise ValueError('ca_trust_missing')


def check_system(profile):
    expected = EXPECTED[profile]
    values = dict(line.split('=', 1) for line in Path('/etc/os-release').read_text().splitlines()
                  if '=' in line and not line.startswith('#'))
    values = {key: value.strip('"') for key, value in values.items()}
    if values.get('ID') != expected[0] or (expected[1] and values.get('VERSION_ID') != expected[1]):
        raise ValueError('system_profile_mismatch')
    if profile == 'archlinux' and Path('/etc/sub2api-arch-snapshot').read_text().strip() != '2026/10/25':
        raise ValueError('arch_snapshot_mismatch')
    if sys.version_info < (3, 9):
        raise ValueError('python_version_unsupported')
    for executable in ('setpriv', 'nft'):
        if not shutil.which(executable):
            raise ValueError('runtime_dependency_missing')
    check_ca_trust()
    # Invokes native utilities but requires no NET_ADMIN or network access.
    subprocess.run(['/usr/bin/setpriv', '--version'], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run([shutil.which('nft'), '--version'], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def check_payload():
    kernel = Path('/opt/kin-payload/usr/local/bin/kin-codex-kernel')
    if not kernel.is_file() or not os.access(kernel, os.X_OK):
        raise ValueError('payload_missing')
    with tempfile.TemporaryDirectory(prefix='sub2-profile-probe-') as directory:
        root = Path(directory)
        credentials = root / 'credentials.json'
        credentials.write_text(json.dumps({'accounts': []}))
        config = {'vm_id': 'profile-probe', 'device_id': '11111111-1111-4111-8111-111111111111',
                  'socket_path': str(root / 'kernel.sock'), 'credential_path': str(credentials),
                  'proxy_required': False, 'proxy_url': '', 'internal_token': 'profile-probe-token',
                  'upstream_transport': 'prefer_websocket', 'client_features': False,
                  'analytics': False, 'test_endpoints': False}
        target = root / 'config.json'
        target.write_text(json.dumps(config))
        target.chmod(0o600)
        credentials.chmod(0o600)
        env = {key: value for key, value in os.environ.items()
               if key.lower() not in ('http_proxy', 'https_proxy', 'all_proxy', 'no_proxy')
               and not key.startswith('KIN_CODEX')}
        process = subprocess.Popen([str(kernel), str(target)], env=env,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            deadline = time.monotonic() + 10
            while not Path(config['socket_path']).exists():
                if process.poll() is not None:
                    raise ValueError('payload_start_failed')
                if time.monotonic() > deadline:
                    raise ValueError('payload_start_timeout')
                time.sleep(0.05)
            connection = http.client.HTTPConnection('localhost', timeout=3)
            connection.sock = socket.socket(socket.AF_UNIX)
            connection.sock.settimeout(3)
            connection.sock.connect(config['socket_path'])
            try:
                connection.request('GET', '/internal/health', headers={'x-kin-internal-token': config['internal_token'], 'x-internal-token': config['internal_token']})
                response = connection.getresponse()
                if response.status != 200:
                    raise ValueError('payload_health_failed')
                health = json.loads(response.read(65536))
                if health.get('healthy', health.get('ok', False)) is not True:
                    raise ValueError('payload_not_healthy')
            finally:
                connection.close()
        finally:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=3)


def check_network_tools():
    # Run only in a disposable --network=none container with CAP_NET_ADMIN.
    # This proves native Netlink support; emulation --version is insufficient.
    command = [shutil.which('nft')]
    subprocess.run(command + ['add', 'table', 'inet', 'sub2_profile_probe'], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    subprocess.run(command + ['delete', 'table', 'inet', 'sub2_profile_probe'], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--profile', choices=EXPECTED, required=True)
    parser.add_argument('--payload', action='store_true')
    parser.add_argument('--network-check', action='store_true')
    args = parser.parse_args()
    try:
        check_system(args.profile)
        if args.network_check:
            check_network_tools()
        if args.payload:
            check_payload()
        print(json.dumps({'ok': True, 'profile_id': args.profile}))
    except Exception as error:
        code = str(error) if isinstance(error, ValueError) else type(error).__name__
        print(json.dumps({'ok': False, 'profile_id': args.profile, 'error': code}))
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
