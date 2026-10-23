# VM 内核账号模式：分支基线与工作边界

日期：2026-10-22。当前阶段：用户已授权实现，独立分支中已加入 VM 账户模式、运行支撑、HTTP/WS 接入与定向测试；真实 KVM/payload 验收仍未完成。用户指定首版为 KVM 真虚拟机，并要求 WS 与跨协议转换在本次迭代交付。

2026-10-22 实验后更新：分支与纯上游基线不变，现有实现保留。本轮完成 [本地 vm2api 创建/启动/清理实验](vm2api-local-kvm-experiment.md)，后续运行时工作遵循 [调整计划](vm-kernel-runtime-adjustment-plan.md)：先解决 guest 启动并验证 payload，再收敛为复用上游运行链；显式 TCG 仅作为建议新增的测试路径。普通模式、WS/跨协议与原计费边界不变，当前仍未完成真实服务链验收。

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

早期仅文档范围已由后续实施授权取代。当前实现与验证边界见 [实施状态](vm-kernel-implementation-status.md)，未部署业务服务或执行生产迁移。

## 2. 功能边界

- 保持纯上游 v2.15.0 已有功能。
- 从“账号管理 → 添加账号”增加普通模式/VM 内核（KVM）模式，默认普通；不新增账号管理系统。
- VM 首版固定单机 Linux amd64 + 可用 /dev/kvm，独立 Linux guest 内核与稳定 SMBIOS/MAC/磁盘序列号。Docker 仅封装 QEMU runner；不以普通 Docker slot、TCG 或宿主推理代替。
- 只有选择 VM 模式的账号才创建专属 KVM guest 并进入 guest → 真实推理内核链路。原选账号算法、授权刷新、代理配置、用量和计费继续复用 sub2api，仅新增必要的执行/就绪检查和凭据投影。
- 本次必交付原 Responses WS 入口到 Codex guest 的连续多轮、连接内 previous_response_id 展开、工具/取消和逐轮计费；跨协议包括 Chat/Messages→Codex Responses、Chat/Responses→Claude Messages，均覆盖 JSON/SSE。复用原 WS hooks 与转换器，补齐必要的 VM partial 用量提交接点，不新增会话平台或计费系统。
- 不以通用 HTTP 转发器代替用户要求的内核执行。
- 不在本阶段移植原本地分支的定制功能，也不把 VM 功能的限制施加到普通账号。

初始化阶段的分支与文档工作已结束。当前存在功能代码与迁移 296；完成标准仍要求真实 KVM、固定 payload、全部约定协议及生命周期验收，现阶段不能宣布整期交付完成。

## 3. 已核对的开发要求

| 项目 | 上游要求/本机观察 | 当前证据边界 |
|---|---|---|
| Go | backend/go.mod 与 CI 使用 1.27.3；本机已有该工具链 | 已使用独立临时 GOCACHE/GOTMPDIR 执行定向测试；详细结果见实施状态 |
| Node | 上游 CI 使用 Node 20；本机已准备 Node 20.20.2 | 没有向仓库添加版本配置 |
| pnpm | 上游 CI 使用 pnpm 9；本机已核对 9.15.9 可用 | 已按冻结锁文件安装依赖并验证；未改锁文件 |
| 本机架构 | macOS arm64 | 不能代替 Linux amd64 内核运行证据 |
| 真实内核环境 | 首版必须 Linux amd64、实际可用 /dev/kvm、固定 QEMU/OVMF/Ubuntu 24.04 guest 与 payload | 尚未验证 KVM VM 创建、guest 硬件身份、relay、凭据及真实请求；本机 macOS arm64 不能代替这些证据 |

分支初始化时遵守了当时的仅文档要求；后续实现授权后已安装前端依赖、执行 Go/前端测试与 helper 构建。UDS/HTTP/WS 自动化使用真实本机 socket 配合模拟内核，不是 Docker/KVM/payload 验收。没有启动或重启业务服务。

