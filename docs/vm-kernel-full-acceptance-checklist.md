# VM Kernel 全链路验收清单

> 本文前序隔离脚本及账单数字保留作历史验收方法。本次账号并发/2FA/业务复用与跨连接历史修复以 [业务复用修订说明](vm-kernel-business-reuse-20261022.md) 为准；旧脚本中“跨连接一律拒绝”的断言不能直接用作当前验收。最新 30 路真实并发验收进行中，尚未在此记录通过。

验收入口：`deploy/vm-kernel/tests/full_acceptance.py`；独立部署模板位于 `.dev/vm-kernel/verification/full-acceptance-20261022/compose.yaml`。这些文件是可执行验收准备，不是已经通过的运行证据。

## 隔离部署

- 使用父任务构建的当前 Sub2API 镜像、独立 compose project、PostgreSQL 与 Redis。数据库名必须以 `sub2api_vm_accept_` 开头，不连接业务库。应用只绑定本机 18081，模板 PostgreSQL 只绑定 15433。
- 父任务审查映射端口和实际镜像后启动。仅父任务管理 Docker、helper、runner 和凭据。不能把现有业务 `.env` 整体复制到这里。
- `VM_KERNEL_ENABLED=true`、`VM_KERNEL_HELPER_SOCKET` 使用绝对路径；app/helper/runner 必须以完全一致的绝对路径访问 helper 返回的 Unix socket。模板的 helper.sock 文件名须与实际 helper 启动参数匹配。
- helper 需要既有已验证原生 runner 镜像与 guest 模板。不要为这次验收重置业务 VM、覆写已有 overlay 或复用业务账号数据库记录。
- 当前仅有 OpenAI 真实账号授权；Claude 真实链路标记缺少凭据，不得借用业务 Claude 账号补齐。

## 种子与执行

创建 mode 0600 的父任务私密 JSON 文件，字段为 `isolated_database`、`admin_email`、`admin_password`、`proxy`、`credentials` 和可选 `model`（默认 gpt-6-luna）。`proxy` 对象使用 admin API 字段 protocol/host/port/username/password；credentials 是已授权账号的投影，不能重新触发第二个自动刷新控制方。文件不能提交或在日志打印。

设置 `PGHOST=127.0.0.1 PGPORT=15433 PGUSER=sub2api_accept PGDATABASE=sub2api_vm_accept_20261022` 以及私密 `PGPASSWORD`；本机须有 psql。然后运行：

```sh
python3 deploy/vm-kernel/tests/full_acceptance.py \
  --config /absolute/private/acceptance-config.json \
  --output .dev/vm-kernel/verification/full-acceptance-20261022/result.json
```

脚本按真实 API 合同依次创建 proxy、独占 openai group（`allow_messages_dispatch=true`）、vm_kernel/oauth account、余额 10 的隔离用户；用户登录后调用 `/api/v1/keys` 创建 key。管理员 `/api-keys` 不提供创建接口。脚本不输出 JWT、API key、账号凭据或响应正文。

## 通过条件

1. Responses、ChatCompletions、Messages 各 JSON 与 SSE，共 6 次请求，HTTP 成功且协议终止正确；SSE 不能包含 error。
2. 公共 `/v1/responses` WebSocket 成功升级；同一连接第二轮使用 previous_response_id，结果包含第一轮要求记住的 cobalt。共计 2 次完成。
3. 异步记账最多等候 60 秒；隔离用户/key/account 下恰好 8 条不同 request_id 的 usage_logs；input/output token 及实际费用为正；用户余额下降等于 sum(actual_cost)，容差 1e-8。此处验证原有余额计费链路，而非 helper 自报 usage。
4. 同步检查 helper/runner 代际和 guest 进程证据，证明公共请求实际命中了指定 VM。该部分需父任务取脱敏运行证据，脚本本身不证明隔离防火墙。

## 仍需分别验收的范围

- 普通 execution_mode 账号回归可以使用确定性 fake upstream，但必须与真实推理证据分开标注；本脚本仅创建 VM 账号，不把 fake upstream 作为真实 inference。
- 取消、上游错误、代理断路、繁忙、rotate、重启、删除、unknown quarantine、账单幂等故障路径应结合现有 UDS 集成测试和单独操作证据。8 次 happy-path 不能替代故障路径验收。
- Claude 无真实凭据时保留 fixture 协议覆盖与明确未验收状态。

## 收尾

