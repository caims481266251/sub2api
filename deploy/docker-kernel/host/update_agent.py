#!/usr/bin/env python3
"""Root-owned release installer. Only fixed-repository, checksum-verified releases.
The application can select a version, never a command, path, image or URL.
Activation is fenced by the application's lifetime coordinator lock.
"""
import argparse
import copy
from contextlib import contextmanager
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
import stat
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.request
import uuid

REPOSITORY = 'caims481266251/sub2api'
VERSION = re.compile(r'^[0-9]+\.[0-9]+\.[0-9]+$')
UPDATER_VERSION = 2
PROFILES = ('centos-stream-9', 'ubuntu-24.04', 'debian-12', 'archlinux', 'fedora-44')
MAX_ARCHIVE = 2 * 1024 * 1024 * 1024
MAX_EXPANDED = 6 * 1024 * 1024 * 1024
DISK_RESERVE = 512 * 1024 * 1024
DEFAULT_SCRIPTS = ('entry', 'common.py', 'observe', 'firewall', 'proxy_bridge.py')
DEFAULT_FINGERPRINT = '''import hashlib,json,shlex
from pathlib import Path
release={}
for line in Path('/etc/os-release').read_text().splitlines():
    if '=' in line and not line.startswith('#'):
        key,value=line.split('=',1)
        parsed=shlex.split(value)
        release[key]=parsed[0] if parsed else ''
files=('entry','common.py','observe','firewall','proxy_bridge.py')
print(json.dumps({'os_release':{key:release.get(key,'') for key in ('ID','VERSION_ID')},'runtime_scripts':{name:hashlib.sha256((Path('/usr/local/lib/sub2-container')/name).read_bytes()).hexdigest() for name in files}}))
'''


def run(*args, timeout=180):
    return subprocess.check_output(args, stderr=subprocess.PIPE, timeout=timeout).decode().strip()


def image_probe(*args):
    name = 'sub2-update-check-' + uuid.uuid4().hex
    try:
        return run('docker', 'run', '--rm', '--name', name, '--pull=never', '--network=none', *args, timeout=35)
    finally:
        try:
            run('docker', 'rm', '-f', name, timeout=10)
        except Exception:
            pass  # Only this disposable probe is eligible for cleanup.


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(data)
    return h.hexdigest()


