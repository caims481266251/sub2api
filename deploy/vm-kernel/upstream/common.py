"""Restricted local bridge helpers; never accepts commands from account data."""
import json
import socket
import subprocess
from pathlib import Path


def qmp(command, path='/slot/kvm/qmp.sock'):
    if command not in ('query-kvm', 'query-status'):
        raise ValueError('unsupported_qmp')
    with socket.socket(socket.AF_UNIX) as sock:
        sock.settimeout(5)
        sock.connect(path)
        stream = sock.makefile('rwb', buffering=0)
        json.loads(stream.readline())
        for index, cmd in enumerate(('qmp_capabilities', command)):
            stream.write(json.dumps({'execute': cmd, 'id': index}).encode() + b'\n')
            while True:
                response = json.loads(stream.readline())
                if response.get('id') == index:
                    break
            if 'error' in response:
                raise RuntimeError('qmp_failed')
        return response['return']


def guest(operation):
    if operation not in ('apply', 'observe'):
        raise ValueError('unsupported_guest_operation')
    result = subprocess.run(['/usr/local/bin/kin-guest-exec', '-u', '0', '--',
                             '/usr/local/sbin/sub2-vm-control', operation],
                            capture_output=True, timeout=35)
    if result.returncode:
        raise RuntimeError('guest_control_failed')
    return json.loads(result.stdout)


def observation(operation):
    subprocess.run(['/usr/local/lib/sub2-vm/firewall', '--check'], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    desired = json.loads(Path('/slot/run/instance.json').read_text())
    actual = qmp('query-kvm')
    accel = 'kvm' if actual.get('enabled') is True else 'tcg'
    wanted = Path('/slot/run/accelerator').read_text().strip()
    if accel != wanted or not qmp('query-status').get('running'):
        raise RuntimeError('accelerator_mismatch')
    receipt = guest(operation)
    if receipt['generation'] != desired['generation']:
        raise RuntimeError('guest_generation_mismatch')
    return {**receipt, 'actual_accelerator': accel, 'kvm_enabled': actual.get('enabled') is True,
            'kvm_present': actual.get('present') is True, 'capacity': 0}
