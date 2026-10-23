"""Fixed account projection checks; never emit credential or proxy contents."""
import hashlib
import json
from pathlib import Path


def read(path):
    return json.loads(Path(path).read_text())


def projection():
    state = read('/slot/run/instance.json')
    revision = read('/slot/credentials/revision.json')
    proxy = read('/slot/credentials/proxy.json')
    credential = Path('/slot/credentials/credential.json').read_bytes()
    digest = hashlib.sha256(credential).hexdigest()
    if digest != revision['digest'] or revision['revision'] != state['desired_credential_revision'] or proxy['revision'] != state['spec']['proxy_revision']:
        raise ValueError('projection_mismatch')
    return state, proxy, {'generation': state['generation'], 'credential_revision': revision['revision'],
                          'proxy_revision': proxy['revision'], 'credential_digest': digest,
                          'device_id': state['client_identity']['device_id']}


def process_identity():
    # PID 1 is exec'd into the fixed kernel, not an idle supervisor. Its start
    # time is paired with Docker container ID/StartedAt by the privileged helper.
    argv = Path('/proc/1/cmdline').read_bytes().split(b'\0')
    kernel = b'/opt/kin-payload/usr/local/bin/kin-codex-kernel'
    native = argv[0] == kernel
    # Linux binfmt may retain its fixed native interpreter as argv[0].
    # Require the exact payload immediately after it, never a substring match.
    emulated = argv[0] in (b'/usr/bin/qemu-x86_64', b'/usr/bin/qemu-x86_64-static', b'/run/rosetta/rosetta') and len(argv) > 1 and argv[1] == kernel
    if not (native or emulated):
        raise ValueError('wrong_kernel_process')
    stat = Path('/proc/1/stat').read_text().rsplit(')', 1)[1].split()
    return stat[19]
