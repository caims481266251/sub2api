import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

ROOT = Path(__file__).parents[1]
spec = importlib.util.spec_from_file_location('profile_check', ROOT / 'container/check-runtime.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class SystemProfilesTest(unittest.TestCase):
    def test_catalog_pins_real_distros_and_architectures(self):
        catalog = json.loads((ROOT / 'container/profiles/profiles.json').read_text())['profiles']
        self.assertEqual(set(catalog), set(m.EXPECTED))
        for name, profile in catalog.items():
            self.assertEqual(set(profile['bases']), {'amd64', 'arm64'} if name in ('ubuntu-24.04', 'debian-12') else {'amd64'})
            for base in profile['bases'].values():
                self.assertRegex(base, r'^[a-z0-9./-]+@sha256:[a-f0-9]{64}$')
            dockerfile = (ROOT / 'container/profiles' / profile['dockerfile']).read_text()
            self.assertIn('ARG BASE_IMAGE', dockerfile)
            self.assertIn('check-runtime.py --profile', dockerfile)
            self.assertNotIn('> /etc/os-release', dockerfile)
        arch = (ROOT / 'container/profiles/Dockerfile.archlinux').read_text()
        self.assertIn('archive.archlinux.org/repos/2026/10/25/', arch)
        self.assertIn('pacman -Syyuu', arch)
        self.assertIn('/usr/bin/setpriv', (ROOT / 'container/profiles/Dockerfile.rpm').read_text())

    def test_wrong_distro_is_rejected(self):
        with patch.object(m.Path, 'read_text', return_value='ID=debian\nVERSION_ID="12"\n'):
            with self.assertRaisesRegex(ValueError, 'system_profile_mismatch'):
                m.check_system('ubuntu-24.04')

    def test_missing_native_dependency_or_trust_rejected(self):
        with patch.object(m.Path, 'read_text', return_value='ID=debian\nVERSION_ID="12"\n'), patch.object(m.shutil, 'which', return_value=None):
            with self.assertRaisesRegex(ValueError, 'runtime_dependency_missing'):
                m.check_system('debian-12')
        trust = Mock()
        trust.get_ca_certs.return_value = []
        with patch.object(m.Path, 'read_text', return_value='ID=debian\nVERSION_ID="12"\n'), patch.object(m.shutil, 'which', return_value='/usr/bin/fixture'), patch.object(m.ssl, 'create_default_context', return_value=trust), patch.object(m.ssl, 'get_default_verify_paths', return_value=Mock(capath=None)):
            with self.assertRaisesRegex(ValueError, 'ca_trust_missing'):
                m.check_system('debian-12')

    def test_preloaded_ca_bundle_is_accepted(self):
        trust = Mock()
        trust.get_ca_certs.return_value = [{'subject': 'fixture-ca'}]
        with patch.object(m.ssl, 'create_default_context', return_value=trust), patch.object(m.ssl, 'get_default_verify_paths') as defaults:
            m.check_ca_trust()
        defaults.assert_not_called()

    def test_lazy_default_hash_directory_is_validated_without_disabling_tls(self):
        empty, loaded = Mock(), Mock()
        empty.get_ca_certs.return_value = []
        loaded.get_ca_certs.return_value = [{'subject': 'fixture-ca'}]
        with tempfile.TemporaryDirectory() as directory:
            anchor = Path(directory) / '0123abcd.0'
            anchor.write_text('fixture')
            with patch.object(m.ssl, 'create_default_context', return_value=empty), patch.object(m.ssl, 'get_default_verify_paths', return_value=Mock(capath=directory)), patch.object(m.ssl, 'SSLContext', return_value=loaded) as context:
                m.check_ca_trust()
            context.assert_called_once_with(m.ssl.PROTOCOL_TLS_CLIENT)
            loaded.load_verify_locations.assert_called_once_with(cafile=str(anchor))

    def test_empty_unhashed_and_invalid_ca_directories_are_rejected(self):
        empty, invalid = Mock(), Mock()
        empty.get_ca_certs.return_value = []
        invalid.load_verify_locations.side_effect = m.ssl.SSLError('invalid certificate')
        with tempfile.TemporaryDirectory() as directory, patch.object(m.ssl, 'create_default_context', return_value=empty), patch.object(m.ssl, 'get_default_verify_paths', return_value=Mock(capath=directory)), patch.object(m.ssl, 'SSLContext', return_value=invalid):
            for filename in (None, 'arbitrary.pem', '0123abcd.0'):
                if filename:
                    (Path(directory) / filename).write_text('not a certificate')
                with self.assertRaisesRegex(ValueError, 'ca_trust_missing'):
                    m.check_ca_trust()
            invalid.load_verify_locations.assert_called_once_with(cafile=str(Path(directory) / '0123abcd.0'))

    def test_build_package_contains_all_images_and_bootstrap(self):
        script = (ROOT / 'package-tools.sh').read_text()
        self.assertIn("'manifest_version':2,'minimum_updater':2", script)
        self.assertIn('images.tsv', script)
        self.assertIn('--network-check', script)
        self.assertIn('docker-kernel-updater-bootstrap-$version-linux-$arch.tar.gz', script)


if __name__ == '__main__':
    unittest.main()