父任务保存脱敏 result.json 与必要运行证据，删除本轮私密配置，停止明确属于本轮的 app/helper/runner 和测试容器。数据库/数据卷是验收记录，不能当编译缓存无条件删除；保留或删除需按父任务既定保留策略。不要全局 Docker prune。Go 构建使用本轮缓存并核对清理，记录磁盘剩余空间。不得声称仅因测试进程退出便不存在遗留 Testcontainers。

## 分阶段与中断恢复

`--phase seed|http|ws|billing|all` 可拆分执行。私密配置旁自动创建 mode 0600 `.state.json` 保存种子 IDs、key 和已完成请求；复用同一个配置避免重新创建账号。每个计费请求发出前写 pending，遇到不确定失败会阻止自动重放，必须先按 request ID、用户和时间对账后人工协调状态。HTTP 保存 x-request-id，WS 保存 response_id。种子创建途中失败尚不能自动恢复，应由父任务检查已创建资源再调整。取消使用独立脚本 `deploy/vm-kernel/tests/full_acceptance_cancel.py --config ... --output ...`，须在主验收 billing 完成后运行。显式取消要求 response_cancelled、reconnect_required=true 及连接关闭；等待 runtime.ready 后新建连接恢复推理。断连模式分别记录独立账单基线。socket 关闭不等于 guest 已排空，helper/guest drain 仍需单独证据；脚本拒绝盲目重复取消尝试。

使用既有 PostgreSQL server 时，由父任务新建专属 `sub2api_vm_accept_20261022` 数据库和最小权限专属用户，应用设置 DATABASE_HOST/PORT/USER/PASSWORD/DBNAME/SSLMODE；不得将 DBNAME 指向业务库。使用既有 Redis 时，先确认未使用的独立逻辑 DB，再配置 REDIS_HOST/PORT/PASSWORD/DB；不假设 DB15 空闲，不执行 FLUSHALL。独立 Redis 容器隔离性更强。绑定应用 SERVER_HOST=0.0.0.0、SERVER_PORT=8080，宿主仅映射18081。

## GPT 加强验收：工具与取消

最新范围仅 GPT。基础八次请求通过不能等价于所有功能通过。

- `deploy/vm-kernel/tests/full_acceptance_tools.py --config ... --output ...`：三种公共协议各 JSON/SSE 两轮函数工具闭环，共十二次调用。强制 lookup_weather(Paris)，核对 call ID/name/JSON arguments，提交工具结果 marker，核对最终答复消费结果，再对账十二条新增唯一 usage 与余额差。脚本 tool_attempt 阻止不确定失败后盲重放。
- `deploy/vm-kernel/tests/full_acceptance_cancel.py --mode explicit --config ... --output ...`：收到 delta 后发送 response.cancel，要求 response_cancelled、reconnect_required=true、close 1008；等 runtime.ready 后通过新连接创建响应并完成。保留 unknown quarantine，不承诺同连接恢复；取消与恢复分别对账，余额必须一致。
- 同脚本 `--mode disconnect` 独立验证客户端断连；两种模式分别保存尝试状态，不混用。关闭客户端和账单稳定并不证明 guest 已排空，仍须同步取 helper/guest 活跃请求与 slot 释放证据。

仍不能用真实随机取消证明 partial usage 的所有路径：provider 可能在中断前完全没发 usage。应另用可控 helper fixture 在 usage 已出现之后断流，经公共 handler 和数据库证明一次记账且不重试。现有 UDS partial 测试覆盖服务结果，不足以替代公开入口和真实数据库整合断言。

其他尚需专项覆盖：重叠 WS response.create 拒绝；取消后上一轮未完成 response_id 不可续接；鉴权/权限/模型映射在续接间变化；超长工具参数/坏 JSON/工具错误返回；并行多个工具调用。新增工具脚本只证明一个标准 function 工具的完整闭环。

## 调度缓存与健康观察争用

精简 scheduler metadata 会移除 access token 和 Proxy，不能使用精简 Credentials 的 hash 代替权威账号的 revision。缓存投影携带从完整账号生成的专用 credential/proxy revision 摘要，旧投影缺摘要时强制 miss/reload；公开 DTO 不暴露这些内部字段。完整账号仍现场计算 revision，Execute 保留执行前数据库与 binding fence。对应回归覆盖秘密剔除、旧缓存 miss、credential/proxy 变化拒绝旧 metadata，以及 Execute 的最终数据库 fence。

