# VM 内核账号模式：当前实现与验收状态

更新日期：2026-10-22。功能分支 `codex/vm-kernel-account-mode`，开发基线为纯上游 Sub2API v2.15.0（`8b36fe7d647c28e8189147c6bfce52a41b6e26db`）。机制参考固定 vm2api v1.3.206，提交 `894a78daba0361ecf825e5ec4a0707108cdbae9b`。

当前 `upstream-v1` 已接通 Go helper、独立 guest 与 Sub2API 账号模式。前序首版 GPT/Codex 的已列范围真实验收完成：24笔完成请求、24个唯一账单（基础8 + HTTP工具12 + WS工具2 + 两轮显式取消后的新连接恢复请求；断连不新增业务请求），input 47118、output 301、费用0.00545494、余额9.99454506。显式取消后generation 12→13、新连接恢复；断连后13→14，guest boot与进程证据更新并恢复ready；该断连场景没有再发成功业务请求。连续90秒十次ready均为true。host及guest均验证指定代理可通、直连TCP与DNS TCP53拒绝，IPv6/direct resolver关闭，access-only投影只读。原API停用后请求503且Docker停止；删除后账号404、容器不存在，tombstone/磁盘/身份保留且无新增账单。详细范围和证据见 [GPT验收记录](vm-kernel-gpt-acceptance-20261022.md)。这些结果不代表所有故障路径均已实测；硬件KVM、Claude仍未验收。

本次修复已恢复账号配置并发、原 2FA 初始化、原质量/准入流程及受控 HTTP/WS 跨连接续接。源码边界及逐轮结果见 [业务复用修订说明](vm-kernel-business-reuse-20261022.md)。真实 30 请求前四轮依次为 4/26、0/30、0/30、24/6（成功/失败），所有失败证据保留。最新第四轮三个本地峰值（client 在途、账号 lease、guest relay 连接）均为 30，24 条唯一账单、权威 usage 及余额对账一致，费用 0.01061946；仍有 6 条 HTTP 502，仅确认 guest `hop_err` 的 `sent=false`、`retry=true`、`error sending request`，缺少底层 source，不能称为具体超时，未达到全成功验收。第四轮前后 generation 14、guest boot 不变，ready 保持、结束 lease 全为 0。此证据不证明 provider 内部 30 路并行，也不代表硬件 KVM 性能；本轮使用显式 TCG。独立 post-load 首轮为 5/8 通过：HTTP/WS 续接与 Chat SSE usage 呈现失败；6 条唯一账单及余额一致。该失败报告保留。接点修复后 `post-run2` 完整 8 项为 5 通过、3 条上游连接 `hop_err` 失败，WS 跨连接续接成功；随后固定四项 `post-targeted` 全部通过，4 条唯一账单、费用 0.00025306、余额一致。八类功能分轮均有成功证据，不能称同一轮 8/8。上述第四轮 24 条与前序首版 24 条是不同批次，不混用账务证据。


## 当前源码边界

保留原账号、分组、API key、OAuth 刷新、代理、选择器及计费。`normal` 是旧账号/旧创建 API 默认模式，`vm_kernel` 是原账号的执行方式；不新建扣费系统或 runtime SQL 表。数据库迁移为 `296_account_execution_mode.sql`。helper 私有 manifest 保存身份、generation、磁盘与投影版本、删除 tombstone。

公开请求只使用已准备的 VM，不负责创建或启动；控制面负责生命周期及 access-token-only 投影，guest 不成为第二个刷新者。本地 guest/kernel 健康及当前 receipt 验证通过后发布 ready，不再自动调用模型作 VM 就绪探测；原账号业务准入独立保留。代理必填，并发跟随原账号配置；业务和管理 probe 共用按配置上限等待的运行时活跃计数，不重复获取 Redis lease。HTTP Responses 与 WS 共用有界完成历史，同用户、同分组、同账号、同 binding generation 可跨连接续接；未知/过期或跨归属历史拒绝，不承诺 guest 或服务重启透明恢复。

