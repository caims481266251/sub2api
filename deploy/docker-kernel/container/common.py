"""Fixed account projection checks; never emit credential or proxy contents."""
import hashlib
import json
from pathlib import Path

# Must match vmkernel.DefaultCodexUserAgent; checked by the release tests.
DEFAULT_CODEX_USER_AGENT = 'codex_cli_rs/0.160.0 (linux x86_64)'

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


def verify_kernel_isolation(status, config, state, machine_id, hostname):
    """Verify local execution evidence; this does not claim upstream identity."""
    fields = dict(line.split(":", 1) for line in status.splitlines() if ":" in line)
    for name in ("Uid", "Gid"):
        if fields.get(name, "").split() != ["10001"] * 4:
            raise ValueError("kernel_user_mismatch")
    for name in ("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"):
        if int(fields.get(name, "-1").strip(), 16) != 0:
            raise ValueError("kernel_capabilities_mismatch")
    if fields.get("NoNewPrivs", "").strip() != "1":
        raise ValueError("kernel_privileges_mismatch")
    if (config.get("device_id") != state["client_identity"]["device_id"]
            or config.get("vm_id") != state["instance_id"]
            or config.get("user_agent") != DEFAULT_CODEX_USER_AGENT
            or config.get("credential_path") != "/slot/credentials/credential.json"
            or config.get("socket_path") != "/slot/run/kernel.sock"
            or config.get("proxy_required") is not True
            or machine_id.strip() != state["identity"]["machine_id"].replace("-", "")
            or hostname != state["identity"]["hostname"]):
        raise ValueError("kernel_identity_mismatch")


def verify_kernel_identity_receipt(health, config):
    # The fixed kernel reports the identity it loaded. This is stronger than a
    # config-file checksum, but still not proof of an upstream HTTP fingerprint.
    if (health.get("device_id") != config.get("device_id")
            or health.get("vm_id") != config.get("vm_id")
            or not health.get("device_id") or not health.get("vm_id")):
        raise ValueError("kernel_identity_receipt_mismatch")
