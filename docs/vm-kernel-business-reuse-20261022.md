# VM 内核模式：业务复用与并发修订

日期：2026-10-22。适用分支：`codex/vm-kernel-account-mode`，基线 Sub2API v2.15.0。本文件描述当前源码修订，区分前序首版证据与本次验证，不把实现状态等同于部署或真实通过。

## 实现边界

VM 只替换已选账号的推理执行环节。账号、分组、API Key、原 OAuth/2FA 登录及刷新、模型映射、用户/账号限额、原调度准入、质量判断、用量解析和原计费链路继续由 Sub2API 管理。`normal` 仍是默认；已创建账号不直接切换执行模式。VM 必须绑定有效代理。本地 guest/kernel 健康及当前 receipt 验证通过后发布 ready，不再自动调用模型作为 VM 就绪门槛。原账号业务准入保持独立。

```text
原授权/2FA → 原账号及凭据存储 → 受限 access-token-only guest 投影
原公共入口 → 原账号选择/准入/并发 → VM 最终账号与代际检查
          → 私有 client → runner relay → guest 推理内核
          → 原协议呈现/错误观察/RecordUsage
原管理测试/质量/初始准入 → 原问题和判定逻辑 → 同一 guest 执行
```

不引入第二套账号、调度器、价格、账单或 OAuth 刷新器。guest 不接收密码、TOTP secret 或 refresh token；原登录系统取得并持久化凭据后，控制面再投影必需的访问凭据。

## 本次调整

| 领域 | 当前行为 | 主要接点 |
| --- | --- | --- |
| 并发 | 取消固定 1，沿用账号配置。原业务 lease 不重复获取；runtime 的 business/probe 活跃总量按最新账号并发等待，任一请求结束后唤醒并重新校验；配置不大于 0 时沿用不限制语义 | `vm_kernel_service.go`、账号创建/编辑与公开状态 |
| 2FA | 使用原 OpenAI 2FA 登录和初始准入流程；VM 不是独立登录类型 | 原 admin account/API、CreateAccountModal |
| 管理与质量 | 管理文本、初始准入、quality/Pelican/candy 的问题/模型/判定/观察逻辑继续复用；对应推理进入 guest，不能因 VM 模式直接跳过原流程 | `account_test_service_vm_kernel.go` |
| 业务准备 | 模型映射、原 Codex OAuth 请求变换、必要策略校验在 guest 派发前执行；HTTP/WS 保留原 hooks 和原计费提交位置 | `openai_responses_business_body.go`、`vm_kernel_business.go`、各原 Forward 接点 |
| 错误与恢复 | 未提交且明确可安全重试的 runtime 失败可交原 failover；已派发且结果未知不能重放。已知模型/provider 失败沿用原错误分类及限流观察，不显示为账号 VM 故障，不触发 guest 重建 | `vmkernel/client.go`、`vm_kernel_business.go` |
| HTTP/WS 续接 | 完成历史有界共享；校验原用户/分组所有权、response→账号与实例 generation 后重建完整上下文。原规则允许同用户不同 API Key；未知/过期/跨归属或跨代际历史明确拒绝 | `vm_kernel_history.go`、`vm_kernel_ws.go` |
| 凭据变更 | 实际 guest 投影变化才更新 credential revision；模型映射、header 元数据或 host refresh token 变化不再单独触发 guest 重启 | `vmCredentialProjection`、`vmCredentialRevision` |
| 排空 | 配置轮换、未知执行结果恢复与删除不能打断其他在途请求。取消等待尚未派发时不隔离实例；已派发未知取消暂停新请求，等待其他请求结束再恢复 | `vm_kernel_service.go` |
| 删除 | 原数据库删除成功后立即撤销 runtime 新请求权限；后台完成排空和资源清理。晚到的健康观察不能恢复已删除账号 | `MarkVMKernelAccountDeleting`、原 admin 删除逻辑 |

业务请求使用控制面三十秒有效的本地就绪快照，不逐请求执行 helper inspect；派发前仍校验最终数据库账号状态、credential/proxy revision、路由与 session binding。业务成功不会延长该快照的健康有效期。

固定同步内核的非 200 HTTP 响应在限定 64 KiB 内完整读取结束时，`Settled=true` 仅表示本地 handler 已返回，避免将已结束的错误请求误作内核仍在执行。它不证明上游未处理请求，`SafeToRetry` 仍为 false；读取失败、截断、超限或其他未确认的执行结果仍保留 unknown 边界。

第二轮真实 502 定位到 cloud-init 覆盖 `/etc/hosts`，使禁用 DNS 的 guest 丢失代理域名映射；同一代理使用 IP 直连时 CONNECT 200、TLS 成功。修复将 guest setup 排在 cloud-init 之后，seed 禁止覆盖 hosts，并在 apply 写入、observe 校验实际代理解析投影，receipt 不再只校验配置文件 revision。该校验不调用模型，不开放 guest 直连或 DNS。

