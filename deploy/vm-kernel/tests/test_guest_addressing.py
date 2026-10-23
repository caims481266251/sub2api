"""Guest slirp addresses are per-lease, distinct from Docker, and observed live."""
import copy
import importlib.machinery
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1] / 'upstream'


def load(name, path):
    loader = importlib.machinery.SourceFileLoader(name, str(path))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


entry = load('addressing_entry', ROOT / 'entry')
control = load('addressing_control', ROOT / 'guest/sub2-vm-control')


def state():
    return {'identity': {'mac': '02:10:20:30:40:50'}, 'network': {
        'network_name': 'sub2-vm-test', 'subnet': '10.240.0.16/28',
        'gateway': '10.240.0.17', 'address': '10.240.0.18',
        'guest_subnet': '10.241.0.16/28', 'guest_gateway': '10.241.0.18',
        'guest_address': '10.241.0.20', 'guest_dns': '10.241.0.19'}}


def address_observations():
    return ([{'ifname': 'enp0s3', 'address': '02:10:20:30:40:50', 'addr_info': [
        {'family': 'inet', 'scope': 'global', 'local': '10.241.0.20', 'prefixlen': 28}]}],
        [{'dst': 'default', 'gateway': '10.241.0.18', 'dev': 'enp0s3'}])