## 4. 纯上游与前期分析的差异

前期分析使用过本地已合并版本。切换为纯上游后，以下结论必须重新限定：

1. 本基线没有本地 `ticket_borrow*` 模块及其 `PrepareBorrowHTTPReplay/PrepareBorrowHTTPContinuation` 前处理。实施方案不应为了本地定制链路新增兼容代码。上游已有 harvest、插件、WS 等能力仍需保留，并分别检查 VM 模式兼容性。
2. 普通账号创建已有初始准入逻辑：`buildAccountForCreate` 中 `Schedulable` 取决于 `InitialAdmissionPlan`。VM readiness 应叠加到现有准入条件，不覆盖它；普通模式不能被改成无条件可调度。
3. 纯上游 `backend/internal/repository/integration_harness_test.go` 的 `TestMain` 仍在登记容器清理 defer 后直接 `os.Exit`。该结构会跳过清理；原本地版本中的 helper 修复不属于本分支基线。本阶段只记录，不修改测试代码。
4. 集成测试在非 CI 且 Docker 不可用时可能成功退出但跳过测试；后续验收必须核对实际执行，不能只看退出码。

## 5. 后续实施纪律

以下纪律已用于实施与验证，并继续约束后续真实 KVM 验收。

- 开始实际修改前重新检查分支、HEAD、工作区及上游变动。迁移、模型、接口等以本工作区源码为准，不沿用旧工作区行号。
- Go 测试/构建使用每轮独立的临时 GOCACHE、GOTMPDIR，同轮复用，退出时清理并核对；保留 GOMODCACHE。
- 构建前检查剩余空间，结束后记录空间及缓存清理结果；不重复运行没有新增验证价值的构建。
- Docker 集成测试记录本轮 Testcontainers session；自建内核实验使用明确的项目/会话标签。结束后核对实际容器，无相关进程和连接时才清理明确归属本轮的残留。
- 测试报告、凭据、home、VM 磁盘、数据库、数据卷、ledger、备份不是编译缓存；不对共享 Docker 环境执行无范围清理。
- KVM overlay、OVMF VARS、hardware identity、cloud-init instance-id 和仍被 overlay 引用的基础镜像是实例状态/依赖，不随构建缓存清理。禁止照搬上游 base.id/disk_gb 变化就删除 overlay 的逻辑；停机/测试收尾核验 runner 与 QEMU 均结束。
- 当前部署产物及必要的一个回滚版本按需保留，不清理包含业务记录的整个 .dev 目录。
- 不因本地提交自动推送、部署或执行迁移。

## 6. 后续入口

下一阶段必须补足方案中的 K1 真实环境验收：在真实 KVM 环境验证独立 guest、硬件身份、磁盘持久化、host→guest relay 和 guest 内固定版本推理内核，同时取得 WS/转换所需的多轮/工具/usage 样本。Claude/Codex 分别验证，Codex 需要新增 guest 服务；不以宿主 spawn 验收。K2–K6 已有实现和本机测试；这些证据不能替代 K1。确认凭据单一权威、guest 应用版本、流/usage/取消后，再按 K7 做真实全矩阵验收。缺 WS 或任一约定转换链不能算本次迭代完成。当前文档不构成已经完成这些验证的声明。

## 7. 已落实的实施决策

数据库只新增 accounts.execution_mode（迁移 296）；runtime 使用 helper 私有 manifest、generation、身份与删除 tombstone。本机协调器保留私有 flock。当前并发跟随原账号配置；2FA、初始准入和质量文本流程复用原业务能力，推理转交 guest；HTTP/WS 通过受控共享完成历史续接。账号删除在数据库成功后立即撤销 runtime 新请求权限，后台排空并清理实例；磁盘与 tombstone 保留。详见 [业务复用修订说明](vm-kernel-business-reuse-20261022.md) 与 [实施状态](vm-kernel-implementation-status.md)。
