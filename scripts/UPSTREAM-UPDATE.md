# 合并官方版本到本地定制分支

在仓库根目录运行。需要 Git、Python 3、Go、Node.js 和 Corepack；前端固定使用 pnpm 9.15.9。Go 按当前升级版本 go.mod 自动选择工具链，依赖下载缓存保留。

```bash
# 示例版本；替换为你要升级的正式 tag
./scripts/update-upstream.sh v2.13.4
./scripts/update-upstream.sh status
```

脚本从 `upstream` 获取指定 tag，创建 `codex/backup-before-*` 恢复分支及 `codex/update-*` 升级分支。独立升级工作区路径会打印，并保存在原 checkout 的 `.git/upstream-update/state.json` 中。当前分支和原未提交修改不会在这个阶段移动。

有冲突时，进入打印出的升级工作区处理，不要在原工作区解决：

```bash
cd <打印出的升级工作区>
# 编辑冲突文件，检查 Git 自动复用的解决结果
 git add <已解决的文件>
cd <原仓库根目录>
./scripts/update-upstream.sh continue
```

`continue` 执行针对借票、托管、会话亲和、API Key、Astra、保活/断连、加密会话隔离、连接诊断、安全审计及定时调度的 Go 单元回归和保活关键路径竞态检测，以及后端构建、前端类型/i18n/关键 UI 回归和构建。默认不运行 Docker 集成测试。新版本改变测试目录或依赖管理方式时，应先调整脚本的验证清单；这些检查不代表完整业务验收。

验证成功后自动生成合并提交；如果你手动提交过冲突解决结果，也会验证该提交。审查升级分支差异，然后在原仓库运行：

```bash
./scripts/update-upstream.sh finish
./scripts/update-upstream.sh cleanup
```

`finish` 要求原分支仍位于升级开始时的提交，升级工作区无新增修改且 HEAD 与验证结果一致。它临时 stash 原目录的已跟踪和未跟踪修改（不包含忽略文件），快进原分支后用 `--index` 恢复。为防意外，stash 备份保留，确认后自行按对应条目删除。若恢复出现冲突，合并提交已经落地，原修改仍保存在状态文件记录的 stash SHA 中；不要重复 apply 或 reset，先查看 `git status` 并解决恢复冲突。

`cleanup` 仅在 finish 已完成后删除升级工作区及升级分支；若其中还有未提交文件会拒绝删除。恢复分支保留。未完成升级需要取消时，保留状态并人工检查后再移除对应工作区/分支和 state.json，不做强制清理。

仓库启用 `rerere.enabled=true`、`rerere.autoupdate=false`，只对本仓库有效。历史解决方案可以复用，但必须审查、暂存、验证。

每轮验证创建独立临时 GOCACHE/GOTMPDIR，与构建输出一起在成功、失败及正常中断后清理。最低预留 12 GiB；构建前后打印可用空间。强制杀进程或断电时可能遗留临时目录，确认没有相关构建进程后再清理。本脚本不清理全局缓存、业务记录、数据卷或部署文件。

此流程只合并源代码，不自动推送、部署、重启或执行数据库迁移。原工作区未提交功能在 finish 时才恢复，没有参与隔离验证；这类改动应补跑针对性测试。建议已完成的定制功能在升级前先提交。
