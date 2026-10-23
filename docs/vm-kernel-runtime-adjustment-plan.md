# VM 运行时调整计划

> 历史阶段记录：本文下列运行结果和阶段限制仅适用于记录时源码，不是本次业务复用修复的验收结论。固定并发 1、禁止 2FA 初始化、仅连接内历史等旧限制已调整；当前边界见 [业务复用修订说明](vm-kernel-business-reuse-20261022.md) 与 [实施状态](vm-kernel-implementation-status.md)。硬件 KVM 仍未验收。

本文记录运行时调整顺序与当前落地边界。2026-10-22 已完成原生 runner、CPU 兼容、exec 退出码及 overlay 保留修复，并取得空 guest 的真实内核健康响应；见 [修复实测记录](vm-kernel-runtime-fix-verification.md)。后续Codex guest、私有Go client和`gpt-6-luna`真实推理已通过，见 [Codex实测](vm-kernel-codex-guest-verification.md)；Go helper 尚未接入新运行包，完整业务链路未验收；原失败过程保留在 [首次实验报告](vm2api-local-kvm-experiment.md)。

本文调整后续实施顺序与运行时复用方式。[实施状态](vm-kernel-implementation-status.md) 保留既有源码和测试记录，其中自建离线模板、KVM-only driver 及人工 verified 标记属于当前实现现状，不能直接当作最终部署流程。[账号模式方案](vm-kernel-account-mode-plan.md) 中的账号、WS、跨协议、usage、刷新与生命周期边界继续有效，须逐项以真实运行验证。

## 本轮已取得的证据

固定参考版本为 vm2api v1.3.206。首次实验确认严格模式会拒绝缺少 `/dev/kvm` 的执行端；显式允许 TCG 后能创建 guest，但 ARM64 Docker 执行端运行 amd64 QEMU 时出现 BOOT/UEFI 等待超时。停机读取已确认分区和标签存在，不能归因于标签缺失。

本轮保持同一磁盘、资源及 QEMU/OVMF 包版本，改用原生 ARM64 QEMU 后约61秒完成 guest 初始化，SSH及挂载成功。随后发现默认 qemu64 CPU 下 CLI 报 SIGILL；只将 CPU 改为 max 后，真实内核 HTTP 200、healthy=true、ready_slots=20、cli_restarts=0。最终带补丁镜像仍传默认 qemu64，入口按明确规则映射 max，再次启动和健康验证通过，身份与磁盘保持。加速器观测为 TCG / KVM enabled=false。

`kin-guest-exec` 的失败退出码问题已修复，真实 guest exit 0/7 传递正确。overlay 遇到 base/容量/状态异常时不再自动重建。4组针对性测试共20项通过，详见修复实测记录。

空槽原始健康仍为 `ok=false, credential_state=missing`，没有导入凭据。代理字段为 configured=false、required=false；因此本轮没有凭据消费、真实推理、强制代理或计费的成功证据。原 vm2api 的20个slots不改变 Sub2API 单执行槽约束。独立 runtime PoC不等于现有 Go helper 已支持该镜像。

## 调整方向

已在功能分支加入固定来源的 `deploy/vm-kernel/upstream/` 运行包及局部修复，并通过原 vm2api 的健康调用链完成独立 guest 实测。继续复用官方 runner、guest bootstrap 和匹配 payload，收缩自建 QEMU 启动链的维护范围。下一阶段按 [上游 runner 适配合同](vm-kernel-upstream-adapter-contract.md) 对接现有 helper，不以替换镜像名代替接口适配。

保留 Sub2API 业务适配层：不可变账户 execution_mode、强制代理和单执行槽、管理员状态与运行时 readiness 分离、原账户选择器、Forward、公开 WS、用量提交和计费入口。避免另建调度或计费系统。

保留 helper 的生命周期接口以及 account/instance/generation/guest boot identity 校验、配置 revision、drain/delete 顺序与持久身份。优先替换或调整其内部 DockerDriver、自建 runner、guest setup 与 relay，使其调用已验证的上游运行链。是否能直接复用上游挂载和通信方案，需通过路径、权限、半关闭、取消与流 trailer 实验证明。

