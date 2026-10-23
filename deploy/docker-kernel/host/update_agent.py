#!/usr/bin/env python3
"""Root-owned release installer. Only fixed-repository, checksum-verified releases.
The application can select a version, never a command, path, image or URL.
Activation is fenced by the application's lifetime coordinator lock.
"""
import argparse
import fcntl
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import platform
import re
import shutil
import socket
import socketserver
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.request

REPOSITORY = 'caims481266251/sub2api'
VERSION = re.compile(r'^[0-9]+\.[0-9]+\.[0-9]+$')
MAX_ARCHIVE = 500 * 1024 * 1024


def run(*args):
    return subprocess.check_output(args, stderr=subprocess.PIPE, timeout=180).decode().strip()


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(data)
    return h.hexdigest()


def atomic_json(path, value):
    path = Path(path)
    tmp = path.with_suffix('.new')
    tmp.write_text(json.dumps(value))
    tmp.chmod(0o600)
    os.replace(tmp, path)


def download(url, path, limit):
    request = urllib.request.Request(url, headers={'User-Agent': 'sub2api-runtime-updater'})
    with urllib.request.urlopen(request, timeout=60) as response, open(path, 'wb') as output:
        size = 0
        while True:
            block = response.read(1024 * 1024)
            if not block:
                break
            size += len(block)
            if size > limit:
                raise ValueError('release_asset_too_large')
            output.write(block)


def extract(archive, destination):
    with tarfile.open(archive, 'r:gz') as source:
        members = source.getmembers()
        total = 0
        for member in members:
            path = Path(member.name)
            if path.is_absolute() or '..' in path.parts or not (member.isfile() or member.isdir()):
                raise ValueError('unsafe_release_archive')
            total += member.size
            if total > 2 * MAX_ARCHIVE:
                raise ValueError('expanded_release_too_large')
        source.extractall(destination, members=members)


class LocalHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=5)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class Installer:
    def __init__(self, config):
        self.cfg = config
        self.root = Path(config['releases'])
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.arch = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
        if not self.arch:
            raise ValueError('unsupported_architecture')
        self.lock = threading.Lock()

    def health(self, version):
        conn = LocalHTTP(self.cfg['helper_socket'])
        try:
            conn.request('GET', '/v1/version')
            response = conn.getresponse()
            data = json.loads(response.read(4096))
            return response.status == 200 and data == {'version': version, 'runtime': 'container-v1', 'protocol': 1}
        except (OSError, ValueError, http.client.HTTPException):
            return False
        finally:
            conn.close()

    def validate(self, target, version):
        manifest = json.loads((target / 'runtime-release.json').read_text())
        if manifest['version'] != version or manifest['arch'] != self.arch or manifest['protocol'] != 1:
            raise ValueError('runtime_release_incompatible')
        for name in ('vm-runtime-helper', 'kernel-image.tar'):
            if digest(target / name) != manifest['files'][name]:
                raise ValueError('runtime_release_checksum_mismatch')
        if not re.fullmatch(r'sha256:[a-f0-9]{64}', manifest['image']):
            raise ValueError('invalid_image_digest')
        return manifest

    def prepare(self, version):
        target = self.root / version
        if not target.exists():
            with tempfile.TemporaryDirectory(prefix='.prepare-', dir=self.root) as work:
                work = Path(work)
                name = 'docker-kernel-tools-%s-linux-%s.tar.gz' % (version, self.arch)
                base = 'https://github.com/%s/releases/download/v%s/' % (REPOSITORY, version)
                download(base + name + '.sha256', work / 'checksum', 4096)
                line = (work / 'checksum').read_text().strip().split()
                if len(line) != 2 or line[1] != name or not re.fullmatch('[a-f0-9]{64}', line[0]):
                    raise ValueError('invalid_release_checksum')
                download(base + name, work / 'archive', MAX_ARCHIVE)
                if digest(work / 'archive') != line[0]:
                    raise ValueError('release_checksum_mismatch')
                stage = work / 'stage'
                stage.mkdir()
                extract(work / 'archive', stage)
                self.validate(stage, version)
                os.replace(stage, target)
        manifest = self.validate(target, version)
        run('docker', 'load', '-i', str(target / 'kernel-image.tar'))
        # Docker classic reports a config digest while containerd image stores
        # may report a manifest digest. Bind both to the verified archive before
        # pinning the local engine's immutable ID; never trust a tag alone.
        with tarfile.open(target / 'kernel-image.tar') as archive:
            entries = json.load(archive.extractfile('manifest.json'))
            if len(entries) != 1 or len(entries[0]['RepoTags']) != 1:
                raise ValueError('ambiguous_image_archive')
            image_config = json.load(archive.extractfile(entries[0]['Config']))
            tag = entries[0]['RepoTags'][0]
            expected_tag = 'sub2api-docker-kernel:release-%s-%s' % (version, self.arch)
            if tag != expected_tag:
                raise ValueError('image_tag_mismatch')
        actual = json.loads(run('docker', 'image', 'inspect', tag))[0]
        if actual['Architecture'] != self.arch or actual['Os'] != 'linux':
            raise ValueError('image_architecture_mismatch')
        if actual['RootFS']['Layers'] != image_config['rootfs']['diff_ids'] or actual['Config'] != image_config['config']:
            raise ValueError('loaded_image_content_mismatch')
        if not re.fullmatch(r'sha256:[a-f0-9]{64}', actual['Id']):
            raise ValueError('loaded_image_digest_invalid')
        manifest = dict(manifest, image=actual['Id'])
        # Prove the packaged helper is executable and has the declared version.
        if run(str(target / 'vm-runtime-helper'), '--version') != version:
            raise ValueError('helper_version_mismatch')
        run('docker', 'run', '--rm', '--network=none', '--cap-drop=ALL', '--entrypoint=/bin/true', manifest['image'])
        template = json.loads(Path(self.cfg['templates']).read_text())
        for value in template.values():
            payload = Path(value['payload_dir'])
            files = {str(p): digest(p) for p in payload.rglob('*') if p.is_file()}
            # Never silently bless changed private payloads.
            if not files or files != value['artifacts']:
                raise ValueError('payload_checksum_mismatch')
        return manifest

    def owned_containers(self):
        # Select exact private manifests; never stop unrelated Docker containers.
        rows = []
        for path in Path(self.cfg['instances']).glob('*/manifest.json'):
            state = json.loads(path.read_text())
            name = 'sub2-docker-' + state['instance_id']
            ids = run('docker', 'ps', '-aq', '--filter', 'name=^/' + name + '$')
            if not ids:
                continue
            container = json.loads(run('docker', 'inspect', name))[0]
            labels = container['Config']['Labels']
            if labels.get('sub2.vm.runtime') != 'container-v1' or labels.get('sub2.vm.instance') != state['instance_id'] or labels.get('sub2.vm.account') != str(state['account_id']):
                raise ValueError('container_ownership_conflict')
            rows.append(container['Id'])
        return rows

    def activate(self, version):
        if self.health(version):
            return
        target = self.root / version
        self.validate(target, version)  # Require pre-staged files; no downloads during startup.
        manifest = self.prepare(version)  # Recheck/load image and private payload before stopping anything.
        with open(self.cfg['helper_socket'] + '.coordinator.lock', 'r+') as fence:
            try:
                fcntl.flock(fence, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise ValueError('application_still_running')
            templates_path = Path(self.cfg['templates'])
            unit_path = Path(self.cfg['helper_unit'])
            templates = json.loads(templates_path.read_text())
            previous_templates = templates_path.read_bytes()
            previous_unit = unit_path.read_bytes()
            containers = self.owned_containers()
            journal = self.root / 'activation.json'
            # Durable restore material survives a host interruption.
            atomic_json(journal, {'version': version, 'templates': previous_templates.decode(), 'unit': previous_unit.decode()})
            try:
                for cid in containers:
                    run('docker', 'stop', '--time=40', cid)
                run('systemctl', 'stop', self.cfg['helper_service'])
                for cid in containers:
                    run('docker', 'rm', cid)  # Persistent instance directories/identities retained.
                for template in templates.values():
                    template['runtime'] = 'container-v1'
                    template['image'] = manifest['image']
                atomic_json(templates_path, templates)
                args = [str(target / 'vm-runtime-helper'), '--enabled', '--root', self.cfg['instances'], '--socket', self.cfg['helper_socket'], '--templates', self.cfg['templates'], '--service-uid', str(self.cfg['service_uid']), '--service-gid', str(self.cfg['service_gid'])]
                unit_path.write_text('[Unit]\nDescription=Sub2API Docker runtime helper\nAfter=docker.service\nRequires=docker.service\n[Service]\nExecStart=' + ' '.join(args) + '\nRestart=on-failure\n[Install]\nWantedBy=multi-user.target\n')
                run('systemctl', 'daemon-reload')
                run('systemctl', 'start', self.cfg['helper_service'])
                for _ in range(30):
                    if self.health(version):
                        atomic_json(self.root / 'active.json', {'version': version, 'image': manifest['image']})
                        journal.unlink()
                        return
                    time.sleep(1)
                raise ValueError('helper_health_failed')
            except Exception:
                run('systemctl', 'stop', self.cfg['helper_service'])
                templates_path.write_bytes(previous_templates)
                unit_path.write_bytes(previous_unit)
                run('systemctl', 'daemon-reload')
                run('systemctl', 'start', self.cfg['helper_service'])
                journal.unlink(missing_ok=True)
                raise

    def recover(self):
        journal = self.root / 'activation.json'
        if journal.exists():
            previous = json.loads(journal.read_text())
            run('systemctl', 'stop', self.cfg['helper_service'])
            Path(self.cfg['templates']).write_text(previous['templates'])
            Path(self.cfg['helper_unit']).write_text(previous['unit'])
            run('systemctl', 'daemon-reload')
            run('systemctl', 'start', self.cfg['helper_service'])
            journal.unlink()


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        try:
            length = int(self.headers.get('Content-Length', '0'))
            if not 0 < length <= 256 or self.path not in ('/v1/prepare', '/v1/activate'):
                raise ValueError('invalid_request')
            data = json.loads(self.rfile.read(length))
            if set(data) != {'version'} or not VERSION.fullmatch(data['version']):
                raise ValueError('invalid_version')
            installer = self.server.installer
            with installer.lock:
                getattr(installer, self.path.rsplit('/', 1)[1])(data['version'])
            result, status = {'ok': True, 'version': data['version']}, 200
        except Exception as error:
            # No subprocess output/credentials in public diagnostics.
            code = str(error) if isinstance(error, ValueError) else type(error).__name__
            result, status = {'error': code[:120]}, 409
        raw = json.dumps(result).encode()
        self.send_response(status)
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--config', required=True)
    args = parser.parse_args()
    config = json.loads(Path(args.config).read_text())
    installer = Installer(config)
    installer.recover()
    sock = Path(config['helper_socket'] + '.update')
    sock.unlink(missing_ok=True)
    with Server(str(sock), Handler) as server:
        server.installer = installer
        os.chown(sock, 0, config['service_gid'])
        sock.chmod(0o660)
        server.serve_forever()


if __name__ == '__main__':
    main()