原管理员禁用、过期、分组、模型、配额及最终数据库/binding fence 保留。未知或部分完成请求不能自动重放，也不能切回普通模式隐藏错误。已知模型/provider 失败沿用原业务错误处理，不显示为账号 VM 故障，不触发实例重建。2FA 初始化沿用原 OpenAI 登录/凭据持久化流程；管理文本测试、初始准入及质量/Pelican/candy 文本请求走同一 guest 并复用原解析、评估和记账观察。宿主插件、BPS、SIWC、Prism 等其他执行通道及依赖 host ticket/harvest 的特定 Codex 状态探测仍不支持。

显式取消保留原 attempt 与已收到的权威 partial usage，返回 `response_cancelled`、`reconnect_required=true` 并关闭连接（1008）。保留 unknown quarantine；客户端等待 runtime.ready 后新建连接，不承诺原连接恢复或未完成历史可续接。

post-load 暴露的接点按原业务语义修复：HTTP 入口不再以普通 OAuth 的限制排除具有受控本地续接能力的 VM 账号；历史首次发布的正常 Redis miss 不再当成发布错误，真实缓存错误及所有权/账号/代际约束不放宽；VM Chat SSE 沿原转换规则打开 `IncludeUsage`，在权威终态输出 usage chunk。原验收脚本已经传入 `stream_options.include_usage=true`，缺 usage 是 VM 呈现遗漏，不是脚本漏参，也不是漏记账。接点修复已部署；`post-run2` 与 `post-targeted` 的分轮结果及失败保留见业务复用说明。

最新应用镜像为 `sha256:6a54d2868d8b…`，二进制 SHA-256 为 `de2b8f867d8d…`，部署健康及完整摘要见 `minimal-fix/deployment-settled.json`。第四轮 30 压测发生在此前并发/状态修复版本，最新接点补丁通过后两轮功能验收，未重跑 30 路。不能把功能分轮成功视作网络波动已经消失。

## 当前部署机制

当前部署入口为 [部署 README](../deploy/vm-kernel/README.md) 与 [upstream adapter 合同](vm-kernel-upstream-adapter-contract.md)。顶层 legacy 自建 runner、离线模板及双 verified gate 是历史路径，不是 upstream-v1 用户必须执行的步骤。

- 使用固定不可变 registry digest 或本地 image ID；runner 工具架构匹配 Docker server。固定 guest/payload 为 x86_64，完整 payload 根含 `usr/local/bin/kin-codex-kernel` 及所需 loader/libs。
- 默认 KVM；TCG 必须同时配置模板 acceleration=tcg 和 helper --allow-tcg，不自动降级或伪造 KVM enabled。Linux root helper 管理 Docker；主应用非 root、不挂 Docker socket。
- 使用专用 service-gid 连接 helper/实例 socket，父路径授予必要 group-x，私密文件维持0600。部署预创建应用 UID 所有的0600 coordinator lock，保留 inode，各进程挂载同一父目录及绝对路径。不能仅挂 socket 文件。
- Apply 最多480秒；单账号后台任务独立十分钟，四个 bootstrap 许可跨轮询共享，满额下轮重试；独立十六个健康观察许可避免慢启动挡住已有实例刷新。
- receipt 绑定 generation、guest boot、凭据/代理 revision、摘要与实际内核进程。内核 Restart=no，恢复必须经过控制面版本约束；relay 重启不能掩盖内核身份变化。
- 旧 overlay/身份/启动布局不自动迁移。升级先明确退役排空旧绑定，再按发布流程更新；删除不把持久磁盘、凭据或测试记录当缓存清除。

## 真实接入暴露并修复的问题

scheduler metadata 会剔除 access token 和 Proxy；直接对精简字段计算 revision 会让 ready=true 的账号仍被过滤。当前投影携带权威完整账号生成的专用 credential/proxy 摘要，明确 metadata 标记，仅内部缓存使用；旧 VM metadata 缺摘要时 miss/reload。完整账号仍现场校验，执行前继续读取数据库，没有绕过 revision gate，也没有把秘密加回 metadata。