目标请求路径固定为：**原账号选择与准入 → VM binding 最终检查 → 原协议转换/VM 执行适配 → 私有 kernel client → runner relay → guest 推理内核 → 原结果处理与 RecordUsage**。普通模式继续使用原执行分支；VM 模式只替换已选账号的执行环节，不接入 vm2api 的面板、账号池、scheduler 或计费器。

## 执行模式边界

首版硬件 KVM 交付仍要求 Linux amd64 且具备可用 `/dev/kvm` 的执行端。启动前检查设备、权限、架构和资源，并通过 QMP 确认实际 KVM enabled；失败应明确拒绝以硬件 KVM 身份创建。设备检查以实际 Docker 执行端为准，不能只检查控制面进程所在系统。

上游运行包已实测显式 TCG 路径；Sub2API helper 的测试策略及实际加速方式字段尚未接入，后续加入时默认关闭。它沿用同一个账号 VM 模式，在部署配置与状态中区分实际加速方式，不再增加第三个账号创建模式。控制面必须区分用户请求的模式和实际加速方式，展示软件模拟状态；不得将其用于 KVM 性能或硬件加速能力验收。测试结束恢复临时配置，并按测试归属清理实例。

普通 Docker 运行模式是否属于产品范围需要单独界定，其隔离、持久身份、网络与调度契约并不等同于 guest VM。不能把它作为 KVM 不可用时的默认降级方式。

首版产品入口维持“账号管理 → 添加账号 → 普通（默认）/VM 内核”。平台、凭据、分组、代理、授权刷新和账单仍使用现有系统；不增加节点、镜像市场或另一份 VM 账号。账号创建负责触发准备，runner/guest/kernel/原账号准入分别通过后才能调度；测试模式及失败原因在现有管理状态中呈现。

## P0：启动与 Codex 私有调用已验证，helper 接入待完成

运行包已完成四项修复：按 Docker 服务端选择原生工具架构、固定 payload 的 CPU 兼容映射、guest-exec 退出状态传播、overlay 状态保留。SSH、BOOT/UEFI和共享挂载、真实 kernel health、重建后的持久身份均已验证。本机实际是TCG，硬件KVM仍需要可用的执行端。

已使用明确授权账号完成2FA、访问令牌投影、Codex native请求和实际Go client调用，取得完整终端及权威用量。代理不可用时返回502，尚未证明网络防火墙约束；真实控制面刷新后的revision消费、Claude授权请求和新driver接入仍待完成。现有 Go helper 的 KVMEnabled、凭据revision、boot identity和业务准入条件不能因为空槽健康已通过而删除。不能直接把人工 verified 标记置 true，也不能复制现有账号凭据或转换其模式来凑验收。

## 前置 PoC 与退出条件

| PoC | 必须取得的证据 | 未满足时的处理 |
| --- | --- | --- |
| Guest 启动 | 固定 runner 启动、guest boot identity、内部 kernel health、实际 accel | 保持不可调度，不进入业务接入验收 |
| Codex 进入 guest | payload/loader/启动参数、CLI 路径和依赖、内部 Responses 路由均在 guest 内工作 | 暂不宣称 Codex VM 支持 |
| 凭据与刷新 | Claude/Codex 分别验证凭据 schema、过期单位、读取路径、revision 重载；确认 refresh/writeback 责任和文件位置 | 不以健康检查或文件摘要代替真实消费证据；保留 Sub2API 原有自动刷新，不引入 guest 第二刷新者 |
| 强制代理 | guest 实际出口符合指定代理；代理失败时请求失败；DNS 和直连行为明确 | 不进入可调度状态 |
| 权威 usage | 真实流的 response_id、完成事件、输入/输出/缓存用量、部分成功用量与错误语义均有脱敏证据 | 不沿用未经验证的字段映射计费 |
| 取消与中断 | 取消接口、断开、半关闭、trailer 和任务释放可重复验证；已派发请求不自动重放 | 保持保守失败及隔离策略 |
| WS 连续轮次 | 原公开 WS 路径的 previous_response_id、工具/推理续接、每轮原始与有效 payload、一次用量提交 | 不开放未经验证的会话恢复或历史重放 |
| 生命周期 | 重启保留身份，配置更新更换有效 generation，删除先 drain，旧连接失效 | 不进入上线验收 |

