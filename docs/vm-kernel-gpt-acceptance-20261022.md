# GPT VM 内核模式：真实验收记录

> 历史阶段记录：本文下列运行结果和阶段限制仅适用于记录时源码，不是本次业务复用修复的验收结论。固定并发 1、禁止 2FA 初始化、仅连接内历史等旧限制已调整；当前边界见 [业务复用修订说明](vm-kernel-business-reuse-20261022.md) 与 [实施状态](vm-kernel-implementation-status.md)。硬件 KVM 仍未验收。

日期：2026-10-22。基线为纯上游 Sub2API v2.15.0，分支 `codex/vm-kernel-account-mode`。参考运行包固定为 vm2api v1.3.206。本轮按用户最新要求优先完成 GPT；Claude 与硬件 KVM 不计入本轮已通过范围。

## 实现边界

保留 Sub2API 账号、分组、API key、OAuth 刷新、原选择器、准入、额度和计费。只增加账号 `execution_mode=vm_kernel` 及私有执行链路；未指定模式仍为 `normal`。VM 账号使用独立 QEMU guest、固定 Codex payload、代理和单执行槽，不引入第二套账号或扣费系统，不在失败后自动切回普通模式。

原公开入口 → 原账号选择/准入 → VMKernelService 的数据库与代际检查 → 私有 Unix socket → runner/guest relay → Codex kernel → 原 usage_logs 与余额计费。

WS 历史属于当前连接；同连接可续接及提交工具结果，未知/跨连接历史被拒绝。取消或执行结果不确定时隔离旧 binding；显式取消要求关闭旧连接、等待 runtime.ready，再建立新连接。

## 真实运行环境

- 独立测试应用 `127.0.0.1:18081`，独立 PostgreSQL/Redis；应用 UID/GID 1000、补充组 20000，应用未挂载 Docker socket。
- Docker server 为 Linux ARM64；原生 QEMU 运行 x86_64 guest，实际加速器为显式启用的 **TCG**，`kvm_enabled=false`。这是实际 guest、实际固定 payload 和真实 provider 请求，不是 kernel fixture；不据此宣称硬件 KVM 已验收。
- runner 镜像：`sha256:356005f39cfe7a9cafc2112cf33dc0a7f3e26144551e5dfe72cf8ab333db8fdd`。
- Codex payload SHA-256：`0ffec538a57afbe66f13832513ee4c2a27ecc970faa9367c7cb8a13551196dd3`。
- 测试模型为用户指定的 `gpt-6-luna`。凭据通过已授权的 2FA 登录获得；本文和可提交文件不保存凭据。
- 脱敏证据位于 `.dev/vm-kernel/verification/full-acceptance-20261022/`，私密配置不纳入 Git。

## 已完成的真实请求

| 验收项 | 结果 | 证据 |
| --- | --- | --- |
| Responses JSON / SSE | 各 1 次成功、正确终止 | `all-result.json` |
| Chat Completions JSON / SSE → Codex Responses | 各 1 次成功、正确终止 | `all-result.json` |
| Messages JSON / SSE → Codex Responses | 各 1 次成功、正确终止 | `all-result.json` |
| WS 同连接两轮 previous_response_id 续接 | 第二轮正确保留 cobalt 上下文 | `all-result.json` |
| 三协议 × JSON/SSE 函数工具闭环 | 12 次完成；call ID、名称、Paris 参数及结果 marker 正确 | `tools-result.json` |
| WS 函数工具续接 | 2 次完成，回传结果 marker 被消费 | `ws_tools-result.json` |
| 未知及跨连接历史 | 均 close 1008；新增账单 0 | `ws_tools-result.json` |
| 默认普通模式 | 原管理 API 创建默认 normal，模式不可变更新返回 400 | `normal-mode.json` |

基础八笔原账单：输入 1368、输出 53 token，费用 `0.00050122`，余额从 `10` 变为 `9.99949878`。十二笔 HTTP 工具账单费用 `0.00431162`，两笔 WS 工具账单费用 `0.00051702`。共 22 笔唯一成功请求，Sub2API 记录费用合计 `0.00532986`。这些是 Sub2API 原价格表记账结果，不是 provider 官方账单。

最终账单为 **24条、24个唯一请求**，输入47118、输出301 token，费用 `0.00545494`，最终余额 `9.99454506`。组成是上述22条加上两轮显式取消后各一次新连接 `RECOVERED` 请求；两份 explicit 报告各新增1条、费用 `0.00006254`。断连场景新增账单0，**没有再发送成功的新业务请求**，不能把它写成第二类业务恢复验证。

Messages 首次测试触发原分组 `allow_messages_dispatch=false` 的 403；通过原管理 API 仅为隔离测试组开启该权限后通过。拒绝请求没有费用；实现没有绕过原分组策略。

## 真实测试推动的修复

1. scheduler metadata 原先不携带凭据/Proxy，直接计算 revision 导致 ready 账号被错误排除；增加只供内部候选使用的权威摘要，执行前仍重读完整账号。
2. 后台健康观察与公开请求争用账号锁，导致伪 `vm_runtime_busy`；增加可取消、有界的控制锁等待，实际推理仍保持单槽，不排队重放。
3. 公共 WS 原有兼容逻辑会剥离首包 previous_response_id；VM 首包改为在派发前严格拒绝，普通账号保持原逻辑，服务层 continuation 拒绝也返回策略关闭 1008。
4. non-root 应用需要完整父路径 traverse 权限及独立 coordinator lock；仅开放专用组的 socket 连接权限，凭据和私有文件保持不可读。
5. 原上游 guest 启动与 9p 权限、投影原子替换、Codex/Claude engine 分支已按真实启动问题修复；镜像和 payload 固定，真实 guest 验证 access-token-only 和只读凭据投影。