helper inspect 只在控制面健康观察中执行，业务 Execute 不再逐请求 inspect，使用三十秒有效的本地就绪快照；业务完成不会刷新健康观察时间。控制锁仍只在账号已 ready 时最多等待十秒且可取消，推理不持有控制面锁。达到账号配置并发上限时等待任一请求结束，再校验最新账号状态；派发前的最终数据库、credential/proxy revision、路由及 session binding fence 保留。等待发生在 dispatch 之前，不发送或重放推理。

第二轮的 502 根因为 cloud-init 覆盖 guest `/etc/hosts`，代理域名映射在禁用 DNS 环境中丢失；同一代理通过 IP 直连可完成 CONNECT 200 与 TLS。guest setup 现排在 cloud-init 之后，seed 禁止覆盖 hosts，apply/observe 校验实际解析投影而非仅相信配置 revision；就绪校验不发模型请求。

第三轮 0/30 来自临时部署 stdin 传输失败留下空 guest 网络脚本，不能判定源码 hosts 修复无效。部署改为参数传输和 SHA 校验后，源码/镜像/seed/guest 一致，apply/getaddrinfo 与单次公共业务 HTTP 200/completed/usage smoke 通过。证据为 `minimal-fix/guest-network-verified.json` 与 `network-business-smoke.json`；第四轮已恢复 24 条真实成功，剩余 6 条失败继续诊断。

另一个独立错误是完整非 200 返回曾被当作 unknown，导致 generation 12→13 的误重建。当前限定 64 KiB 内完整读完的固定同步内核非 200 响应标记 `Settled=true`，只表示本地 handler 结束，`SafeToRetry=false` 不变。读取失败、截断、超限或其他未确认执行仍按 unknown 处理，不自动重放。

对应回归包括 `TestSchedulerVMMetadataKeepsOnlyRevisionFences`、`TestVMKernelSchedulerMetadataRevisionFences`、`TestVMKernelAdmissionWaitsForHealthyControlObservation`、`TestVMKernelAdmissionControlWaitHonorsCancellation`；这些源码回归不能替代最终部署验证。

## 组件地图

| 组件 | 当前主要文件 |
| --- | --- |
| 账号/API/数据库 | `account_execution_mode.go`、`admin_account.go`、`account.go`、迁移296、admin account handlers |
| 精简调度投影 | `vm_kernel_scheduler_projection.go`、`scheduler_cache.go` |
| 运行协调与生命周期 | `vm_kernel_service.go`、`vm_kernel_coordinator*.go` |
| helper 与 upstream 驱动 | `backend/cmd/vm-runtime-helper/main.go`、`backend/internal/vmruntime/{helper,http,docker,upstream}.go` |
| 当前 runner/guest | `deploy/vm-kernel/upstream/` |
| 单 attempt 私有客户端 | `backend/internal/vmkernel/{client,types}.go` |
| 原入口适配 | `vm_kernel_{forward,protocol,ws}.go` 与原 Gateway/OpenAI handlers |
| UI 与脱敏状态 | VMKernelStatusBadge、Create/EditAccountModal、`vm_kernel_status.go` 及显式 DTO mapper |
| 可执行真实验收 | `deploy/vm-kernel/tests/full_acceptance*.py`，参见 [验收清单](vm-kernel-full-acceptance-checklist.md) |

runtime token、socket、投影与代理秘密不进入公开 DTO 或报告；本文件不包含运营凭据或测试服务身份信息。

## 已有证据及其边界

[Codex guest 验证](vm-kernel-codex-guest-verification.md) 记录固定 payload、原生 runner、guest relay 和私有 Go client 的真实 `gpt-6-luna` 请求。早期 [运行时修复记录](vm-kernel-runtime-fix-verification.md) 属于独立 runner 阶段的历史证据，其“尚未接 helper”等阶段描述不能代替当前状态。

