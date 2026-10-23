#!/usr/bin/env python3
"""Read-only lifecycle evidence. Never starts/stops/removes containers or reads credentials.

snapshot --docker-context CONTEXT --instance-id UUID --output PATH
validate --before PATH --after PATH --expect recovered|stopped|absent
"""
import argparse
import datetime
import json
from pathlib import Path
import subprocess
import uuid

GUEST_CODE = """import json,os,pathlib,subprocess
p=int(subprocess.check_output(['systemctl','show','kin-kernel.service','-p','MainPID','--value']))
assert p>0
stat=pathlib.Path('/proc',str(p),'stat').read_text().rsplit(')',1)[1].split()
print(json.dumps({'kernel_pid':p,'start_ticks':stat[19],'executable':os.readlink('/proc/'+str(p)+'/exe'),'guest_boot_id':pathlib.Path('/proc/sys/kernel/random/boot_id').read_text().strip()}))
"""


def docker(context, args):
    result = subprocess.run(['docker', '--context', context] + args, capture_output=True, text=True, timeout=60)
    if result.returncode:
        raise ValueError('docker_operation_failed')
    return result.stdout


def snapshot(context, instance):
    instance = str(uuid.UUID(instance))
    name = 'sub2-vm-' + instance
    # A successful inventory distinguishes confirmed absence from daemon failure.
    names = docker(context, ['ps', '-a', '--format', '{{.Names}}']).splitlines()
    report = {'at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              'instance_id': instance, 'docker_context': context, 'container_exists': name in names}
    if name not in names:
        return report
    template = '{{json .State}}\n{{json .Image}}\n{{json .Id}}\n{{json .Config.Labels}}'
    lines = docker(context, ['inspect', '--format', template, name]).splitlines()
    state, image, container, labels = [json.loads(line) for line in lines]
    if labels.get('sub2.vm.instance') != instance:
        raise ValueError('container_identity_mismatch')
    report.update(container_id=container, image=image, running=state['Running'], status=state['Status'])
    if not state['Running']:
        return report
    observation = json.loads(docker(context, ['exec', name, '/usr/local/lib/sub2-vm/observe']))
    # Strict allowlist: never expose credential/proxy revisions or private state.
    for key in ('generation', 'guest_boot_id', 'kernel_ready', 'capacity', 'actual_accelerator', 'kvm_enabled'):
        report[key] = observation[key]
    guest = json.loads(docker(context, ['exec', name, '/usr/local/bin/kin-guest-exec', '-u', '0', '--', 'python3', '-c', GUEST_CODE]))
    if guest['guest_boot_id'] != report['guest_boot_id']:
        raise ValueError('snapshot_boot_changed')
    report['guest'] = {key: guest[key] for key in ('kernel_pid', 'start_ticks', 'executable', 'guest_boot_id')}
    # Observe again to reject generation/boot changes during collection.
    final = json.loads(docker(context, ['exec', name, '/usr/local/lib/sub2-vm/observe']))
    if any(final[key] != observation[key] for key in ('generation', 'guest_boot_id')):
        raise ValueError('snapshot_generation_changed')
    return report


def validate(before, after, expect):
    if any(before.get(k) != after.get(k) for k in ('instance_id', 'docker_context')):
        raise ValueError('evidence_identity_mismatch')
    if not before.get('container_exists') or not before.get('running'):
        raise ValueError('before_not_running')
    if expect == 'absent':
        if after.get('container_exists') is not False:
            raise ValueError('container_not_absent')
    elif expect == 'stopped':
        if after.get('container_exists') is not True or after.get('running') is not False or after.get('status') not in ('exited', 'created'):
            raise ValueError('container_not_stopped')
    elif expect == 'recovered':
        if not after.get('running') or after.get('kernel_ready') is not True:
            raise ValueError('runtime_not_ready')
        if not isinstance(after.get('generation'), int) or after['generation'] <= before.get('generation', 0):
            raise ValueError('generation_not_advanced')
        if not after.get('guest_boot_id') or after['guest_boot_id'] == before.get('guest_boot_id'):
            raise ValueError('boot_not_changed')
        if after.get('image') != before.get('image'):
            raise ValueError('image_changed')
    else:
        raise ValueError('expectation_invalid')
    return {'verified': expect, 'instance_id': after['instance_id'], 'business_inference_verified': False,
            'helper_tombstone_verified': False, 'disk_retention_verified': False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    collect = sub.add_parser('snapshot')
    collect.add_argument('--docker-context', required=True)
    collect.add_argument('--instance-id', required=True)
    collect.add_argument('--output', required=True)
    check = sub.add_parser('validate')
    check.add_argument('--before', required=True)
    check.add_argument('--after', required=True)
    check.add_argument('--expect', choices=['recovered', 'stopped', 'absent'], required=True)
    args = parser.parse_args()
    if args.command == 'snapshot':
        report = snapshot(args.docker_context, args.instance_id)
        Path(args.output).write_text(json.dumps(report, indent=2) + '\n')
    else:
        report = validate(json.loads(Path(args.before).read_text()), json.loads(Path(args.after).read_text()), args.expect)
    print(json.dumps(report))

if __name__ == '__main__':
    try:
        main()
    except Exception:
        # Neither Docker diagnostics nor supplied file content are printable.
        raise SystemExit('lifecycle_evidence_failed')