原 Sub2API 自动刷新机制必须保留，normal 模式不受影响。VM 模式由原控制面统一负责刷新与新 revision 下发，guest 是否存在隐式刷新或写回必须通过 PoC 查明并消除双刷新竞争。

凭据 PoC 需使用明确授权的账号与最小请求，日志只保留脱敏元数据。后续已完成授权Codex访问令牌消费与真实推理/usage；刷新后新revision、两平台完整协议与计费仍待验证。

Codex 必须单独验证进入 guest。固定上游 guest 启动脚本执行的是 `kin-kernel --gateway-worker`，不能用现有控制面 Codex 进程来证明 Codex guest 支持；需核对 `kin-codex-kernel`、CLI/loader 和 guest 内部路由。最终仍须交付两平台、12 个 HTTP 形态及 Responses WS，不能把一个平台的原生请求成功扩展成全部协议已完成。

## 现有文件的最小调整清单

运行包的 P0 修复已经落地，其余下表为后续接入任务；旧 `deploy/vm-kernel/` 自建合同和 Go helper 仍保持原实现。各文件仅由一名 Agent 修改，真实实例由运行时负责人串行管理。

| 任务与文件 | 保留与调整内容 | 可检验的完成条件 |
| --- | --- | --- |
| 运行物料：已新增 `deploy/vm-kernel/upstream/`；旧目录的 `Dockerfile`、`build-template`、`runner`、`guest/*` 待按适配合同收敛 | 固定上游 runner/bootstrap/payload，改为发布时提供可直接获取的固定制品；收缩重复启动脚本。对 guest 启动问题、exec 退出码及 overlay 数据保护作有证据的最小补丁 | 固定 digest 可复现，空 guest SSH 与 kernel health 通过；没有因 base ID/容量变化自动删除既有盘 |
| Driver：`backend/internal/vmruntime/docker.go`、`contract.go`、`helper.go` | 保留受限接口、实例归属和持久状态；检查实际 Docker 执行端；增加明确的请求/实际加速信息，区分 KVM 与显式 TCG。将人工 verified 标记改为发布制品验证记录和自动运行检查；不直接把标记常量置 true | 缺能力时给出准确原因；QMP、guest boot ID、kernel ready 均有证据；TCG 不被报告为 KVM enabled；旧代连接被拒绝 |
| 配置与协调：`backend/internal/config/vm_kernel.go`、`backend/internal/service/vm_kernel_service.go`、`vm_kernel_coordinator*.go` | 默认功能关闭、普通模式不变；开发测试 TCG 默认关闭；保留 drain、generation、revision、停机和删除顺序 | 单实例准备/重启/修改/删除可重复，未知状态不创建重复实例或自动重放请求 |
| 凭据与平台：`backend/internal/service/vm_kernel_service.go`、`backend/internal/vmkernel/{client,types}.go`、`deploy/vm-kernel/guest/*` | 依据真实 payload 修正 schema、过期单位、读取路径、loader 与服务参数；仅由原 Sub2API 控制面刷新并下发配置 | 两平台分别证明真实凭据消费及更新；无第二刷新者、无凭据日志；代理故障时没有直连 |
| HTTP/转换/WS：`backend/internal/service/vm_kernel_{forward,protocol,ws}.go` 及原 Gateway/OpenAI Forward 接点 | 保留现有协议与 hooks；依据真实流修正完成事件、工具/推理、多轮 continuation、取消、trailer 与错误映射 | H01–H12 与 W01 使用真实 guest/payload 逐项通过；未知 attempt 不重放；不新增 WS 网关 |
| 用量与准入：原 `gateway_handler*`、`openai_gateway_handler.go`、`openai_chat_completions.go`、账号选择和最终发包检查 | 保留原调度与 RecordUsage；仅修正真实 payload 暴露的不匹配，沿用原成功/partial/unknown 语义 | 完整及 partial 请求各提交一次权威用量；禁用、限额、过期与 VM 未 ready 均不能被运行时状态绕过 |
| 管理呈现：`vm_kernel_status.go`、DTO、`CreateAccountModal.vue`、`VMKernelStatusBadge.vue` 与语言键 | 保留普通默认和现有表单；必要时增加枚举形式的实际加速/失败阶段摘要，不公开 token、路径或 socket | 用户能区分准备中、guest 启动失败、内核未 ready、TCG 与 KVM；普通账号页面和 API 兼容 |