UDS fixture 使用真实 socket/HTTP/SSE/WS，但模拟 helper/kernel，覆盖三入口×两provider×JSON/SSE十二种形态与 WS 多轮。主要测试：`TestVMKernelForwardUDSTwelveCombinations`、`TestVMKernelPresentationTwelveHTTPShapes`、`TestVMKernelWSUDSReplayHooksAndUsageOnce`。GPT 真实验收无需等待 Claude；Claude fixture 通过不表示真实 Claude 已可用。

partial/取消权威用量边界由 `TestVMKernelForwardPartialIsAuthoritativeAndNeverFailover` 和 `TestVMKernelWSUDSCancelKeepsAttemptAndPartialUsage` 覆盖。未知、重叠与非法 continuation 的源码/fixture 约束不标成已完成专项真实验收。真实取消可能未收到 usage，零账单不能证明所有 partial-usage 路径。

## 历史验证记录（保留原轮次，非最新全量复跑）

以下为前序开发轮次的原记录。时长、数量和通过状态只适用于当时源码及测试范围；之后的 upstream 接入、调度/取消修复由对应定向验证和最终验收记录补充，不据此宣称最新源码全量重跑。

| 验证 | 已确认结果 | 能证明什么 / 不能证明什么 |
|---|---|---|
| service `TestVMKernel` 全套初次运行 | PASS，1.268s | 含 selector、presentation、真实 UDS/WS fixture；后续新增 lifecycle 由最终全包与 race 覆盖；不是 KVM |
| partial 整合测试与 client 重复 `-count=20` | 初次暴露 flaky；定位 fixture 未读完请求 Body 就关闭 UDS，cause 为 `write: broken pipe`。修复共享 fixture 消费完整 Body 后通过：service 1.686s、client 0.423s | 纠正测试设施，不修改 production retry 语义；不是固定 payload 验证。后续最终全包与 VM 专项 race 也通过 |
| helper 测试 | 13 项含 race 通过 | 持久身份、fencing、投影、删除意图、缺 PoC 拒绝等；driver 为模拟实现，不是实际 QEMU |
| kernel client | 最后确认 PASS，0.482s | socket/流/usage/cancel/dispatch 边界；不证明实际 payload |
| config/admin/repository mode tests | PASS | env/default、配置拒绝、API 兼容、mode 缓存边界 |
| DTO 与 scheduler cache 相关回归 | PASS | 脱敏映射、旧缓存 miss、metadata/full-cache、快照 fencing 等 |
| 前端 CreateAccountModal / locale completeness / VMKernelStatusBadge | 93 tests PASS | normal 默认、VM import/代理/并发、禁止 API key、状态显示与语言键 |
| 前端 `vue-tsc --noEmit` 与 production Vite build | PASS；Vite 1m39s，常规大 chunk warning | 类型/生产构建检查；未做完整真实浏览器/KVM交互验收 |
| helper Linux 静态构建 | PASS | 构建产物可生成，不证明目标环境可运行 |
| Windows amd64 完整 server 构建与 helper Windows 包编译 | PASS | 普通部署跨平台编译兼容，不提供 Windows VM 能力 |
| Linux amd64 server 构建 | PASS；最后控制面及插件边界修改后的复核也通过 | 不能替代 Linux KVM 运行 |
| VM service + kernel client race | PASS：service 3.183s、client 1.422s | `-race` 定向覆盖 `^TestVMKernel|^TestClient`，不是全 service race |
| `git diff --check` | PASS | 空白检查 |
| 普通业务 8 包完整 suite | 8 包均通过。service 最终完整重跑 11647 个 pass 事件、3 skip、0 fail；repository 最终 1439 个 pass 事件、0 fail（计数均含子测试） | service 跳过原有真实 moderation/API token 对照/plugin runtime 外部集成；未运行 PostgreSQL/Redis integration build tag。完整 JSON 报告在 `.dev/vm-kernel/verification/`，旧失败记录保留用于追溯 |
| plugin metadata/账号目录回归 | 定向 PASS，1.860s | ExecutionMode 归类为非秘密 metadata；VM 从插件目录和 outbound identity 解析排除，阻断 host plugin 出站 |