def atomic_bytes(path, value, mode=0o600):
    path = Path(path)
    with tempfile.NamedTemporaryFile(prefix='.' + path.name + '-', dir=path.parent, delete=False) as stream:
        tmp = Path(stream.name)
        try:
            os.fchmod(stream.fileno(), mode)
            stream.write(value)
            stream.flush()
            os.fsync(stream.fileno())
            os.replace(tmp, path)
            directory = os.open(path.parent, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            tmp.unlink(missing_ok=True)


def atomic_json(path, value):
    atomic_bytes(path, json.dumps(value).encode())


def image_tag(version, arch, profile='default'):
    suffix = '' if profile == 'default' else '-' + profile
    return 'sub2api-docker-kernel:release-%s-%s%s' % (version, arch, suffix)


def release_images(manifest):
    if manifest.get('manifest_version', 1) == 1:
        return {'default': {'image': manifest['image'], 'tag': image_tag(manifest['version'], manifest['arch'])}}
    return manifest['images']


def normalized_image_config(config):
    # Classic Docker inspect fills optional OCI fields with zero values.
    # Ignore only known empty defaults; preserve every nonempty/unknown field.
    normalized = dict(config)
    defaults = {
        'Hostname': '', 'Domainname': '', 'User': '', 'Image': '', 'WorkingDir': '',
        'AttachStdin': False, 'AttachStdout': False, 'AttachStderr': False,
        'Tty': False, 'OpenStdin': False, 'StdinOnce': False,
        'Cmd': None, 'OnBuild': None, 'Volumes': None,
    }
    for key, value in defaults.items():
        if key in normalized and type(normalized[key]) is type(value) and normalized[key] == value:
            del normalized[key]
    return normalized


def payload_template(directory):
    payload = Path(directory)
    if not payload.is_absolute() or payload.is_symlink() or not payload.is_dir():
        raise ValueError('payload_missing')
    files = {}
    for path in payload.rglob('*'):
        if path.is_symlink():
            raise ValueError('payload_symlink_forbidden')
        if path.is_dir():
            continue
        if not stat.S_ISREG(path.stat().st_mode):
            raise ValueError('unmanifested_payload')
        files[str(path)] = digest(path)
    if not files or str(payload / 'usr/local/bin/kin-codex-kernel') not in files:
        raise ValueError('payload_missing')
    return {'runtime': 'container-v1', 'payload_dir': str(payload), 'artifacts': files}


def require_space(path, needed):
    if shutil.disk_usage(path).free < needed + DISK_RESERVE:
        raise ValueError('insufficient_release_disk_space')


def download(url, path, limit):
    request = urllib.request.Request(url, headers={'User-Agent': 'sub2api-runtime-updater'})
    with urllib.request.urlopen(request, timeout=60) as response, open(path, 'wb') as output:
        advertised = response.headers.get('Content-Length')
        if advertised and (not advertised.isdigit() or int(advertised) > limit):
            raise ValueError('release_asset_too_large')
        require_space(Path(path).parent, int(advertised) if advertised else min(limit, 16 * 1024 * 1024))
        size = 0
        while True:
            block = response.read(1024 * 1024)
            if not block:
                break
            size += len(block)
            if size > limit:
                raise ValueError('release_asset_too_large')
            require_space(Path(path).parent, len(block))
            output.write(block)


def extract(archive, destination):
    with tarfile.open(archive, 'r:gz') as source:
        members = source.getmembers()
        if len(members) > 50000:
            raise ValueError('expanded_release_too_large')
        total = 0
        seen = set()
        for member in members:
            path = Path(member.name)
            if path.is_absolute() or '..' in path.parts or not (member.isfile() or member.isdir()):
                raise ValueError('unsafe_release_archive')
            normalized = str(path)
            if normalized in seen:
                raise ValueError('duplicate_release_member')
            seen.add(normalized)
            total += member.size
            if total > MAX_EXPANDED:
                raise ValueError('expanded_release_too_large')
        require_space(destination, total)
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

    @contextmanager
    def operation(self, wait=False):
        # Root bootstrap and HTTP requests share the same mutation fence.
        with self.lock, open(self.root / '.operation.lock', 'a+') as fence:
            os.fchmod(fence.fileno(), 0o600)
            try:
                fcntl.flock(fence, fcntl.LOCK_EX | (0 if wait else fcntl.LOCK_NB))
            except BlockingIOError:
                raise ValueError('runtime_update_in_progress')
            yield

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
        schema = manifest.get('manifest_version', 1)
        minimum = manifest.get('minimum_updater', 1)
        if type(schema) is not int or schema not in (1, 2) or type(minimum) is not int or minimum > UPDATER_VERSION:
            raise ValueError('runtime_updater_bootstrap_required')
        if schema == 2:
            profiles = PROFILES if self.arch == 'amd64' else ('ubuntu-24.04', 'debian-12')
            if not isinstance(manifest.get('images'), dict) or set(manifest['images']) != {'default', *profiles}:
                raise ValueError('runtime_image_catalog_incomplete')
            if manifest['image'] != manifest['images']['default']['image']:
                raise ValueError('runtime_default_image_mismatch')
        for name in ('vm-runtime-helper', 'kernel-image.tar'):
            if (target / name).is_symlink() or digest(target / name) != manifest['files'][name]:
                raise ValueError('runtime_release_checksum_mismatch')
        for profile, image in release_images(manifest).items():
            if not re.fullmatch(r'sha256:[a-f0-9]{64}', image['image']):
                raise ValueError('invalid_image_digest')
            if image['tag'] != image_tag(version, self.arch, profile):
                raise ValueError('image_tag_mismatch')
        return manifest

    def prepare(self, version, initial_payload=None):
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
                inventory = {'files': [], 'directories': []}
                for path in stage.rglob('*'):
                    inventory['directories' if path.is_dir() else 'files'].append(str(path.relative_to(stage)))
                # Only files proven to belong to this downloaded package may be
                # removed later; pre-existing release folders remain untouched.
                atomic_json(stage / '.release-files.json', inventory)
                os.replace(stage, target)
        manifest = self.validate(target, version)
        manifest.pop('refresh_default', None)  # Never trust this internal decision in a public manifest.
        # Docker classic reports a config digest while containerd image stores
        # may report a manifest digest. Bind both to the verified archive before
        # pinning the local engine's immutable ID; never trust a tag alone.
        expected = release_images(manifest)
        configs = {}
        with tarfile.open(target / 'kernel-image.tar') as archive:
            members = archive.getmembers()
            if len(members) > 100000 or len({item.name for item in members}) != len(members):
                raise ValueError('ambiguous_image_archive')
            def read_bytes(name, limit):
                path = Path(name)
                try:
                    member = archive.getmember(name)
                except KeyError:
                    raise ValueError('image_digest_mismatch')
                if path.is_absolute() or '..' in path.parts or not member.isfile() or member.size > limit:
                    raise ValueError('unsafe_image_archive')
                return archive.extractfile(member).read()
            def bind_digest(declared, config_digest, depth=0):
                if declared == config_digest:
                    return
                if depth >= 4 or not re.fullmatch(r'sha256:[a-f0-9]{64}', declared):
                    raise ValueError('image_digest_mismatch')
                raw = read_bytes('blobs/sha256/' + declared[7:], 4 * 1024 * 1024)
                if 'sha256:' + hashlib.sha256(raw).hexdigest() != declared:
                    raise ValueError('image_digest_mismatch')
                descriptor = json.loads(raw)
                if descriptor.get('schemaVersion') != 2:
                    raise ValueError('image_digest_mismatch')
                if descriptor.get('mediaType') in ('application/vnd.oci.image.manifest.v1+json', 'application/vnd.docker.distribution.manifest.v2+json'):
                    if descriptor.get('config', {}).get('digest') != config_digest:
                        raise ValueError('image_digest_mismatch')
                    return
                if descriptor.get('mediaType') not in ('application/vnd.oci.image.index.v1+json', 'application/vnd.docker.distribution.manifest.list.v2+json'):
                    raise ValueError('image_digest_mismatch')
                choices = descriptor.get('manifests', [])
                if not isinstance(choices, list) or len(choices) > 32:
                    raise ValueError('image_digest_mismatch')
                native = [item for item in choices if item.get('platform', {}).get('os') == 'linux' and item.get('platform', {}).get('architecture') == self.arch]
                if len(native) != 1:
                    raise ValueError('image_digest_mismatch')
                bind_digest(native[0]['digest'], config_digest, depth + 1)
            entries = json.loads(read_bytes('manifest.json', 1024 * 1024))
            if not isinstance(entries, list) or not 0 < len(entries) <= len(expected):
                raise ValueError('ambiguous_image_archive')
            expected_tags = {image['tag'] for image in expected.values()}
            expected_by_tag = {image['tag']: image['image'] for image in expected.values()}
            for entry in entries:
                config_raw = read_bytes(entry['Config'], 4 * 1024 * 1024)
                image_config = json.loads(config_raw)
                config_digest = 'sha256:' + hashlib.sha256(config_raw).hexdigest()
                for tag in entry['RepoTags']:
                    if tag not in expected_tags or tag in configs:
                        raise ValueError('image_tag_mismatch')
                    bind_digest(expected_by_tag[tag], config_digest)
                    configs[tag] = image_config
            if set(configs) != expected_tags:
                raise ValueError('image_archive_incomplete')
        docker_root = run('docker', 'info', '--format', '{{.DockerRootDir}}')
        if not docker_root or not Path(docker_root).is_absolute():
            raise ValueError('docker_storage_unavailable')
        require_space(docker_root, (target / 'kernel-image.tar').stat().st_size)
        docker_arch = run('docker', 'info', '--format', '{{.Architecture}}')
        if {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(docker_arch, docker_arch) != self.arch:
            raise ValueError('image_host_architecture_mismatch')
        run('docker', 'load', '-i', str(target / 'kernel-image.tar'))
        resolved = {}
        for profile, image in expected.items():
            actual = json.loads(run('docker', 'image', 'inspect', image['tag']))[0]
            image_config = configs[image['tag']]
            if actual['Architecture'] != self.arch or actual['Os'] != 'linux':
                raise ValueError('image_architecture_mismatch')
            if actual['RootFS']['Layers'] != image_config['rootfs']['diff_ids'] or normalized_image_config(actual['Config']) != normalized_image_config(image_config['config']):
                raise ValueError('loaded_image_content_mismatch')
            if not re.fullmatch(r'sha256:[a-f0-9]{64}', actual['Id']):
                raise ValueError('loaded_image_digest_invalid')
            resolved[profile] = dict(image, image=actual['Id'])
        manifest = dict(manifest, image=resolved['default']['image'])
        if manifest.get('manifest_version', 1) == 2:
            manifest['images'] = resolved
        # Prove the packaged helper is executable and has the declared version.
        if run(str(target / 'vm-runtime-helper'), '--version') != version:
            raise ValueError('helper_version_mismatch')
        image_probe('--cap-drop=ALL', '--security-opt=no-new-privileges', '--entrypoint=/bin/true', manifest['image'])
        templates_path = Path(self.cfg['templates'])
        templates = json.loads(templates_path.read_text()) if templates_path.exists() else {'default': payload_template(initial_payload)} if initial_payload else {}
        if 'default' not in templates:
            raise ValueError('default_payload_template_missing')
        for value in templates.values():
            files = payload_template(value['payload_dir'])['artifacts']
            # Never silently bless changed private payloads.
            if not files or files != value['artifacts']:
                raise ValueError('payload_checksum_mismatch')
        self.assess_default_refresh(manifest, templates)
        for profile, image in resolved.items():
            if profile == 'default':
                continue
            value = templates.get(profile, templates['default'])
            image_probe('--cap-drop=ALL', '--cap-add=NET_ADMIN', '--security-opt=no-new-privileges', '--entrypoint=/usr/bin/python3',
                        image['image'], '/usr/local/lib/sub2-container/check-runtime.py', '--profile', profile, '--network-check')
            raw = image_probe('--user=10001:10001', '--cap-drop=ALL', '--security-opt=no-new-privileges',
                '--mount', 'type=bind,src=' + value['payload_dir'] + ',dst=/opt/kin-payload,readonly', '--entrypoint=/usr/bin/python3',
                image['image'], '/usr/local/lib/sub2-container/check-runtime.py', '--profile', profile, '--payload')
            result = json.loads(raw)
            if result.get('ok') is not True or result.get('profile_id') != profile:
                raise ValueError('profile_probe_invalid')
        atomic_json(target / '.prepared.json', {'managed_by': 'sub2api-runtime-updater-v2', 'version': version, 'images': resolved})
        return manifest

    def default_fingerprint(self, image):
        if not re.fullmatch(r'sha256:[a-f0-9]{64}', image):
            raise ValueError('default_image_digest_invalid')
        try:
            actual = json.loads(run('docker', 'image', 'inspect', image))[0]
        except (subprocess.SubprocessError, ValueError, IndexError):
            raise ValueError('default_image_unavailable')
        if actual.get('Architecture') != self.arch or actual.get('Os') != 'linux':
            raise ValueError('default_image_architecture_mismatch')
        try:
            result = json.loads(image_probe('--user=10001:10001', '--cap-drop=ALL', '--security-opt=no-new-privileges',
                                            '--entrypoint=/usr/bin/python3', image, '-c', DEFAULT_FINGERPRINT))
            scripts = result['runtime_scripts']
            system = result['os_release']
            if set(scripts) != set(DEFAULT_SCRIPTS) or any(not re.fullmatch(r'[a-f0-9]{64}', value) for value in scripts.values()):
                raise ValueError('default_image_probe_invalid')
            if set(system) != {'ID', 'VERSION_ID'} or not all(isinstance(value, str) and value for value in system.values()):
                raise ValueError('default_image_probe_invalid')
            return result
        except (subprocess.SubprocessError, ValueError, KeyError, TypeError):
            raise ValueError('default_image_probe_failed')

    def assess_default_refresh(self, manifest, templates):
        manifest['refresh_default'] = False
        previous = templates['default'].get('image')
        if manifest.get('manifest_version', 1) != 2 or not previous or previous == manifest['image']:
            return
        before = self.default_fingerprint(previous)
        after = self.default_fingerprint(manifest['image'])
        if before['runtime_scripts'] == after['runtime_scripts']:
            return
        if before['os_release'] != after['os_release']:
            # Some fresh system-pool installations used a pool image as the
            # legacy default. Refresh that alias only from an archive-verified
            # same-system image carrying the exact new shared runtime scripts.
            for profile in PROFILES:
                candidate = manifest.get('images', {}).get(profile)
                if not candidate:
                    continue
                fingerprint = self.default_fingerprint(candidate['image'])
                if fingerprint['os_release'] == before['os_release'] and fingerprint['runtime_scripts'] == after['runtime_scripts']:
                    manifest['image'] = candidate['image']
                    manifest['images']['default'] = dict(manifest['images']['default'], image=candidate['image'])
                    break
            else:
                raise ValueError('default_system_migration_forbidden')
        manifest['refresh_default'] = True

    def owned_containers(self, changed):
        # Select exact private manifests; never stop unrelated Docker containers.
        rows = []
        for path in Path(self.cfg['instances']).glob('*/manifest.json'):
            state = json.loads(path.read_text())
            if (state.get('spec', {}).get('template') or 'default') not in changed:
                continue
            if not re.fullmatch(r'[a-zA-Z0-9_-]+', state['instance_id']):
                raise ValueError('container_ownership_conflict')
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

    def active_matches(self, version):
        active_path = self.root / 'active.json'
        if not active_path.exists():
            return False
        active = json.loads(active_path.read_text())
        templates = json.loads(Path(self.cfg['templates']).read_text())
        return active.get('version') == version and active.get('images') == {key: value['image'] for key, value in templates.items()}

    def next_templates(self, templates, manifest):
        updated = copy.deepcopy(templates)
        for profile, image in release_images(manifest).items():
            # Pool-only activation retains default. An offline-proven same-OS
            # runtime script repair uses the ordinary drained image update.
            if profile == 'default' and manifest.get('manifest_version', 1) == 2 and profile in updated and not manifest.get('refresh_default', False):
                continue
            value = updated.setdefault(profile, copy.deepcopy(templates['default']))
            value.update(runtime='container-v1', image=image['image'])
        return updated

    def stop_helper(self):
        try:
            run('systemctl', 'stop', self.cfg['helper_service'])
        except subprocess.CalledProcessError:
            # A fresh install has no helper unit yet. Only a confirmed absent
            # unit is safe to skip; retain the original stop error otherwise.
            load_state = None
            try:
                load_state = run('systemctl', 'show', '--property=LoadState', self.cfg['helper_service'])
            except (OSError, subprocess.SubprocessError):
                pass
            if load_state != 'LoadState=not-found':
                raise

    def restore(self, previous):
        self.stop_helper()
        for name, path, mode in (('templates', self.cfg['templates'], 0o600), ('unit', self.cfg['helper_unit'], 0o644), ('active', self.root / 'active.json', 0o600)):
            if name == 'active' and name not in previous:
                continue  # A v1 journal predates active metadata snapshots.
            value = previous.get(name)
            if value is None:
                Path(path).unlink(missing_ok=True)
            else:
                atomic_bytes(path, value.encode(), mode)
        run('systemctl', 'daemon-reload')
        if previous.get('unit') is not None:
            run('systemctl', 'start', self.cfg['helper_service'])

    def activate(self, version):
        if self.health(version) and self.active_matches(version):
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
            updated = self.next_templates(templates, manifest)
            changed = {key for key, value in updated.items() if templates.get(key, {}).get('image') != value['image']}
            containers = self.owned_containers(changed)
            journal = self.root / 'activation.json'
            # Durable restore material survives a host interruption.
            active_path = self.root / 'active.json'
            previous = {'version': version, 'templates': templates_path.read_text(), 'unit': unit_path.read_text() if unit_path.exists() else None, 'active': active_path.read_text() if active_path.exists() else None}
            atomic_json(journal, previous)
            try:
                for cid in containers:
                    run('docker', 'stop', '--time=40', cid)
                self.stop_helper()
                for cid in containers:
                    run('docker', 'rm', cid)  # Persistent instance directories/identities retained.
                atomic_json(templates_path, updated)
                args = [str(target / 'vm-runtime-helper'), '--enabled', '--root', self.cfg['instances'], '--socket', self.cfg['helper_socket'], '--templates', self.cfg['templates'], '--service-uid', str(self.cfg['service_uid']), '--service-gid', str(self.cfg['service_gid'])]
                atomic_bytes(unit_path, ('[Unit]\nDescription=Sub2API Docker runtime helper\nAfter=docker.service\nRequires=docker.service\n[Service]\nExecStart=' + ' '.join(args) + '\nRestart=on-failure\n[Install]\nWantedBy=multi-user.target\n').encode(), 0o644)
                run('systemctl', 'daemon-reload')
                run('systemctl', 'start', self.cfg['helper_service'])
                for _ in range(30):
                    if self.health(version):
                        prior = json.loads(previous['active']) if previous['active'] else {}
                        rollback_version = prior.get('version') if prior.get('version') != version else prior.get('rollback_version')
                        active = {'version': version, 'image': updated['default']['image'], 'images': {key: value['image'] for key, value in updated.items()}, 'rollback_version': rollback_version, 'rollback_images': {key: value['image'] for key, value in templates.items()}}
                        atomic_json(active_path, active)
                        journal.unlink()
                        try:
                            self.cleanup(active)
                        except Exception as error:
                            # Cleanup is best effort; retain a root-only signal
                            # without exposing subprocess output or credentials.
                            try:
                                atomic_json(self.root / 'cleanup-status.json', {'version': version, 'deferred': True, 'error': type(error).__name__})
                            except OSError:
                                pass
                        return
                    time.sleep(1)
                raise ValueError('helper_health_failed')
            except Exception:
                self.restore(previous)
                journal.unlink(missing_ok=True)
                raise

    def recover(self):
        journal = self.root / 'activation.json'
        if journal.exists():
            with open(self.cfg['helper_socket'] + '.coordinator.lock', 'r+') as fence:
                try:
                    fcntl.flock(fence, fcntl.LOCK_EX | fcntl.LOCK_NB)
                except BlockingIOError:
                    raise ValueError('application_still_running')
                self.restore(json.loads(journal.read_text()))
                journal.unlink()

    def cleanup(self, active):
        # Fail closed on uncertain references. Never prune Docker globally, force
        # delete an image, or traverse payload/instance directories.
        templates = json.loads(Path(self.cfg['templates']).read_text())
        referenced = set(active['images'].values()) | set(active.get('rollback_images', {}).values())
        referenced.update(value['image'] for value in templates.values())
        ids = run('docker', 'ps', '-aq').split()
        for offset in range(0, len(ids), 100):
            for container in json.loads(run('docker', 'inspect', *ids[offset:offset + 100])):
                referenced.add(container['Image'])
                referenced.add(container['Config']['Image'])
        protected = [Path(self.cfg[key]).resolve() for key in ('instances', 'templates', 'helper_unit')]
        protected.extend(Path(value['payload_dir']).resolve() for value in templates.values())
        unit = Path(self.cfg['helper_unit']).read_text()
        keep = {active['version'], active.get('rollback_version')}
        for path in self.root.iterdir():
            if path.is_symlink() or not path.is_dir() or not VERSION.fullmatch(path.name) or path.name in keep:
                continue
            if str(path / 'vm-runtime-helper') in unit or any(path.resolve() == p or path.resolve() in p.parents for p in protected):
                continue
            marker = path / '.prepared.json'
            inventory_path = path / '.release-files.json'
            if not marker.is_file() or marker.is_symlink() or not inventory_path.is_file() or inventory_path.is_symlink():
                continue
            prepared = json.loads(marker.read_text())
            if prepared.get('managed_by') != 'sub2api-runtime-updater-v2' or prepared.get('version') != path.name:
                continue
            images = prepared['images']
            if not images or not set(images) <= {'default', *PROFILES}:
                continue
            if any(image['tag'] != image_tag(path.name, self.arch, profile) for profile, image in images.items()):
                continue
            inventory = json.loads(inventory_path.read_text())
            actual_files, actual_directories = set(), set()
            safe = True
            for item in path.rglob('*'):
                if item.is_symlink() or not (item.is_file() or item.is_dir()):
                    safe = False
                    break
                (actual_directories if item.is_dir() else actual_files).add(str(item.relative_to(path)))
            if not safe or actual_files != set(inventory['files']) | {'.prepared.json', '.release-files.json'} or actual_directories != set(inventory['directories']):
                continue
            removable = [image for image in images.values() if image['image'] not in referenced and image['tag'] not in referenced]
            for image in removable:
                actual = json.loads(run('docker', 'image', 'inspect', image['tag']))[0]
                if actual['Id'] != image['image']:
                    raise ValueError('release_cleanup_image_changed')
            for image in removable:
                run('docker', 'image', 'rm', image['tag'])
            shutil.rmtree(path)


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path != '/v1/capabilities':
            self.send_error(404)
            return
        raw = json.dumps({'updater_version': UPDATER_VERSION, 'multi_image': True}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self):
        try:
            length = int(self.headers.get('Content-Length', '0'))
            if not 0 < length <= 256 or self.path not in ('/v1/prepare', '/v1/activate'):
                raise ValueError('invalid_request')
            data = json.loads(self.rfile.read(length))
            if set(data) != {'version'} or not VERSION.fullmatch(data['version']):
                raise ValueError('invalid_version')
            installer = self.server.installer
            with installer.operation():
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
    with installer.operation(wait=True):
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