runtime 私有 `capacity=0` 表示不另设固定 guest 单槽上限；公开账号状态显示账号配置的并发上限。它不表示内核、上游或宿主的计算资源无限。原调度仍控制业务准入，runtime 活跃计数只避免管理探测绕过账号配置且不重复计算 Redis lease。

## 续接及取消的精确范围

共享快照只保存已经完成并经过终止/用量核验的请求。缓存最多 512 项、64 MiB，单快照不超过 16 MiB，TTL 使用原 sticky 配置并限制在一小时内；连接内历史继续有独立上限。它位于当前进程内，不是持久会话库。guest generation、凭据/代理应用后的绑定变化或服务重启会使旧快照失效，客户端需提供完整上下文。

仅继承必要模型默认值；本轮显式模型、工具和 instructions 仍走原策略校验，旧 instructions 不强制沿用。工具调用输出与 reasoning 项作为完整上下文保留，不把历史压缩成纯文本。

同一个 WS 连接仍一次执行一个 response.create；账号多并发由多个请求/连接实现。取消已派发请求仍可能返回 `reconnect_required=true` 并关闭旧连接，因为固定内核尚无可靠的请求级排空确认。并发修复保证其他已派发请求可以结束，不承诺取消后原连接立即复用。

## 保留的执行能力边界

- VM 标准 OAuth 账号不组合 BPS、Prism、SIWC、Copilot、插件出站、影子/聚合账号或 regional 执行通道。这些涉及另一条实际执行路径；普通模式维持原行为。
- 依赖 host ticket/harvest/turn-state 特性的 Codex 状态探测仍明确不支持。恢复质量文本和原管理测试不等于恢复这些特定 transport 探测。
- 图片/音视频生成、Responses compact/Lite、count_tokens、内建搜索等专用执行能力不在当前 guest 文本与函数工具合同内；不静默切回宿主出站。
- 本次优先 GPT。Claude 的协议 fixture 与代码兼容性不代表真实 Claude 验收；Claude payload 内部 `slots_per_worker=1` 的行为仍需独立实测，不能将 GPT 并发结论扩展到 Claude 内核。
- 默认硬件 KVM；本机显式 TCG 验证不证明 `/dev/kvm` 可用或硬件加速性能。硬件 KVM、真实 Claude、多机或跨服务重启续接仍未验收。

## 验证状态与下一步验收

前序首版真实 TCG 验收记录见 [GPT 验收记录](vm-kernel-gpt-acceptance-20261022.md)：其中 24 笔唯一账单、取消/网络/生命周期证据只适用于当时产物。当时固定单并发、跨连接历史拒绝等是历史事实，不再作为本次源码要求。

本轮真实验收目前未达到 30 请求全部成功。各轮失败证据完整保留，不用后续结果覆盖：

| 轮次 | 成功 / 失败 | 当前结论 |
| --- | --- | --- |
| 第一轮 | 4 / 26 | 未通过 |
| 第二轮 | 0 / 30（HTTP 502） | client 在途、账号 lease、guest relay 连接峰值均为 30；代理 hosts 映射被覆盖，且错误曾导致 generation 12→13 误重建 |
| 第三轮 | 0 / 30 | 临时部署经 stdin 写文件失败，使 guest 网络脚本为空；不是修复后源码的有效推理验收。错误前后 generation 14、guest boot 不变，ready 保持，租约清零且无扣费 |
| 第四轮 | 24 / 6（HTTP 502） | 修正部署传输并校验产物后，三个本地峰值仍均为 30；24 条唯一账单、usage 与余额对账一致。6 条失败仅确认 guest `hop_err`（`sent=false`、`retry=true`、`error sending request`），缺少底层 source；不能归为具体超时，不能标记全成功通过 |

第四轮 `run4/load30-result.json` 记录费用 0.01061946，权威 provider input 106128、output 18261；结束时四类 lease 均为 0。`minimal-fix/before-run4.json` 与 `after-run4.json` 显示 generation 14、guest boot 不变且 ready 保持，模型失败未再误重建 VM。上述峰值证明本地请求、账号租约及 guest relay 并发达到 30，不单独证明 provider 内部同时执行 30 条；本轮环境为显式 TCG，不代表硬件 KVM 性能。

第三轮部署错误已改为参数传输并进行 SHA 校验，源码、镜像、seed 与实际 guest 文件一致；实际 apply/getaddrinfo 通过，单次公共业务 smoke 返回 HTTP 200、completed 及 usage。证据为 `minimal-fix/guest-network-verified.json`、`network-business-smoke.json`。首轮独立 post-load 8 项为 5 通过、3 失败：HTTP 续接被 HTTP 入口的普通 OAuth 限制拒绝；续接历史发布把正常 Redis cache miss 当成错误；Chat SSE 输出完成但缺 usage chunk。6 条唯一账单及余额一致，其中 Chat SSE 记账正常，失败在客户端呈现。原始 post-load 失败报告保留。原 2FA 登录成功不替代上述推理与计费结果。