前序轮次使用临时 GOCACHE/GOTMPDIR，保留依赖与测试报告，未使用全局 go-build 积累产物。该轮清理后剩余空间约 106.87 GiB 是历史读数。前序首版独立真实验收和清理均已完成，当时收尾剩余 107.47 GiB；测试容器与网络已移除、测试凭据副本已撤除，数据库卷、账单和 VM 磁盘保留，详见 [GPT 验收记录](vm-kernel-gpt-acceptance-20261022.md)。

## 当前验收与发布要求

前序首版真实验收结果以 [GPT验收记录](vm-kernel-gpt-acceptance-20261022.md) 为准；本次修复和 30 路真实并发必须另有当前产物及账务证据。未列出的故障路径不推定通过，历史fixture和真实证据继续分层。

本轮 `business-reuse-20261022/minimal-fix/settled-final.log`、`settled-race.log` 中 vmkernel/service 定向及竞态验证通过；最终接点的 `presentation-final.log`、`continuation-final.log` 中 service/handler 定向验证通过。`proxy-hosts-fix-20261022.json` 记录运行包 Python 53 项通过、追加 receipt 回归后 bridge 9 项通过；这是源码回归证据，不能将第四轮 24/30 的结果表述为全成功。

默认 VM_KERNEL_ENABLED=false。启用使用 VM_KERNEL_HELPER_SOCKET、VM_KERNEL_TEMPLATE、VM_KERNEL_CPU、VM_KERNEL_MEMORY_MIB、VM_KERNEL_DISK_GIB；完整权限、模板和挂载步骤以部署 README 为准。单机完整 Sub2API coordinator 是当前支持边界，不能把本地锁当分布式调度能力。

回滚旧二进制前应持久禁用 VM 账号、排空并处理快照，不能仅关闭 VM 开关让旧代码将其当 normal。前序首版构建缓存和独立测试容器已清理，数据库、请求记录和 VM 磁盘保留。本次构建和验收的清理、部署及通过状态以本轮报告为准，不沿用历史完成声明。

本轮最终状态见 `business-reuse-20261022/minimal-fix/final-state.json`：账号 active、schedulable、并发 30；VM ready=true、实际 TCG、generation 14 与 guest boot 不变；应用 healthy、restart count 0。`cleanup.json` 确认独立构建缓存已清理、宿主剩余 106.91 GiB、无运行中的 Testcontainers；数据库、账单、请求证据、VM 磁盘及当前部署保留。收尾不继续生成测试请求，不将剩余上游连接波动描述为已解决。

## 前序首版 WS 历史边界专项记录

前序首版为修复首帧 ID 被剥除的问题，曾统一拒绝首帧 previous_response_id；下列 close1008 是该阶段历史证据。本次改为验证归属、账号及 binding 后从共享完成历史重建完整请求，未知 ID 仍拒绝，已授权同归属跨连接 ID 不再一概拒绝。

安全 `ws_tools-result.json` 已确认两轮 WS 工具 marker、2条usage和余额一致；unknown/cross-connection 两项边界均close1008，边界重测新增usage为0。取消、重启和网络的最终范围参见本文开头与GPT验收记录。脚本及完整用法见 [验收清单](vm-kernel-full-acceptance-checklist.md)。

## 暂态观察的有限恢复

对已验证且binding/credential/proxy revision均未变化的实例，暂态只读观察错误采用最多三次有限观察：前两次发布不可调度状态并等待后续观察，第三次仍失败才进入原新generation恢复。成功观察清零计数；quarantine、deleting、版本变化不享受此缓冲。它不是推理重试，也不在观察失败期间沿用旧ready放行。
