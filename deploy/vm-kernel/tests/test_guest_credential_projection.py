import ast
import os
from pathlib import Path
import tempfile
import unittest

class GuestCredentialProjectionTests(unittest.TestCase):
    def setUp(self):
        source = Path(__file__).resolve().parents[1] / 'upstream/guest/sub2-vm-network'
        tree = ast.parse(source.read_text())
        function = next(n for n in tree.body if isinstance(n, ast.FunctionDef) and n.name == 'project_claude_credentials')
        scope = {'Path': Path, 'os': os}
        exec(compile(ast.Module(body=[function], type_ignores=[]), str(source), 'exec'), scope)
        self.project = scope['project_claude_credentials']

    def test_codex_does_not_touch_claude_directory(self):
        with tempfile.TemporaryDirectory() as root:
            target = Path(root) / '.claude'
            self.project('codex', target)
            self.assertFalse(target.exists())

    def test_claude_projection_is_repeatable_without_chown(self):
        with tempfile.TemporaryDirectory() as root:
            target = Path(root) / '.claude'
            self.project('claude', target, os.getuid())
            self.project('claude', target, os.getuid())
            self.assertEqual(os.readlink(target / 'credentials.json'), '/run/sub2-credentials/credential.json')

    def test_wrong_owner_fails_closed(self):
        with tempfile.TemporaryDirectory() as root:
            with self.assertRaisesRegex(SystemExit, 'credential_directory_owner_invalid'):
                self.project('claude', Path(root) / '.claude', os.getuid() + 1)
