#!/usr/bin/env python3
"""Isolated upstream release merges. State stays in this checkout's Git directory."""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import signal
import fcntl
import subprocess
import sys
import tempfile
import time


def run(*args, cwd=None, capture=False, env=None):
    return subprocess.run(args, cwd=cwd, env=env, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def git(*args, cwd=None):
    return run('git', *args, cwd=cwd or ROOT, capture=True).strip()


def save(state):
    STATE.parent.mkdir(parents=True, exist_ok=True)
    temporary = STATE.with_suffix('.tmp')
    temporary.write_text(json.dumps(state, indent=2) + '\n')
    temporary.replace(STATE)


def read():
    if not STATE.exists():
        raise RuntimeError('没有进行中的升级。先运行：update-upstream.sh vX.Y.Z')
    return json.loads(STATE.read_text())


def dirty(path):
    return bool(git('status', '--porcelain', '--untracked-files=normal', cwd=path))


def verify(s):
    s['verified'] = None
    save(s)
    path = Path(s['worktree'])
    if git('diff', '--name-only', '--diff-filter=U', cwd=path):
        raise RuntimeError('请先解决冲突并 git add，再运行 continue。')
    if git('diff', '--name-only', cwd=path):
        raise RuntimeError('请先 git add 暂存全部冲突修复，避免验证内容与提交不一致。')
    git('diff', '--check', cwd=path)
    git('diff', '--cached', '--check', cwd=path)
    free = shutil.disk_usage(path).free
    print(f'构建前可用空间：{free / 2**30:.1f} GiB', flush=True)
    if free < 12 * 2**30:
        raise RuntimeError('可用空间不足 12 GiB；未启动构建。')
    env = os.environ.copy()
    env['GOTOOLCHAIN'] = 'auto'
    # A fresh cache is shared by checks in this round and always removed on exit.
    with tempfile.TemporaryDirectory(prefix='sub2api-build.') as tmp:
        for name in ('cache', 'tmp'):
            Path(tmp, name).mkdir()
        env.update(GOCACHE=str(Path(tmp, 'cache')), GOTMPDIR=str(Path(tmp, 'tmp')))
        try:
            backend = path / 'backend'
            run('go', 'test', '-tags=unit', './internal/service', './internal/repository',
                './internal/handler/...', './internal/server/...', './internal/pkg/requesttiming',
                './internal/config', './migrations',
                '-run', 'Borrow|Hosting|Hosted|SessionAffinity|Seamless|Astra|APIKey|Admission|Config|Migration',
                '-timeout', '180s', cwd=backend, env=env)
            run('go', 'build', '-o', str(Path(tmp, 'sub2api')), './cmd/server', cwd=backend, env=env)
            frontend = path / 'frontend'
            pnpm = ['corepack', 'pnpm@9.15.9']
            run(*pnpm, 'install', '--frozen-lockfile', cwd=frontend, env=env)
            run(*pnpm, 'exec', 'vue-tsc', '--noEmit', cwd=frontend, env=env)
            run(*pnpm, 'run', 'check:i18n', cwd=frontend, env=env)
            run(*pnpm, 'exec', 'vitest', 'run', 'src/views/user/__tests__/KeysView.spec.ts',
                'src/components/admin/usage/__tests__/UsageTable.spec.ts',
                'src/components/admin/usage/__tests__/UsageTimingDialog.spec.ts',
                'src/components/admin/__tests__/HarvestGatewayBorrowPanel.spec.ts',
                cwd=frontend, env=env)
            run(*pnpm, 'exec', 'vite', 'build', '--outDir', str(Path(tmp, 'frontend')), cwd=frontend, env=env)
        finally:
            print('本轮结束，正在清理临时 Go 缓存和构建产物。', flush=True)
    print(f'临时缓存已清理；剩余空间：{shutil.disk_usage(path).free / 2**30:.1f} GiB')
    merge_head = Path(git('rev-parse', '--absolute-git-dir', cwd=path)) / 'MERGE_HEAD'
    if merge_head.exists():
        git('commit', '-m', f"Merge upstream {s['tag']} preserving local customizations", cwd=path)
    if dirty(path):
        raise RuntimeError('升级工作区仍有未提交修改，请提交解决结果后重新运行 continue。')
    git('merge-base', '--is-ancestor', s['target'], 'HEAD', cwd=path)
    git('merge-base', '--is-ancestor', s['base'], 'HEAD', cwd=path)
    s['verified'] = git('rev-parse', 'HEAD', cwd=path)
    save(s)
    print('验证通过。审查后运行 finish，将更新快进合入原分支。')


def main():
    p = argparse.ArgumentParser(description='官方 tag 升级：vX.Y.Z / status / continue / finish / cleanup')
    p.add_argument('action')
    args = p.parse_args()
    action = args.action
    if re.fullmatch(r'v\d+\.\d+\.\d+(?:[-.][A-Za-z0-9.-]+)?', action):
        if STATE.exists():
            raise RuntimeError('已有升级记录，请先 status；完成后 cleanup。')
        branch = git('symbolic-ref', '--short', 'HEAD')
        for marker in ('MERGE_HEAD', 'CHERRY_PICK_HEAD', 'rebase-merge', 'rebase-apply'):
            if (GITDIR / marker).exists():
                raise RuntimeError('当前工作区有未结束的 Git 操作。')
        git('fetch', '--no-tags', 'upstream', f'refs/tags/{action}')
        target = git('rev-parse', 'FETCH_HEAD^{commit}')
        if subprocess.run(['git', 'merge-base', '--is-ancestor', target, 'HEAD'], cwd=ROOT).returncode == 0:
            print(f'{action} 已包含在当前分支，无需升级。')
            return
        stamp = time.strftime('%Y%m%d-%H%M%S')
        update = f'codex/update-{action}-{stamp}'
        backup = f'codex/backup-before-{action}-{stamp}'
        base = git('rev-parse', 'HEAD')
        git('branch', backup, base)
        worktree = STATE.parent / f'worktree-{stamp}'
        git('worktree', 'add', '-b', update, str(worktree), base)
        s = dict(tag=action, target=target, base=base, branch=branch, backup=backup,
                 update=update, worktree=str(worktree), verified=None, completed=False)
        save(s)
        print(f'升级工作区：{worktree}\n恢复分支：{backup}\n原工作区及未提交修改保持原状。', flush=True)
        result = subprocess.run(['git', '-c', 'merge.autostash=false', 'merge', '--no-commit', '--no-ff', target], cwd=worktree)
        if result.returncode:
            print('合并未完成。请在升级工作区处理冲突，git add 后运行 continue。')
            return 1
        verify(s)
    elif action == 'status':
        s = read()
        print(json.dumps(s, ensure_ascii=False, indent=2))
        print(git('status', '--short', '--branch', cwd=s['worktree']))
    elif action == 'continue':
        s = read()
        if s['completed']:
            raise RuntimeError('升级已完成，可运行 cleanup。')
        verify(s)
    elif action == 'finish':
        s = read()
        if s['completed']:
            raise RuntimeError('升级已完成，可运行 cleanup。')
        if not s['verified'] or git('rev-parse', 'HEAD', cwd=s['worktree']) != s['verified'] or dirty(s['worktree']):
            raise RuntimeError('升级结果未验证或已变动，请运行 continue。')
        if git('symbolic-ref', '--short', 'HEAD') != s['branch'] or git('rev-parse', 'HEAD') != s['base']:
            raise RuntimeError('原分支已切换或新增提交，请重新评估合并，未修改原工作区。')
        if s.get('stash'):
            raise RuntimeError('存在上次 finish 保存的修改，请查看 status 并先恢复该 stash。')
        if dirty(ROOT):
            git('stash', 'push', '--include-untracked', '-m', f"upstream-update-{s['tag']}")
            s['stash'] = git('rev-parse', 'stash@{0}')
            save(s)
        git('merge', '--ff-only', s['verified'])
        s['completed'] = True
        save(s)
        if s.get('stash'):
            # Keep the exact stash as a recovery copy, including if reapplication conflicts.
            run('git', 'stash', 'apply', '--index', s['stash'], cwd=ROOT)
            print('原未提交修改已恢复；备份 stash 保留，确认无误后可自行删除。')
        print(f"已更新 {s['branch']} 至 {s['verified']}。未推送、未部署。可运行 cleanup。")
    elif action == 'cleanup':
        s = read()
        if not s['completed']:
            raise RuntimeError('升级尚未完成，保留工作区与冲突处理成果。')
        git('worktree', 'remove', s['worktree'])
        git('branch', '-d', s['update'])
        STATE.unlink()
        print('升级工作区已清理，升级前备份分支保留。')
    else:
        p.error('请输入正式版本 tag，或 status / continue / finish / cleanup')
    return 0


if __name__ == '__main__':
    try:
        ROOT = Path(run('git', 'rev-parse', '--show-toplevel', capture=True).strip())
        GITDIR = Path(git('rev-parse', '--absolute-git-dir'))
        STATE = GITDIR / 'upstream-update' / 'state.json'
        STATE.parent.mkdir(parents=True, exist_ok=True)
        signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
        with (STATE.parent / 'lock').open('w') as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RuntimeError('另一个更新命令正在运行。')
            sys.exit(main())
    except (RuntimeError, subprocess.CalledProcessError) as exc:
        print(f'更新已停止：{exc}', file=sys.stderr)
        sys.exit(1)
    except KeyboardInterrupt:
        print('已中断；升级工作区保留，临时构建缓存已清理。', file=sys.stderr)
        sys.exit(130)
