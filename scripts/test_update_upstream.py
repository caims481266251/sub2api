"""Git lifecycle regression tests; no network, Go builds, or deployed services."""
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('updater', Path(__file__).with_name('update_upstream.py'))
u = importlib.util.module_from_spec(spec)
spec.loader.exec_module(u)


class UpdateLifecycle(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='sub2api-update-test.')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.up = self.root / 'upstream'
        self.local = self.root / 'local'
        env = dict(os.environ, GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull,
                   GIT_AUTHOR_NAME='Test', GIT_AUTHOR_EMAIL='test@example.invalid',
                   GIT_COMMITTER_NAME='Test', GIT_COMMITTER_EMAIL='test@example.invalid')
        self.environment = patch.dict(os.environ, env, clear=True)
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.up.mkdir()
        self.g(self.up, 'init', '-b', 'main')
        (self.up / 'common').write_text('base\n')
        (self.up / 'draft').write_text('base\n')
        self.g(self.up, 'add', '.')
        self.g(self.up, 'commit', '-m', 'base')
        subprocess.run(['git', 'clone', str(self.up), str(self.local)], check=True, capture_output=True)
        self.g(self.local, 'remote', 'add', 'upstream', str(self.up))
        (self.up / 'release').write_text('official\n')
        self.g(self.up, 'add', '.')
        self.g(self.up, 'commit', '-m', 'release')
        self.g(self.up, 'tag', 'v9.0.0')
        u.ROOT = self.local
        u.GITDIR = self.local / '.git'
        u.STATE = u.GITDIR / 'upstream-update/state.json'

    def g(self, where, *args):
        return subprocess.check_output(['git', *args], cwd=where, text=True, stderr=subprocess.PIPE).strip()

    def call(self, action):
        with patch.object(sys, 'argv', ['update', action]):
            return u.main()

    def verified_without_builds(self, s):
        worktree = Path(s['worktree'])
        self.g(worktree, 'commit', '-m', 'test merge')
        s['verified'] = self.g(worktree, 'rev-parse', 'HEAD')
        u.save(s)

    def test_dirty_worktree_survives_finish_and_cleanup(self):
        (self.local / 'draft').write_text('staged\n')
        self.g(self.local, 'add', 'draft')
        (self.local / 'draft').write_text('unstaged\n')
        (self.local / 'notes').write_text('untracked\n')
        before = self.g(self.local, 'status', '--porcelain')
        with patch.object(u, 'verify', self.verified_without_builds):
            self.call('v9.0.0')
        self.assertEqual(before, self.g(self.local, 'status', '--porcelain'))
        self.call('finish')
        self.assertEqual(before, self.g(self.local, 'status', '--porcelain'))
        self.assertEqual('staged', self.g(self.local, 'show', ':draft'))
        self.assertEqual('unstaged\n', (self.local / 'draft').read_text())
        self.assertEqual('untracked\n', (self.local / 'notes').read_text())
        self.assertTrue((self.local / 'release').exists())
        worktree = Path(u.read()['worktree'])
        self.call('cleanup')
        self.assertFalse(worktree.exists())
        self.assertFalse(u.STATE.exists())

    def test_changed_verified_worktree_blocks_finish(self):
        with patch.object(u, 'verify', self.verified_without_builds):
            self.call('v9.0.0')
        s = u.read()
        (Path(s['worktree']) / 'release').write_text('changed\n')
        with self.assertRaises(RuntimeError):
            self.call('finish')
        self.assertEqual(s['base'], self.g(self.local, 'rev-parse', 'HEAD'))

    def test_conflicts_leave_original_untouched(self):
        (self.local / 'common').write_text('local\n')
        self.g(self.local, 'add', 'common')
        self.g(self.local, 'commit', '-m', 'local customization')
        (self.up / 'common').write_text('upstream\n')
        self.g(self.up, 'add', 'common')
        self.g(self.up, 'commit', '-m', 'conflicting release')
        self.g(self.up, 'tag', 'v9.0.1')
        before = self.g(self.local, 'rev-parse', 'HEAD')
        self.assertEqual(1, self.call('v9.0.1'))
        self.assertEqual(before, self.g(self.local, 'rev-parse', 'HEAD'))
        self.assertEqual('local\n', (self.local / 'common').read_text())
        self.assertEqual('common', self.g(Path(u.read()['worktree']), 'diff', '--name-only', '--diff-filter=U'))
        with self.assertRaises(RuntimeError):
            self.call('finish')


if __name__ == '__main__':
    unittest.main()
