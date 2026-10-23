"""Execute guest engine selection with fake payload binaries and temporary paths."""
import json
import os
import shutil
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1] / 'upstream'


class GuestEngineTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='sub2-engine-')
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        source = (ROOT / 'guest/kin-kernel-start').read_text()
        self.mapping = {}
        for original in ('/usr/local/bin/kin-codex-kernel', '/run/kin/codex-kernel.json',
                         '/home/kincli/.kin/kin-kernel', '/usr/local/bin/kin-kernel',
                         '/opt/kin/kin-kernel', '/run/kin/kernel.json'):
            replacement = str(self.root / original.lstrip('/'))
            self.mapping[original] = replacement
            source = source.replace(original, replacement)
        self.script = self.root / 'start'
        self.script.write_text(source)
        for original in ('/usr/local/bin/kin-codex-kernel', '/usr/local/bin/kin-kernel'):
            binary = Path(self.mapping[original])
            binary.parent.mkdir(parents=True, exist_ok=True)
            binary.write_text('#!' + sys.executable + '\nimport json,os,sys\nprint(json.dumps({"bin":sys.argv[0],"args":sys.argv[1:],"env":dict(os.environ)}))\n')
            binary.chmod(0o700)
        config = Path(self.mapping['/run/kin/codex-kernel.json'])
        config.parent.mkdir(parents=True)
        config.write_text('{}')

    def run_start(self, engine=None):
        env = dict(os.environ)
        env.pop('KIN_GUEST_ENGINE', None)
        if engine is not None:
            env['KIN_GUEST_ENGINE'] = engine
        env.update(HTTP_PROXY='fixture', https_proxy='fixture', All_PrOxY='fixture',
                   NO_PROXY='fixture', KEEP_ME='retained')
        return subprocess.run(['/bin/sh', str(self.script)], env=env, text=True, capture_output=True, timeout=5)

    def test_default_and_explicit_claude_keep_original_arguments(self):
        for engine in (None, 'claude'):
            with self.subTest(engine=engine):
                result = self.run_start(engine)
                self.assertEqual(result.returncode, 0, result.stderr)
                output = json.loads(result.stdout)
                self.assertEqual(output['args'], ['--gateway-worker', '--config', self.mapping['/run/kin/kernel.json']])
                self.assertEqual(output['env']['HTTP_PROXY'], 'fixture')

    def test_codex_positional_config_and_case_insensitive_proxy_removal(self):
        result = self.run_start('codex')
        self.assertEqual(result.returncode, 0, result.stderr)
        output = json.loads(result.stdout)
        self.assertEqual(output['args'], [self.mapping['/run/kin/codex-kernel.json']])
        self.assertEqual(output['env']['KEEP_ME'], 'retained')
        self.assertFalse(any(k.lower() in ('http_proxy','https_proxy','all_proxy','no_proxy') for k in output['env']))

    def test_missing_codex_files_never_fall_back_to_claude(self):
        for original in ('/run/kin/codex-kernel.json', '/usr/local/bin/kin-codex-kernel'):
            with self.subTest(path=original):
                path = Path(self.mapping[original])
                renamed = path.with_name(path.name + '.saved')
                path.rename(renamed)
                try:
                    result = self.run_start('codex')
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(result.stdout, '')
                    self.assertIn('missing', result.stderr)
                finally:
                    renamed.rename(path)

    def test_unknown_engine_rejected(self):
        result = self.run_start('other')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, '')

    def test_service_configuration_survives_runtime_directory_cleanup(self):
        service = (ROOT / 'guest/kin-kernel.service').read_text()
        runtime = next(line.split('=', 1)[1] for line in service.splitlines() if line.startswith('RuntimeDirectory='))
        environment = next(line.split('=', 1)[1] for line in service.splitlines() if line.startswith('EnvironmentFile='))
        self.assertFalse(environment.startswith('-'), 'missing engine configuration must fail closed')
        source = (ROOT / 'guest/kin-guest-setup').read_text()
        start = source.index('write_slot_env() {')
        function = source[start:source.index('\nrelax_selinux()', start)]
        function = function.replace('/run/', str(self.root / 'lifecycle') + '/')
        function = function.replace('/etc/systemd/system/kin-kernel.service.d', str(self.root / 'lifecycle-units'))
        harness = self.root / 'lifecycle-setup'
        harness.write_text('set -eu\ndie() { exit 1; }\nchown() { :; }\nsystemctl() { :; }\n' + function + '\nwrite_slot_env\n')
        for engine in ('claude', 'codex'):
            with self.subTest(engine=engine):
                subprocess.run(['/bin/sh', str(harness)], env=dict(os.environ, KIN_GUEST_ENGINE=engine, KIN_KVM_GID='10001'), check=True, capture_output=True, timeout=5)
                config = self.root / 'lifecycle' / environment.removeprefix('/run/')
                expected = config.read_bytes()
                for operation in ('restart', 'stop/start'):
                    with self.subTest(operation=operation):
                        # Model systemd's RuntimeDirectory removal; execute the
                        # actual launcher with EnvironmentFile loaded afterward.
                        shutil.rmtree(self.root / 'lifecycle' / runtime, ignore_errors=True)
                        self.assertEqual(config.read_bytes(), expected)
                        command = 'set -a; . "$1"; exec /bin/sh "$2"'
                        result = subprocess.run(['/bin/sh', '-c', command, 'lifecycle', str(config), str(self.script)], capture_output=True, text=True, timeout=5)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        output = json.loads(result.stdout)
                        wanted = [self.mapping['/run/kin/codex-kernel.json']] if engine == 'codex' else ['--gateway-worker', '--config', self.mapping['/run/kin/kernel.json']]
                        self.assertEqual(output['args'], wanted)

    def test_setup_writes_validated_engine_to_environment_file(self):
        source = (ROOT / 'guest/kin-guest-setup').read_text()
        start = source.index('write_slot_env() {')
        function = source[start:source.index('\nrelax_selinux()', start)]
        function = function.replace('/run/kin-guest', str(self.root / 'run'))
        function = function.replace('/etc/systemd/system/kin-kernel.service.d', str(self.root / 'units'))
        harness = self.root / 'setup'
        harness.write_text('set -eu\ndie() { exit 1; }\nchown() { :; }\nsystemctl() { :; }\n' + function + '\nwrite_slot_env\n')
        for engine in ('claude', 'codex', 'invalid'):
            with self.subTest(engine=engine):
                result = subprocess.run(['/bin/sh', str(harness)], env=dict(os.environ, KIN_GUEST_ENGINE=engine, KIN_KVM_GID='10001'), capture_output=True, timeout=5)
                self.assertEqual(result.returncode == 0, engine != 'invalid')
                if engine != 'invalid':
                    self.assertIn('KIN_GUEST_ENGINE=' + engine + '\n', (self.root / 'run-config/slot.env').read_text())


if __name__ == '__main__':
    unittest.main()
