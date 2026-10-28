#!/usr/bin/env python3
"""Root bootstrap/upgrade. Private payload and instance state stay on the host."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import stat
import subprocess


def install(args, unit_root=Path('/etc/systemd/system')):
    if os.geteuid() != 0:
        raise ValueError('run as root')
    for path in (args.base, args.instances, args.helper_socket, args.templates, args.payload_dir):
        if path is not None and (not path.startswith('/') or not re.fullmatch(r'[a-zA-Z0-9_./-]+', path) or '..' in Path(path).parts):
            raise ValueError('invalid path')
    if not re.fullmatch(r'[a-zA-Z0-9_-]+\.service', args.helper_service):
        raise ValueError('invalid service')
    if args.version is not None and not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', args.version):
        raise ValueError('invalid version')
    if args.activate and not args.version:
        raise ValueError('--activate requires --version')
    if args.service_uid <= 0 or args.service_gid <= 0:
        raise ValueError('service UID and GID must be non-root')
    base = Path(args.base)
    base.mkdir(parents=True, exist_ok=True, mode=0o700)
    # The service needs traversal to its socket/instance mounts, never reads of
    # updater configuration, releases or private payload.
    os.chown(base, 0, args.service_gid)
    base.chmod(0o710)
    spec = importlib.util.spec_from_file_location('sub2api_update_agent_bootstrap', Path(__file__).with_name('update_agent.py'))
    updater = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(updater)
    config = {'releases': str(base / 'releases'), 'instances': args.instances, 'helper_socket': args.helper_socket,
              'templates': args.templates, 'service_uid': args.service_uid, 'service_gid': args.service_gid,
              'helper_service': args.helper_service, 'helper_unit': str(unit_root / args.helper_service)}
    path = base / 'update-agent.json'
    if path.exists() and json.loads(path.read_text()) != config:
        raise ValueError('existing configuration differs; review before replacing')
    templates = Path(args.templates)
    if not templates.exists() and not (args.payload_dir and args.version):
        raise ValueError('fresh installation requires --payload-dir and --version')
    if templates.exists() and args.payload_dir:
        raise ValueError('existing templates already bind the private payload; omit --payload-dir')
    socket_dir = Path(args.helper_socket).parent
    socket_dir.mkdir(parents=True, exist_ok=True, mode=0o750)
    os.chown(socket_dir, 0, args.service_gid)
    socket_dir.chmod(0o750)
    Path(args.instances).mkdir(parents=True, exist_ok=True, mode=0o700)
    templates.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    # Never replace the lock inode, including while an application holds it.
    fd = os.open(args.helper_socket + '.coordinator.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        if not stat.S_ISREG(os.fstat(fd).st_mode):
            raise ValueError('invalid coordinator lock')
        os.fchown(fd, args.service_uid, args.service_gid)
        os.fchmod(fd, 0o600)
    finally:
        os.close(fd)
    updater.atomic_json(path, config)
    updater.atomic_bytes(base / 'update_agent.py', Path(__file__).with_name('update_agent.py').read_bytes(), 0o700)
    unit = unit_root / 'sub2api-runtime-update.service'
    updater.atomic_bytes(unit, ('[Unit]\nDescription=Sub2API paired runtime updater\nAfter=docker.service\nRequires=docker.service\n[Service]\nExecStart=/usr/bin/python3 ' + str(base / 'update_agent.py') + ' --config ' + str(path) + '\nRestart=on-failure\nUMask=0077\n[Install]\nWantedBy=multi-user.target\n').encode(), 0o644)
    subprocess.run(['systemctl', 'daemon-reload'], check=True)
    subprocess.run(['systemctl', 'enable', 'sub2api-runtime-update.service'], check=True)
    # enable --now does not restart an already-running v1 updater.
    subprocess.run(['systemctl', 'restart', 'sub2api-runtime-update.service'], check=True)
    if args.version:
        agent = updater.Installer(config)
        with agent.operation(wait=True):
            agent.recover()
            manifest = agent.prepare(args.version, initial_payload=args.payload_dir)
            if not templates.exists():
                default = updater.payload_template(args.payload_dir)
                default['image'] = manifest['image']
                updater.atomic_json(templates, {'default': default})
            if args.activate:
                agent.activate(args.version)
                subprocess.run(['systemctl', 'enable', args.helper_service], check=True)
    print('Updater v2 installed and restarted. Prepare the matching release before restarting Sub2API.' if not args.version else 'Updater v2 installed; release ' + args.version + (' activated.' if args.activate else ' prepared.'))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--base', required=True)
    parser.add_argument('--instances', required=True)
    parser.add_argument('--helper-socket', required=True)
    parser.add_argument('--templates', required=True)
    parser.add_argument('--service-uid', type=int, default=1000)
    parser.add_argument('--service-gid', type=int, default=20000)
    parser.add_argument('--helper-service', default='sub2api-vm-helper.service')
    parser.add_argument('--version', help='download and validate the selected fixed-repository release')
    parser.add_argument('--activate', action='store_true', help='activate after prepare; the application must be stopped')
    parser.add_argument('--payload-dir', help='fresh installation only: root-provisioned private payload directory')
    args = parser.parse_args()
    try:
        install(args)
    except ValueError as error:
        parser.exit(1, str(error) + '\n')


if __name__ == '__main__':
    main()