上表存在共享文件时，应按阶段交接，不让两个 Agent 同时修改。新增代码验证只覆盖受影响行为；既有源码测试结果保留，但不代替真实运行矩阵。

## 可并行任务与依赖

| 最小任务 | 可并行范围 | 依赖与交付 |
| --- | --- | --- |
| P0. 原项目最小启动修复 | 已完成独立 runtime 启动修复，业务 PoC 待完成 | emergency/SSH/CLI/health 已实测通过；取得授权 native 请求证据后再替换驱动 |
| A. 固定运行物料 | 可与 B、C 的只读梳理并行 | 固定源 tag/commit、镜像平台 manifest digest、payload digest、实际 runner/bootstrap 源脚本 hash；记录匹配关系 |
| B. Helper 驱动适配设计 | 可先独立梳理接口，实施依赖 P0、A 和 guest 启动 PoC | 保留生命周期与 fencing，明确挂载、状态读取、停止/删除和异常回收映射 |
| C. 协议与凭据合同 | 可与 B 并行，但真实执行需使用受控实例，避免并发改写凭据 | 完成 Codex/Claude 凭据与刷新、内部路由、usage、取消的 PoC 和脱敏 fixtures |
| D. 业务接点调整 | 依赖 B、C 的有效合同 | 沿用现有 Forward/WS/计费，修正经实证不匹配的字段与状态，不扩展未经验证的能力 |
| E. 集成验收 | 依赖 A–D | 从账户创建到真实请求、计费、更新、重启和删除完成一轮；明确 KVM 与 TCG 分别通过的项目 |

并行任务按文件所有权拆分，驱动、协议与业务入口分别由单一负责人修改；共享接口变更先同步后实施。测试实例的生命周期和凭据变更由同一执行负责人串行操作，避免相互污染证据。

最小执行顺序为：**固定物料 → guest 启动与空槽 health → 一个匹配平台的 native 请求 → 两平台凭据/协议合同 → Driver 与业务接点适配 → HTTP/WS/用量和生命周期闭环**。生命周期设计可与协议只读梳理并行；准备、写凭据、重启和删除不得在同一实例上并发进行。P0启动和Codex私有真实请求已通过；原helper的revision、网络约束和完整业务合同未完成时，不把独立runner或私有client的成功提升为账号已可调度。

## 可复现性与验收收尾

运行物料验证由实现与发布方完成，并由产品启动时自动执行能力检查，产出可诊断的失败原因。用户无需手工制作离线模板，或设置两个验收开关来声称版本已经 verified。历史自建模板或 PoC gate 若保留作开发工具，不应成为普通用户部署的手工前置任务。

镜像 tag 可变，不能仅记录 `:24.04`。记录所用平台镜像 manifest digest，以及固定源码中 runner、guest bootstrap、相关配置生成脚本的 SHA256；payload 同样记录 digest 和对应 manifest 版本。若任一物料变化，重新验证受影响的合同。

修改前先检查磁盘空间。Go 测试与构建遵循项目要求使用本轮独立 GOCACHE/GOTMPDIR、同轮复用并保证退出清理，保留依赖缓存。只清理已确认属于本轮的测试容器、镜像和临时编译产物，保留业务数据、凭据与必要回滚版本；Docker 集成测试结束后按会话核对容器，而非只检查测试进程。

最终验收必须分别报告：源码与测试结果、物料身份、guest/加速方式、真实请求、代理约束、权威计费、生命周期以及清理结果。当前计划的退出条件是上述必要 PoC 已获得证据且集成链路通过；若环境只能完成 TCG，报告应明确仍欠真实 KVM 验收，不以部分启动成功关闭任务。