本次只补齐上述 VM 接点：HTTP 账号筛选保留可在本地验证并重建历史的 VM 账号，普通 OAuth 的原限制不变；正常 Redis miss 允许首次历史发布，真实缓存错误与 owner/account/generation 校验仍保留；VM Chat 流沿原网关规则设置 `IncludeUsage=true`，输出权威终态 usage chunk，不依赖客户是否显式设置 `stream_options.include_usage`。原脚本已设置该选项，不能把缺 usage 归为脚本漏参。接点补丁已部署，后续独立结果如下，首轮失败报告不覆盖。

修复后的 `post-run2/post-load-result.json` 完整 8 项仍为 5 通过、3 失败：HTTP continuation、Chat SSE、Messages JSON 的三条失败经应用与 guest 日志关联确认是上游连接 `hop_err`，不再是本地校验或协议转换拒绝；WS 跨连接续接已真实成功。该轮 5 条唯一账单、费用 0.0003159，余额一致。

随后固定四项的 `post-targeted/result.json` 为 4/4 通过：HTTP seed、HTTP continuation、含 usage 的 Chat SSE、Messages JSON。该轮 4 条唯一账单、费用 0.00025306，余额一致。两轮分别保留，不能拼写成同一轮 8/8。

| 功能类别 | 接点修复后的真实成功证据 |
| --- | --- |
| HTTP Responses 首轮 | post-run2 与 post-targeted |
| HTTP previous_response_id 续接 | post-targeted |
| WS 首轮 | post-run2 |
| WS 跨连接续接 | post-run2 |
| Chat JSON | post-run2 |
| Chat SSE（包含 usage） | post-targeted |
| Messages JSON | post-targeted |
| Messages SSE | post-run2 |

以上八类功能分轮均已有真实成功证据，但完整八项单轮仍出现连接波动，30 压测也仍为 24/30 成功。最新接点版本应用镜像 `sha256:6a54d2868d8b…`、二进制 SHA-256 `de2b8f867d8d…`，部署健康记录见 `minimal-fix/deployment-settled.json`；完整摘要保留在报告中。30 路第四轮运行于此前并发/状态修复版本，后续接点补丁仅使用上述两轮功能验收，未重跑 30 路，不能把第四轮压力结论当作最新产物再次压测通过。

当前 `minimal-fix/settled-final.log` 与 `settled-race.log` 确认 vmkernel、service 定向测试及竞态检查通过；`presentation-final.log` 与 `continuation-final.log` 另确认最终 presentation、续接相关 service/handler 定向验证通过。代理投影 Python 报告 `proxy-hosts-fix-20261022.json` 记录 53 项运行包测试通过，追加两项 receipt 回归后相关 bridge 9 项通过；两组数字不相加为独立全量结果。这些代码验证不替代各轮真实结果，亦不消除第四轮 6 条失败。

当前源码增加的定向回归包含：

- `TestVMKernelConfiguredConcurrencySharesProbeCapacityAndWakesOnAnyRelease`：业务与探测共用配置容量，释放一个请求即可继续。
- `TestVMKernelCapacityWaitCancellationDoesNotDispatchOrQuarantine`：取消等待不派发、不影响健康 peer。
- `TestVMKernelCapacityWaitRechecksLatestAccount`：不使用 stale selected 并发；醒来重读账号并拒绝已经禁用的账号。
- `TestVMKernelTwoActiveAttemptsDrainBeforeCredentialOrProxyRotation`：两条在途请求排空后才推进一个新 generation。
- `TestVMKernelCancellationDrainsConcurrentPeerBeforeRecovery`：已派发未知取消不能重启其他正在执行的 peer。
- shared history 的所有权、跨连接重放、代际、容量/TTL 与请求策略回归。

30 并发的报告至少分别列出同时发起数、实际 guest/provider 在途证据、各请求 HTTP/SSE 终止与错误、权威 usage、原 usage_logs 唯一账单和余额一致性；30 个最终成功不能单独证明 30 条同时执行。取消 peer 生存、配置轮换与 30 并发压测属于不同验收项，不互相替代。

最终 `minimal-fix/final-state.json` 确认账号 active、schedulable、并发 30，VM ready=true，实际加速 TCG，generation 14 与 guest boot 保持；应用 healthy、restart count 0。本轮测试请求已停止，剩余上游连接波动及未达到 30/30 全成功的事实保留。

收尾 `minimal-fix/cleanup.json` 确认本轮独立 GOCACHE/GOTMPDIR 已清理，宿主剩余 106.91 GiB；本轮没有启动 Testcontainers，最终没有运行中的测试容器。仅清除明确过期的编译产物，保留当前部署、正常业务容器、数据库、账单、请求证据及 VM 磁盘，未做共享 Docker 全局清理。
