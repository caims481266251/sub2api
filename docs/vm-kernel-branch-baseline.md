# VM 内核账号模式：分支基线与工作边界

日期：2026-10-22。当前阶段：独立分支已建立；落实并深审方案文档，不编写功能代码。用户指定首版为 KVM 真虚拟机，并要求 WS 与跨协议转换在本次迭代交付。

## 1. 分支来源

| 项目 | 固定值 |
|---|---|
| 功能分支 | `codex/vm-kernel-account-mode` |
| 基础仓库 | https://github.com/ranxi2001/sub2api |
| 基础发布 | https://github.com/ranxi2001/sub2api/releases/tag/v2.15.0 |
| 上游 tag 对象 | `645dc8c6ae91be9298df689e6e77b69fcd184aba` |
| 上游发布提交 | `8b36fe7d647c28e8189147c6bfce52a41b6e26db` |
| 内核机制参考 | https://github.com/dofastted/vm2api/releases/tag/v1.3.206 |
| 参考提交 | `894a78daba0361ecf825e5ec4a0707108cdbae9b` |

v2.15.0 是带注释标签，上述 tag 对象与其指向的发布提交是不同对象；功能分支从发布提交直接创建。已核对本地标签与远端一致。

本分支使用独立 Git worktree。未以已合并上游的本地定制分支为起点，未复制本地未提交修改。原工作区及其未提交内容原位保留。

本阶段只纳入本文件与 [账号 VM 内核模式方案](vm-kernel-account-mode-plan.md)。不添加或修改业务代码、迁移、依赖清单、锁文件、开发脚本、部署配置或服务数据。

## 2. 功能边界

- 保持纯上游 v2.15.0 已有功能。
- 从“账号管理 → 添加账号”增加普通模式/VM 内核（KVM）模式，默认普通；不新增账号管理系统。
- VM 首版固定单机 Linux amd64 + 可用 /dev/kvm，独立 Linux guest 内核与稳定 SMBIOS/MAC/磁盘序列号。Docker 仅封装 QEMU runner；不以普通 Docker slot、TCG 或宿主推理代替。
- 只有选择 VM 模式的账号才创建专属 KVM guest 并进入 guest → 真实推理内核链路。原选账号算法、授权刷新、代理配置、用量和计费继续复用 sub2api，仅新增必要的执行/就绪检查和凭据投影。
- 本次必交付原 Responses WS 入口到 Codex guest 的连续多轮、连接内 previous_response_id 展开、工具/取消和逐轮计费；跨协议包括 Chat/Messages→Codex Responses、Chat/Responses→Claude Messages，均覆盖 JSON/SSE。复用原 WS hooks 与转换器，补齐必要的 VM partial 用量提交接点，不新增会话平台或计费系统。
- 不以通用 HTTP 转发器代替用户要求的内核执行。
- 不在本阶段移植原本地分支的定制功能，也不把 VM 功能的限制施加到普通账号。

本阶段完成标准：分支来源可核对、独立工作区可用、方案已进入版本控制、相对发布基线只有文档变化。VM 功能实现与真实内核验证均未开始。

## 3. 已核对的开发要求

| 项目 | 上游要求/本机观察 | 当前证据边界 |
|---|---|---|
| Go | backend/go.mod 与 CI 使用 1.27.3；本机已有该工具链 | 只核对版本，没有编译项目 |
| Node | 上游 CI 使用 Node 20；本机已准备 Node 20.20.2 | 没有向仓库添加版本配置 |
| pnpm | 上游 CI 使用 pnpm 9；本机已核对 9.15.9 可用 | 没有安装本工作区前端依赖或改锁文件 |
| 本机架构 | macOS arm64 | 不能代替 Linux amd64 内核运行证据 |
| 真实内核环境 | 首版必须 Linux amd64、实际可用 /dev/kvm、固定 QEMU/OVMF/Ubuntu 24.04 guest 与 payload | 尚未验证 KVM VM 创建、guest 硬件身份、relay、凭据及真实请求；本机 macOS arm64 不能代替这些证据 |

分支初始化时短暂创建的开发辅助脚本与版本配置已按“先不用写代码”的要求移除，没有纳入分支。本次深审也只修改文档。本工作区没有执行依赖安装、Go 测试、项目构建或 Docker/KVM 集成测试，也没有启动或重启业务服务。

## 4. 纯上游与前期分析的差异

前期分析使用过本地已合并版本。切换为纯上游后，以下结论必须重新限定：

1. 本基线没有本地 `ticket_borrow*` 模块及其 `PrepareBorrowHTTPReplay/PrepareBorrowHTTPContinuation` 前处理。实施方案不应为了本地定制链路新增兼容代码。上游已有 harvest、插件、WS 等能力仍需保留，并分别检查 VM 模式兼容性。
2. 普通账号创建已有初始准入逻辑：`buildAccountForCreate` 中 `Schedulable` 取决于 `InitialAdmissionPlan`。VM readiness 应叠加到现有准入条件，不覆盖它；普通模式不能被改成无条件可调度。
3. 纯上游 `backend/internal/repository/integration_harness_test.go` 的 `TestMain` 仍在登记容器清理 defer 后直接 `os.Exit`。该结构会跳过清理；原本地版本中的 helper 修复不属于本分支基线。本阶段只记录，不修改测试代码。
4. 集成测试在非 CI 且 Docker 不可用时可能成功退出但跳过测试；后续验收必须核对实际执行，不能只看退出码。

## 5. 后续实施纪律

当前不执行以下工作；这些要求供后续实现和验证使用。

- 开始实际修改前重新检查分支、HEAD、工作区及上游变动。迁移、模型、接口等以本工作区源码为准，不沿用旧工作区行号。
- Go 测试/构建使用每轮独立的临时 GOCACHE、GOTMPDIR，同轮复用，退出时清理并核对；保留 GOMODCACHE。
- 构建前检查剩余空间，结束后记录空间及缓存清理结果；不重复运行没有新增验证价值的构建。
- Docker 集成测试记录本轮 Testcontainers session；自建内核实验使用明确的项目/会话标签。结束后核对实际容器，无相关进程和连接时才清理明确归属本轮的残留。
- 测试报告、凭据、home、VM 磁盘、数据库、数据卷、ledger、备份不是编译缓存；不对共享 Docker 环境执行无范围清理。
- KVM overlay、OVMF VARS、hardware identity、cloud-init instance-id 和仍被 overlay 引用的基础镜像是实例状态/依赖，不随构建缓存清理。禁止照搬上游 base.id/disk_gb 变化就删除 overlay 的逻辑；停机/测试收尾核验 runner 与 QEMU 均结束。
- 当前部署产物及必要的一个回滚版本按需保留，不清理包含业务记录的整个 .dev 目录。
- 不因本地提交自动推送、部署或执行迁移。

## 6. 后续入口

后续实现从方案中的 K1 开始：在真实 KVM 环境验证独立 guest、硬件身份、磁盘持久化、host→guest relay 和 guest 内固定版本推理内核，同时取得 WS/转换所需的多轮/工具/usage 样本。Claude/Codex 分别验证，Codex 需要新增 guest 服务；不以宿主 spawn 验收。确认凭据单一权威、guest 应用版本、流/usage/取消后，按 K2–K4 接内核、运行支撑和原生 HTTP，K5 完成四条转换，K6 完成 WS，K7 做全矩阵验收。缺 WS 或任一约定转换链不能算本次迭代完成。当前文档不构成已经完成这些验证的声明。