健康 reconcile 与推理 admission 共享每账号锁，单次只读 inspect 持锁时 readiness 仍有效。Execute 对该锁争用使用最多十秒且可取消的等待，取得锁后继续执行原权限/版本检查；active 单槽仍立即拒绝并发请求，冷启动不可调度时不等待十分钟。等待不发送推理，不构成重试。对应回归为 `TestVMKernelAdmissionWaitsForHealthyControlObservation` 和 `TestVMKernelAdmissionControlWaitHonorsCancellation`。源码修复与测试不能替代本轮真实运行证据。

partial usage/重叠请求已有 fixture 层测试或约束；未完成专项真实测试的项目仍明确未验收，不从基础二十次、取消或重启推导全覆盖。

## WS 工具与历史所有权

运行 `python3 deploy/vm-kernel/tests/full_acceptance_ws_tools.py --config /absolute/private/acceptance-config.json --output /absolute/evidence/ws_tools-result.json`，复用既有私密种子状态，串行执行 WS 两轮函数工具调用及 marker 回填，并验证未知 ID 和跨连接 ID 拒绝。脚本 pending 必须先对账再处理，不能为重跑边界而盲目重复已完成的两次收费工具请求。

真实边界验收曾发现公共 handler 的普通账号兼容逻辑会剥离首帧 previous_response_id，使 VM 未知 ID 退化成新推理。前序首版 VM 首帧任何非空 previous_response_id 都在 guest dispatch 前拒绝并 close1008；normal 保留原有兼容逻辑。连接内 history.prepare 的非法 continuation 同样返回客户端策略1008，不泄漏 ID。

本轮已读取的安全报告 `.dev/vm-kernel/verification/full-acceptance-20261022/ws_tools-result.json` 确认：工具两轮 marker 消费成功，新增2条usage、费用0.00051702、余额一致；unknown_history 与 cross_connection_history 均 rejected=true、close_code=1008，边界重测新增usage为0。报告明确仅重测被拒绝边界，保留原两次已完成工具结果。零usage与源码dispatch前guard结合说明拒绝边界；不把零usage单独当作所有路径均未触达provider的证明。

## 原分组策略与 seed

OpenAI 分组默认 `allow_messages_dispatch=false` 时，Messages 返回403是原策略生效。测试 seed 必须通过原管理员 API 显式设置 `allow_messages_dispatch=true`，再进行跨协议验收；不能在 VM 分支跳过该权限。基础八次及工具矩阵的结果应在此配置前提下解读。

## 生命周期只读取证

`lifecycle_evidence.py` 不启动、停止或删除容器，不读取凭据；父任务执行已授权生命周期操作前后分别采集：

```sh
python3 deploy/vm-kernel/tests/lifecycle_evidence.py snapshot \
  --docker-context CONTEXT --instance-id INSTANCE_UUID --output /absolute/evidence/before.json
python3 deploy/vm-kernel/tests/lifecycle_evidence.py snapshot \
  --docker-context CONTEXT --instance-id INSTANCE_UUID --output /absolute/evidence/after.json
python3 deploy/vm-kernel/tests/lifecycle_evidence.py validate \
  --before /absolute/evidence/before.json --after /absolute/evidence/after.json --expect recovered
```

`--expect` 可为 recovered、stopped、absent。recovered 核验同实例/上下文、固定镜像、generation递增、boot ID变化与kernel ready；stopped/absent 分别确认容器已停或经成功inventory确认不存在。输出显式不证明 business inference、helper tombstone 或磁盘保留，这些须单独证据。取消、重启、删除和网络的已完成结果见 [GPT验收记录](vm-kernel-gpt-acceptance-20261022.md)，本节只说明工具用法。

## 本轮已完成范围对照

GPT已完成基础8、HTTP工具12、WS工具2，以及两轮显式取消后各1次新连接恢复，共24条唯一账单，输入47118、输出301，费用0.00545494、余额9.99454506。显式取消最终一轮generation12→13且RECOVERED成功；独立断连仅证明generation13→14、boot/PID更新与ready恢复，未追加成功业务请求，账单新增0。未知/跨连接历史均close1008、无新增账单。停用/删除及指定网络探测已通过，详见 [GPT验收记录](vm-kernel-gpt-acceptance-20261022.md)。

本轮为实际x86_64 guest上的显式TCG、真实provider调用；硬件KVM和Claude尚未验收。普通模式仅验证默认值及模式不可变API，不把该结果等同普通模式全部业务回归。复杂partial、重叠、权限中途变化、多工具及其他未逐项实测故障仍保留fixture/未实测边界。