## 生命周期与最终收尾

显式取消两轮均验证 `response_cancelled`、close 1008、等待 runtime.ready 及新连接 `RECOVERED`；证据分别为 `cancel-explicit-initial-result.json` 和 `cancel-explicit-result.json`。最终一轮 generation 12→13，guest boot 与内核进程证据更新，镜像和容器身份不变；TCG 环境等待恢复为 131.045 秒。两轮各新增一条恢复请求账单，余额一致。取消后需要重新连接，不承诺立即恢复或复用未完成的历史。

断连的独立证据 `cancel-disconnect-result.json` 显示 delta已出现、客户端断开，新增usage和费用均为0，`new_connection_recovered=false`。另有前后生命周期快照确认generation 13→14、guest boot/PID更换及ready恢复；这证明运行时恢复，**不证明断连后新业务请求成功**。取消脚本本身的 `guest_drain_verified=false` 不能被连接关闭替代；进程替换与版本变化来自独立生命周期证据。

稳定性观察连续 90 秒、十次 ready 均为 true。host 与 guest 均验证代理可通、直连 TCP 与 DNS TCP53 被拒绝，IPv6 和 direct resolver 关闭，access-token-only 凭据投影只读。只读证据是写打开返回 EROFS（errno 30），由 host readonly 9p 强制执行；guest `/proc/mounts` 显示 rw，不把它写成 guest mount option 为 ro。此处只报告实际探测的网络边界和短时观察，不推定所有协议或故障组合均已测试，也不等同长期稳定性验收。

`lifecycle-result.json` 确认：原 API 停用后请求 503 且容器 exited；删除后账号 404、容器不存在；tombstone、overlay 磁盘及身份保留；最终 24 条唯一账单不增加。

最终清理已完成，证据为 `cleanup-result.json`：

- 精确核验 `sub2.acceptance=20261022` 标签后，停止并移除 app、helper、PostgreSQL、Redis 四个独立验收容器及空测试网络；该标签剩余容器为 0。helper 进程也已确认停止。本轮验收未使用 Testcontainers，未清理其他测试会话或业务容器。
- 隔离库中 4 个已软删除账号的 credentials 清空，测试 API key 替换并停用；24 条 usage、费用与余额保持一致。PostgreSQL 卷 `sub2-vm-accept-pg` 保留。
- 三个已退役实例保留磁盘、原 manifest 和稳定身份。撤除仅用于本轮验收的凭据投影、internal token、含秘密的运行配置和生成的 guest SSH 私钥；应用配置及日志中的已知测试秘密已脱敏。该操作是验收凭据撤除，不是账号删除自动清除持久数据的产品行为，也不宣称对保留磁盘或数据库页做取证级擦除。
- 本地 private 配置目录已移除；本轮独立 GOCACHE/GOTMPDIR、Python 缓存，以及本地和 Linux 主机的 `servicegroup.test` 均已清理。保留已验收 server/helper、当前 runner 镜像及一个必要回滚镜像，不执行全局 Docker prune。收尾磁盘剩余 107.47 GiB，GOMODCACHE 保留。
- 原 `vm2api-local`、`sub2api-v2101-app-1`、`kin-02`、`kin-03` 仍运行且启动时间未变化；18080 与 8787 的健康检查成功。原工作区三处 ticket-borrowing 修改的完整 diff SHA-256 保持 `36d69013ecf3c3c433c1cf6c5c8afe3ff3356cc690a1ba8cf4c370877ad7f583`。

175 个变更文件的已知测试秘密扫描及 JWT/私钥/provider key 模式扫描均无命中，`.dev` 未纳入 Git。最终验收的 Linux ARM64 server SHA-256 为 `a82337632c974e1e805f1795b4b2c5ecc05a330cc0d976676ad75103a15753e6`。实现仍在功能分支工作区，未提交或推送；隔离验收环境已收回，不代表原业务实例已部署本功能。

启动与空闲观察曾出现未分类失败；追加三次只读observe耗时3.627、1.184、1.275秒，均成功，未重现8秒SSH超时，因此不把历史失败确定归因为超时。当前对已验证且版本未变化的binding采用三次有限观察：前两次失败不可调度，第三次仍失败进入原新generation恢复；成功清零，quarantine/删除/版本变化不缓冲，不重试推理。

## 验证范围说明

当前轮 Python runner/harness 共 49 项通过；前端账号/状态相关 90 项、类型检查和 Vite 构建通过。Go 的 runtime/client/service/config/admin/repository 定向测试及必要 race 已分批通过，Linux 非 root UDS 权限验证及最终 embed 构建通过。本轮使用独立 GOCACHE/GOTMPDIR 并已清除。完整历史 suite 与本轮定向验证的范围见 [实现状态](vm-kernel-implementation-status.md)，不重复累计为一次全量测试。

本轮不宣称 Claude、硬件 KVM、多机调度、图片/音视频或任意跨连接 WS 历史已通过。取消前 provider 未发送 usage 时允许不形成费用；带权威 partial usage 的确定性 fixture 测试另行证明保留一次计费结果，不由零费用的真实取消反推所有 partial 场景。

部署步骤见 [部署 README](../deploy/vm-kernel/README.md)；可重复执行的真实测试入口见 [验收清单](vm-kernel-full-acceptance-checklist.md)。