class GuestAddressingTests(unittest.TestCase):
    def test_real_iproute2_family_filter_omits_mac(self):
        addresses, routes = address_observations()
        addresses[0]['addr_info'].append({'family': 'inet6', 'scope': 'global', 'local': 'fd00::4', 'prefixlen': 64})
        def ip_output(args, **kwargs):
            if 'address' in args:
                response = copy.deepcopy(addresses)
                if '-4' in args:
                    for item in response:
                        item.pop('address', None)
                return json.dumps(response)
            return json.dumps(routes)
        with patch.object(control.subprocess, 'check_output', side_effect=ip_output):
            observed = control.guest_network_observation(state())
        self.assertEqual(observed['interface'], 'enp0s3')
        self.assertEqual(observed['guest_address'], '10.241.0.20')

    def test_client_identity_is_persisted_and_normalizes_existing_file(self):
        expected = '824b3d27-8fbd-4cc0-bf5e-ad2650602a74'
        manifest = {'client_identity': {'device_id': expected}}
        with tempfile.TemporaryDirectory(prefix='sub2-device-') as root:
            installation = Path(root) / 'codex-installation-id'
            with patch.object(entry.uuid, 'uuid4', side_effect=AssertionError('must use helper identity')):
                self.assertEqual(entry.installation_device(manifest, installation), expected)
                self.assertEqual(installation.read_text(), expected)
                installation.write_text(expected.upper() + '\n')
                self.assertEqual(entry.installation_device(manifest, installation), expected)
                self.assertEqual(installation.read_text(), expected.upper() + '\n')
                changed_credentials = dict(manifest, desired_credential_revision='rotated')
                self.assertEqual(entry.installation_device(changed_credentials, installation), expected)
            installation.write_text('814b3d27-8fbd-4cc0-bf5e-ad2650602a74')
            with self.assertRaisesRegex(ValueError, 'client_identity_mismatch'):
                entry.installation_device(manifest, installation)
            self.assertTrue(installation.read_text().startswith('814b'))

    def test_client_identity_rejects_invalid_manifest_and_symlink(self):
        with tempfile.TemporaryDirectory(prefix='sub2-device-') as root:
            installation = Path(root) / 'installation'
            for value in ('not-a-uuid', None, '824B3D27-8FBD-4CC0-BF5E-AD2650602A74'):
                with self.assertRaises(ValueError):
                    entry.installation_device({'client_identity': {'device_id': value}}, installation)
                self.assertFalse(installation.exists())
            target = Path(root) / 'target'
            target.write_text('824b3d27-8fbd-4cc0-bf5e-ad2650602a74')
            installation.symlink_to(target)
            with self.assertRaisesRegex(ValueError, 'client_identity_file_invalid'):
                entry.installation_device({'client_identity': {'device_id': target.read_text()}}, installation)

    def test_legacy_client_identity_keeps_existing_or_generates_once(self):
        with tempfile.TemporaryDirectory(prefix='sub2-device-') as root:
            installation = Path(root) / 'installation'
            first = entry.installation_device({}, installation)
            with patch.object(entry.uuid, 'uuid4', side_effect=AssertionError('identity must persist')):
                self.assertEqual(entry.installation_device({}, installation), first)

    def test_legacy_manifest_keeps_old_network(self):
        self.assertEqual(entry.guest_network_environment({}), {})
        with patch.object(control.subprocess, 'check_output') as command:
            self.assertIsNone(control.guest_network_observation({}))
            command.assert_not_called()

    def test_entry_projects_guest_not_docker_addresses(self):
        env = entry.guest_network_environment(state())
        self.assertEqual(env, {'KIN_KVM_GUEST_SUBNET': '10.241.0.16/28',
                              'KIN_KVM_GUEST_GATEWAY': '10.241.0.18',
                              'KIN_KVM_GUEST_ADDRESS': '10.241.0.20',
                              'KIN_KVM_GUEST_DNS': '10.241.0.19'})
        original = state()
        del original['network']['guest_dns']
        self.assertEqual(entry.guest_network_environment(original), env)

    def test_invalid_or_overlapping_leases_fail_closed(self):
        variants = []
        for key, value in [('guest_address', '10.240.0.18'), ('guest_gateway', '10.241.0.17'),
                           ('guest_dns', '8.8.8.8'), ('guest_subnet', '10.241.0.16/29'),
                           ('guest_subnet', '127.0.0.0/28'), ('subnet', '10.241.0.0/24'),
                           ('guest_address', '10.241.0.20,hostfwd=tcp::1-:2')]:
            bad = state()
            bad['network'][key] = value
            variants.append(bad)
        variants += [{'network': {}}, {'network': []}]
        for bad in variants:
            with self.subTest(lease=bad['network']):
                for validate in (entry.guest_network_environment, control.guest_network_spec):
                    with self.assertRaises((ValueError, KeyError)):
                        validate(bad)

    def test_live_address_and_route_required_without_mutation(self):
        addresses, routes = address_observations()
        with patch.object(control.subprocess, 'check_output', side_effect=[json.dumps(addresses), json.dumps(routes)]), patch.object(control.subprocess, 'run') as mutate:
            observed = control.guest_network_observation(state())
            self.assertEqual(observed['interface'], 'enp0s3')
            self.assertEqual(observed['guest_address'], '10.241.0.20')
            mutate.assert_not_called()
        for invalid_address, invalid_route in [(True, False), (False, True)]:
            addresses, routes = address_observations()
            if invalid_address:
                addresses[0]['addr_info'][0]['local'] = '10.0.2.15'
            if invalid_route:
                routes[0]['gateway'] = '10.0.2.2'
            with patch.object(control.subprocess, 'check_output', side_effect=[json.dumps(addresses), json.dumps(routes)]):
                with self.assertRaisesRegex(ValueError, 'unconfirmed'):
                    control.guest_network_observation(state())

    def test_extra_default_route_or_address_is_not_accepted(self):
        for extra_address in (True, False):
            addresses, routes = address_observations()
            if extra_address:
                addresses[0]['addr_info'].append({'family': 'inet', 'scope': 'global', 'local': '10.0.2.15', 'prefixlen': 24})
            else:
                routes.append({'dst': 'default', 'gateway': '10.0.2.2', 'dev': 'eth9'})
            with patch.object(control.subprocess, 'check_output', side_effect=[json.dumps(addresses), json.dumps(routes)]):
                with self.assertRaisesRegex(ValueError, 'unconfirmed'):
                    control.guest_network_observation(state())

    def test_runner_qemu_parameters_and_legacy_defaults(self):
        source = (ROOT / 'kin-kvm-entry').read_text()
        function = source[source.index('configure_guest_network() {'):source.index('\nwrite_seed() {')]
        harness = 'set -eu\nrequire() { for name in "$@"; do eval "value=\\${$name-}"; [ -n "$value" ] || exit 1; done; }\n' + function + '\nprintf "%s" "$NETDEV"\n'
        env = {k: v for k, v in os.environ.items() if not k.startswith('KIN_KVM_GUEST_')}
        legacy = subprocess.run(['bash', '-c', harness], env=env, capture_output=True, text=True, timeout=5)
        self.assertEqual(legacy.returncode, 0, legacy.stderr)
        self.assertEqual(legacy.stdout, 'user,id=net0,hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp:127.0.0.1:7070-:7070')
        allocated = subprocess.run(['bash', '-c', harness], env=dict(env, **entry.guest_network_environment(state())), capture_output=True, text=True, timeout=5)
        self.assertEqual(allocated.returncode, 0, allocated.stderr)
        self.assertIn('net=10.241.0.16/28,host=10.241.0.18,dhcpstart=10.241.0.20,dns=10.241.0.19,ipv6=off', allocated.stdout)
        self.assertIn('hostfwd=tcp:127.0.0.1:7070-10.241.0.20:7070', allocated.stdout)
        self.assertNotIn('10.240', allocated.stdout)
        env['KIN_KVM_GUEST_ADDRESS'] = '10.241.0.20'
        partial = subprocess.run(['bash', '-c', harness], env=env, capture_output=True, timeout=5)
        self.assertNotEqual(partial.returncode, 0)

    def test_seed_network_config_is_static_without_dns(self):
        with tempfile.TemporaryDirectory(prefix='sub2-network-seed-') as temporary:
            root = Path(temporary)
            guest = root / 'guest'
            guest.mkdir()
            (guest / 'user-data').write_text('#cloud-config\nmanage_etc_hosts: false\n')
            slot = root / 'slot'
            slot.mkdir()
            (slot / 'id_ed25519.pub').write_text('fixture-public-key\n')
            (slot / 'instance-id').write_text('fixture-instance\n')
            tool = root / 'genisoimage'
            tool.write_text('#!' + sys.executable + '\nimport os,pathlib,shutil\nfor name in ("network-config","guest.env"): shutil.copyfile(name,pathlib.Path(os.environ["TEST_OUTPUT"])/name)\n')
            tool.chmod(0o700)
            source = (ROOT / 'kin-kvm-entry').read_text()
            function = source[source.index('write_seed() {'):source.index('\nqmp_cmd() {')]
            function = function.replace('/opt/kin-kvm/guest', str(guest))
            env = dict(os.environ, **entry.guest_network_environment(state()), PATH=str(root) + os.pathsep + os.environ['PATH'],
                       SLOT_KVM=str(slot), TEST_OUTPUT=str(root), KIN_KVM_MAC=state()['identity']['mac'])
            for key in ('KIN_KVM_HOSTNAME', 'KIN_VM_ID', 'KIN_VM_NAME', 'KIN_VM_OS', 'KIN_GUEST_ENGINE',
                        'KIN_KVM_UID', 'KIN_KVM_GID', 'KIN_KVM_MACHINE_ID', 'KIN_KVM_SHARES', 'TZ', 'LANG'):
                env[key] = 'fixture'
            result = subprocess.run(['bash', '-c', 'set -eu\n' + function + '\nwrite_seed\n'], env=env, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            config = json.loads((root / 'network-config').read_text())['ethernets']['sub2guest']
            self.assertEqual(config['addresses'], ['10.241.0.20/28'])
            self.assertEqual(config['routes'], [{'to': '0.0.0.0/0', 'via': '10.241.0.18'}])
            self.assertFalse(config['dhcp4'])
            self.assertNotIn('nameservers', config)
            self.assertIn('KIN_KVM_GUEST_SUBNET=10.241.0.16/28', (root / 'guest.env').read_text())

    def test_boot_setup_reconciles_legacy_overlay_and_checks_real_address(self):
        source = (ROOT / 'guest/kin-guest-setup').read_text()
        code = source[source.index("  python3 - <<'PYNET'", source.index('apply_guest_network()')) + len("  python3 - <<'PYNET'\n"):]
        code = code[:code.index('\nPYNET')]
        code = code.replace("'/usr/local/sbin/sub2-vm-control'", repr(str(ROOT / 'guest/sub2-vm-control')))
        with tempfile.TemporaryDirectory(prefix='sub2-guest-network-') as temporary:
            root = Path(temporary)
            state_file = root / 'instance.json'
            state_file.write_text(json.dumps(state()))
            code = code.replace("'/run/kin/instance.json'", repr(str(state_file)))
            code = code.replace("'/run/systemd/network'", repr(str(root / 'networkd')))
            code = code.replace("'/run/kin-guest-config'", repr(str(root / 'evidence')))
            addresses, routes = address_observations()
            old_addresses = copy.deepcopy(addresses)
            old_addresses[0]['addr_info'][0].update(local='10.0.2.15', prefixlen=24)
            outputs = [json.dumps(addresses), json.dumps(old_addresses), json.dumps(addresses), json.dumps(routes)]
            with patch.object(subprocess, 'check_output', side_effect=outputs), patch.object(subprocess, 'run') as command:
                exec(compile(code, '<boot-network>', 'exec'), {})
            args = [item.args[0] for item in command.call_args_list]
            self.assertIn(['ip', '-4', 'address', 'replace', '10.241.0.20/28', 'dev', 'enp0s3'], args)
            self.assertIn(['ip', '-4', 'route', 'replace', 'default', 'via', '10.241.0.18', 'dev', 'enp0s3'], args)
            self.assertIn('DHCP=no', (root / 'networkd/00-sub2-vm.network').read_text())
            evidence = json.loads((root / 'evidence/network.json').read_text())
            self.assertEqual(evidence['guest_address'], '10.241.0.20')
            self.assertEqual(evidence['interface'], 'enp0s3')


if __name__ == '__main__':
    unittest.main()
