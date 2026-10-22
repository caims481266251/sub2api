# 账号新增 VM 内核模式实施方案（KVM 首版 · 深审收敛版）

日期：2026-10-22。基础版本：纯上游 sub2-api v2.15.0，提交 8b36fe7d647c28e8189147c6bfce52a41b6e26db。参考：vm2api v1.3.206，提交 894a78daba0361ecf825e5ec4a0707108cdbae9b。

状态：仅方案与源码审查，尚未实现、下载或运行内核。本版取代此前文档中的扩展性建设任务；分支信息见 [分支基线说明](vm-kernel-branch-baseline.md)。

**本期目标：在 sub2api 已选定账号之后，增加“该账号专属运行环境 → 真实内核 → 上游”的执行方式，并把结果送回现有请求处理和计费流程。账号管理、授权刷新、调度算法、配额、定价、扣费和账单仍由 sub2api 负责。**

**首版按用户最新指定固定为 KVM 真虚拟机：独立 Linux guest 内核、SMBIOS / MAC / 磁盘序列号，宿主必须提供可实际使用的 /dev/kvm。Docker 只封装 QEMU runner；普通 Docker slot、TCG 模拟和宿主直接运行推理内核均不属于首版 VM 能力。**

## 1. 深审结论与修正

原方案方向可行，但不能按“增加表单 + Handler 调用另一 Forward + 返回相似结构”直接实施。三个 Agent 分别复核了 sub2api 接点、内核协议和运行依赖，发现以下必须修正的问题。P1 表示不修正就不能开放对应 VM 能力；P2 表示必须在实施前确定的范围或兼容性问题。

| 级别 | 原方案缺口 | 本版决定与证据 |
|---|---|---|
| P1 | Handler 提前分流会绕过 Forward 内的准入、模型/档位策略，OpenAI 还存在独立区域计量封装 | 分流收进现有 Service.Forward，以薄 VM wrapper 调用原有策略 helper。VM 首版不支持 regionalMetering；normal 原样。见 openai_gateway_forward.go:93、124，openai_gateway_usage.go:164 |
| P1 | 只保证结果结构兼容，未约束 result/error 组合 | 明确完整成功、partial usage、可安全重试和执行结果未知四种结果。已有用量不能丢；可重试错误必须 result=nil。见 gateway_handler.go:1103，openai_gateway_handler.go:1140、1191 |
| P1 | “未输出”可能被误判为“未执行”，叠加 vm2api router 与 sub2api 重试 | 新客户端只提交一个 attempt；已发往内核但结果不明禁止自动重放。外层仍只有 sub2api 编排重试。见 kernel-router.mjs:273 |
| P1 | 原有 credential lifecycle 设计可能产生第二个刷新者 | 保留 sub2api DB、TokenProvider、OAuthRefreshAPI；新增 CredentialProjection。内核无法满足单一凭据权威时，该平台暂停启用，不能另建刷新/回写系统 |
| P1 | SSE 被直接透传时会泄漏私有事件、提前结束、漏读 trailers | 分别追踪下游提交、标准终止、内核关闭；过滤内部事件，读完整响应后完成 attempt。见 go-worker-client.mjs:802、837、879 |
| P1 | 主分流点拒绝能力不足以阻止其他路径选中 VM | 候选、sticky、previous-response、缓存、legacy fallback 和发包前都检查 mode/capability/readiness；不支持的服务入口也加兜底拒绝 |
| P1 | 把容器 running、内核 ready、账号通过准入合成一个状态 | 三者分开；内核健康不能重新启用被管理员停用的账号。已有测试入口增加 VM probe，避免“先 ready 才能测试，先测试才能 ready”的循环 |
| P1 | Codex 的参考实现不等于每账号 guest 内执行 | v1.3.206 Codex supervisor 在宿主/控制面 spawn，现成 guest 脚本只启动 Claude 内核。Codex 必须新增 guest 内服务及 relay 并独立 PoC；不能以放入外层 runner 代替进入 guest |
| P2 | 节点/模板 UI、Manager+Relay、完整维护框架超出范围 | 删除首版节点和模板选择、远程节点协议、VMCredentialLifecycle、通用任务平台。仅保留本机受限 helper、KVM 必需的固定 guest relay 和账号附属运行记录 |

KVM 专项审查另发现：上游设备预检只检查可访问/可映射，不等于 KVM 能创建 guest；guest socket 不能通过 9p 直接供宿主连接；基础盘或容量变化触发的上游 overlay 删除逻辑不得照搬。第 6 节将三项纳入首版实现要求。

以上结论来自固定版本源码，不代表闭源内核已经通过运行验证。具体证据索引见第 11 节。

## 2. 固定实现边界

| 领域 | 继续使用 sub2api | 本期只增加 | 明确不做 |
|---|---|---|---|
| 账号管理 | 现有账号 ID、平台、认证类型、CRUD、分组、权限、导入导出、禁用/删除 | execution_mode；账号附属运行状态；VM 模式必要校验 | 第二套账号库、VM 独立账号池、独立管理台 |
| 凭据与维护 | 原授权/导入、TokenProvider、OAuthRefreshAPI、额度/模型查询、原代理配置 | 把已持久化凭据投影成内核格式；推理测试按 mode 分流 | 内核登录系统、第二刷新调度器、kernel→DB 凭据回写流程 |
| 选路与并发 | 原账号排序、粘性、组权限、限额、RPM、用户/API Key/账号并发、重试预算 | VM 能力和就绪检查；绑定版本复核；真实接受时回调 | vm2api scheduler、第二个请求队列、跨节点调度 |
| 推理执行 | Handler 的请求身份、组策略、响应和结果后处理 | 两个平台 VM wrapper、UDS kernel client、协议/结果适配 | 通用 HTTP Executor、普通链路的代理替代品、整套 vm2api gateway |
| 用量与计费 | 原 usage worker、RecordUsage、价格/倍率、余额、订阅、配额、去重和账单 | 填完整现有结果；确保 partial/unknown 语义正确 | VM 定价、VM 账单、独立 ledger、补偿收费系统 |
| 运行支撑 | 原账号启停/删除操作与审计入口 | 固定 KVM 模板准备一账号一 guest；QEMU/guest 检查、身份持久化、排空和精确清理 | 云主机创建、节点注册、自动扩缩容、模板市场、迁移/高可用平台 |

账号界面仅增加“执行模式：普通（默认）/ VM 内核（KVM）”。VM 模式继续选择原有平台、凭据、分组和代理，不增加节点、镜像、运行模板表单。只在原账号列表/详情显示模式、实际隔离类型、内核版本、准备/就绪/错误摘要。

添加 VM 账号后仍自动准备该账号的固定实例，以满足从账号管理开始的使用流程；自动准备仅是本链路的运行支撑，不扩展为资源管理产品。PoC 可以先人工准备实例，正式交付不能要求用户再创建一份 VM 账号。

## 3. 首版能力矩阵

以下是确定的交付上限。每个平台只有通过 K1 的固定 payload 验证后才开放，不因另一个平台通过而自动启用。

| 项目 | 首版 VM 模式 | 普通模式 |
|---|---|---|
| 平台与身份 | Anthropic 普通 OAuth；OpenAI 普通 OAuth/Codex，复用现有授权入口 | 保持 v2.15.0 全部原有类型 |
| 请求入口 | Anthropic：POST /v1/messages；OpenAI：POST /v1/responses | 原有入口全部保留 |
| 请求形态 | HTTP 非流式、HTTP SSE；文本、thinking/reasoning、标准工具调用与完整上下文工具结果 | 原行为 |
| 明确不支持的请求 | WS、Responses compact/Lite、Chat Completions、跨平台协议转换、count_tokens、图片/音频/视频生成、内建搜索及其他专门执行路径 | 原行为；可由原能力账号处理 |
| 续聊 | 客户端发送完整上下文，复用原 account sticky；首版不提供依赖内核保存状态的 continuation | 原语义 |
| continuation 限制 | OAuth HTTP previous_response_id 保持现有拒绝规则；Claude thread.previous_message_id 等内核状态续接不开放 | 不改变普通账号既有能力 |
| 特殊账号链路 | 不组合插件、SIWC、Prism、BPS、Copilot、harvest/ticket 出站、影子/聚合账号或自定义中转 | 原行为 |
| 运行环境 | 单机 Linux amd64 + 可用 /dev/kvm，单活本机协调器；固定 Ubuntu 24.04 amd64 guest，一账号一 QEMU/KVM VM；外层 Docker runner | 不依赖 Docker 或内核组件 |
| 区域部署 | VM 不支持 regionalMetering / 远程 regional 请求执行组合，启动启用检查、账号校验与发包检查均拒绝 | 原 regional 模式保持 |
| 隔离目标 | 推理在独立 guest 内执行；SMBIOS/MAC/磁盘序列号真实注入并持久化，界面标为 KVM | 原行为 |

这里有两层内核：KVM guest 自己的 Linux OS 内核，以及 guest 内 kin-kernel / kin-codex-kernel 推理二进制，两者都需要运行证据。固定规格与镜像写在部署配置，首版不提供 Docker/KVM 切换表单。远程节点、其他 guest 发行版、ARM/TCG、在线扩容、自动升级和跨实例会话恢复均延后。

端点相同不代表能力相同：例如 /v1/responses 内的 image_generation、compaction、状态型 continuation 也必须筛除。原有完整上下文工具校验继续生效；不为 VM 自动删除 continuation、补历史或改写成另一个协议。

VM 不支持的请求应在原 selector 中排除该类账号；有合格普通账号时按原规则选择，没有时返回明确的现有协议错误。直接调用某个账号的管理测试也必须检查能力。不得让 VM 同一账号静默改走普通 Forward。

## 4. 请求链路与准确接入位置

~~~mermaid
flowchart TD
    A[现有鉴权 / 组策略 / 配额与并发] --> B[现有 selector + VM 能力与就绪条件]
    B --> C[现有 Service.Forward]
    C --> D{execution_mode}
    D -->|normal| E[原有执行流程]
    D -->|vm_kernel| F[薄 VM wrapper：复用策略与准入]
    F --> G[复核账号 / 实例版本 / 凭据投影]
    G --> H[本机 UDS / QEMU 转发 / guest relay]
    H --> I[账号 KVM guest 内真实内核]
    I --> J[指定出口 / 上游]
    H --> K[平台响应与用量适配]
    E --> L[现有 Handler 结果与错误处理]
    K --> L
    L --> M[现有 usage worker / RecordUsage / 配额与计费]
~~~

### 4.1 Handler 仍拥有原流程

保留账号选择、并发申请/释放、RPM、利润/计费资格、重试预算、组模型策略、请求定价时刻、usage 提交和日志归属。不要在 Handler 增加一套平行请求循环。

OpenAI 保留 rpmAdmission.forwardContext 及包裹 Forward 的账号 release defer（openai_gateway_handler.go:1109）。Claude 保留 OnUpstreamAccepted 的串行队列回调、兜底释放及账号并发释放（gateway_handler.go:888、927、934）。VM adapter 不重复申请或释放这些许可。

Claude adapter 只有在固定内核协议确认本次请求已被接受执行时才触发 OnUpstreamAccepted，且最多一次；health 成功、relay/socket 建连成功都不能代替该信号。接受前失败由原 Handler 兜底释放。无法证明可靠接受信号时，K1 不通过该合同，不能自造“已接受”状态。

### 4.2 Claude 的薄 wrapper

GatewayService.Forward 保留请求非空校验、validateClaude55Request、anthropicSpeedServiceTier 的结果 defer、beginUpstreamResponseModelObservation。**在 shouldEmulateWebSearch 等普通执行分支之前进入 forwardVMKernelMessages**，基线位置约 gateway_forward.go:123–126。

wrapper 复用 evaluateBetaPolicy，block/filter 必须应用到实际送入内核的 beta 表示；CLI 使用 body.betas，不能只在 gin 中设置普通 HTTP builder 才会读取的缓存。OAuth 模型标准化沿用 claude.NormalizeModelID；保留 requested、mapped、response model 的区别，并更新当前 attempt 的 ParsedRequest body/model。

不整段运行普通 HTTP 的 mimicry、system/身份头注入或普通出站函数，再让 CLI 重做一遍。历史 thinking/tool 规范化仅按第 3 节已支持形态复用必要 helper；需要未验证的专用转换时拒绝该能力，不偷偷回退普通执行。

### 4.3 OpenAI 的薄 wrapper

OpenAIGatewayService.Forward 入口识别 VM 后进入 forwardVMKernelResponses，normal 继续原代码顺序。VM wrapper 明确复用：

- withOpenAIHTTPBudget、requesttiming、响应模型观测及前次 attempt 元信息清理。
- admitOpenAITurn 的权威账号、分组/模型、quality、限流和绑定复核。
- detectCodexClientRestriction 等原客户端限制。
- resolveOpenAIForwardMappedModels、reasoning 规范化、LegacyIngress 和标准工具 schema 校验。
- 组强制档位与 evaluateOpenAIFastPolicy；允许把现有深层 body/tier 处理提取成小 helper 供两条链路调用，不能复制一套不同的政策。
- 原 TokenProvider / OAuthRefreshAPI，仅用于取得并持久化权威凭据，再做投影；不调用普通 HTTP/plugin 出站。

VM 不进入 prepareCodexAccountIdentitySource、WS anchor/pool、SIWC/Prism/BPS 或普通 OAuth wire 组装。VM 实例 identity、出口与内核协议由固定适配负责，不能叠加普通传输身份。

admitOpenAITurn 本身不发 harvest 请求，但包含普通 Excel route 和 Codex ticket gate。VM 只跳过与其执行方式无关的这两类传输检查，并替换为 VM 能力/绑定检查；selector 和后台 harvest 的普通 ticket 资格也要识别 mode。**一般模型权限、配额、质量和限流检查继续有效**，不得借此绕过业务准入。

regionalMetering 首版是明确不支持组合，在调用/计量前拒绝。不能伪造 RegionalMetered=true，也不能直接调用 legacy RecordUsage 假装已计费；普通 Forward 的 beginRegionalMeter/finish 封装不变。

### 4.4 新增调度条件，不另建调度器

业务可选条件 = 原账号可调度条件 AND 支持当前端点/请求形态 AND 本机绑定有效 AND runtime 就绪 AND 凭据/出口配置版本已应用。

同一检查覆盖候选列表、sticky 命中、previous-response、legacy fallback、Redis/进程内账号快照和最终发包。旧快照缺 mode 不得把新 VM 账号当普通账号；启用前升级缓存格式并失效旧数据，不能只改数据库 mapper。

快照只用于筛选；发包前重新校验 mode、账号禁用/删除、group/model、instance generation 和配置版本。跨 generation 的池化 UDS 连接必须淘汰，不能连接到旧实例。VM 执行仅能发生在承载其本机 runtime 的网关，其他实例不能认领它。

内核容量是额外执行约束。固定模板应与允许的账号并发匹配，超过验证容量的账号配置需拒绝或经原并发入口施加上限，不新增第二个等待队列。busy 不等于 unhealthy；发包前确认无容量时在原等待预算内处理，不能每层再加完整超时。

## 5. 内核单次调用合同

### 5.1 输入与协议

KernelClient 只知道绑定实例、平台、generation、内核版本、attempt ID、已规范化 payload 和取消/超时，不接受分组、余额或另一个账号列表。一个 attempt 只提交一次，不在内部切账号或自动重发。

| 合同 | Claude | Codex |
|---|---|---|
| 推理二进制 | kin-kernel + 固定匹配 CLI/loader/libs | kin-codex-kernel |
| 启动方式 | kin-kernel --gateway-worker --config /run/kin/kernel.json | kin-codex-kernel 配置文件路径 |
| IPC | guest 专有 Unix socket 上 HTTP，经第 6 节宿主/guest 桥连接 | 同左 |
| 推理端点 | POST /internal/v1/messages | POST /internal/v1/codex/responses |
| 健康 | GET /internal/health | 同左 |
| 取消 | POST /internal/v1/cancel，request_id 与 attempt 一致 | 调用端同合同，实际支持必须 PoC |
| 内部认证 | 每实例 x-kin-internal-token，兼容头仅按固定版本使用 | 同左 |

Claude envelope 包含 request_id、body、headers、stream、delivery_mode、preserve_cache_breakpoints、cache_ttl；request_context 只有固定版本明确支持才发送。Codex envelope 包含 request_id、body、headers、stream、session、options；首版固定 HTTP 上游传输，previous_response_id 不启用。session_id 若需发送，只来自已有请求会话标识并隔离到当前账号，不建立新的 session 存储。

headers 使用平台允许列表，内部 token 由服务注入；客户端不得指定 socket、路径、runtime ID、内部认证头或任意出站身份。request_id 是单次尝试标识，不能拿它代替会话 ID。原客户端 request ID、计费 request ID 和内核 attempt ID 分别保留，不用新 attempt ID 覆盖现有去重键。

### 5.2 SSE 与完成时点

适配器分别记录：dispatch_state、downstream_written/semantic_committed、protocol_terminal_seen、kernel_response_closed、usage_presence。一个 completed 布尔值不足以表达全部状态。

- Anthropic 标准 message/content/tool 事件、Responses 标准事件按原下游协议发送；长工具 JSON、thinking、tool-only 结果必须正确解析。
- response_headers、job_done 及其他内核控制事件在 adapter 消费，不无条件透传。内部 token、诊断路径不进入公共 headers。
- Claude message_stop 后继续读取 job_done/trailers，直到正常 EOF；HTTP 200、message_stop 或 health 成功都不单独代表完整业务成功。
- 为避免先宣告成功再发现用量/尾部缺失，首版只暂存有限的标准终止事件，待 kernel EOF 与必要 metadata 核对后再发出；正文增量仍实时转发。尾部读取受剩余请求预算和固定完成超时限制，不能无限等待。
- 标准终止缺失、HTTP 200 内的 error、异常 EOF 均进入非成功结果。已发送内容不能撤回，流内错误按现有协议处理，不再追加另一份 JSON 响应。
- 首版不新增自己的 SSE 心跳；Claude 与 OpenAI 对“已经写出”的现有判据不同，必须保留其限制。
- 异步 usage 任务只接收结果快照，不持有 gin.Context、大请求体或仍会变化的内核 usage map。

### 5.3 用量与原计费的衔接

从 JSON、SSE、trailer 读取用量，用后到的权威累计字段覆盖/补齐；**不把每个事件的累计 token 相加**。trailer 只有 totals 时仍保留 SSE 的缓存细分，缺字段与显式 0 区分。

Claude 映射 input/output、cache read、cache creation、5m/1h creation 细分；OpenAI 映射总 input/output、cached_tokens 等原 OpenAIUsage 字段。OpenAI 原 RecordUsage 会把总 input 拆成互斥桶，adapter 不能先减一次缓存再交给它重复扣减。

结果还必须填写 requested/mapped/response model、实际 service tier、reasoning effort、duration/TTFT、disconnect、可信的上游响应标识和限流头。UpstreamEndpoint 表示实际 provider 业务端点，不把私有内核 socket URL伪装成上游 API。kernel version/attempt ID 放诊断字段。只支持能完整表达在原计费结果中的首版请求类型。

价格、倍率、渠道归因、订阅、API Key 配额、PricingAt、payload hash、计费去重与扣费操作全部沿用原入口。Claude 最终请求 hash 沿用当前 attempt 的业务 body；内部 envelope、随机 token、attempt ID 不参与业务请求 hash。OpenAI 保留当前 Handler 的 hash 口径。

返回合同必须固定为：

| 情况 | result | error | 原流程中的处理 |
|---|---|---|---|
| 正常结束且有必需的权威 usage | 完整现有结果 | nil | 原成功路径提交一次 |
| 断流/取消/失败，但已有可归属的 usage | partial 现有结果 | 非 failover 错误 | 原 partial 路径提交一次，不重放 |
| 确认未执行且符合原策略的可重试失败 | nil | 明确受限的 UpstreamFailoverError | 原预算下重选/重试 |
| 已提交，是否执行或完成未知 | 有 usage 则 partial，否则 nil | 非 failover 错误 | 记录未知/失败，不自动重放 |
| 缺全部权威 usage，包括尾部丢失 | nil | 非 failover 错误 | 不构造全零成功账单，记录 usage_missing |
| nil result + nil error | 禁止 | 适配器合同错误 | 不得进入原成功路径 |

Claude ForwardResult 当前没有 OpenAI 的 UsageUnavailable 标记；首版采用上表的失败合同，不为此改造计费系统。已知部分用量继续按原规则记录；未知部分不编造、不估算。不得把“未获得用量、尚未扣费”描述成零成本成功，也不能承诺未知消耗已结算。

### 5.4 重试、取消与错误分类

新客户端不移植 vm2api kernel-router 的 transport/401 自动重试。sub2api 是唯一外层编排者，内核闭源部分是否另有重试只能通过 PoC 观察并记录，不能声称全链路 exactly-once。

- not_dispatched：确认未发给内核，可按原策略重选。
- explicitly_rejected：固定协议证明请求被拒绝且未执行，结合限流/鉴权分类决定重选；不能仅看一个 HTTP 502。
- dispatched_unknown：请求可能已进入 CLI/upstream，无论客户端是否看到文本，都不自动重放。
- downstream_committed：保留原 Handler 已写保护；SafeToFailoverAfterWrite 默认 false，不能据 kernel 的 committed=false 放开。
- 已知 usage 的 attempt 不返回可重试 failover 错误，避免现有 error 分支跳过 partial 记账或重复消费。

区分 runtime 连接/鉴权失败、provider 401/403、429/容量、业务校验、断流、客户端取消。内核有把 provider 错误折叠成 502 的情况；只按固定版本可验证的结构/限定映射恢复分类。runtime 故障不得写成账号授权失效；未知 provider 状态不得据字符串永久禁号。内核内部认证 token 失败也不是 OAuth 401。

客户端取消先停止向客户端写入，并以独立短超时 cleanup context 对同 request_id 发送 cancel，再结束/关闭本次流；不能使用已取消的请求 context 发 cancel。保留此前已经取得的用量。不因为取消失败重发推理。

原账号并发许可最终仍由原 Handler 释放；guest 内核执行槽是另一事实。取消未确认且无法确认槽位结束时，记录未释放状态并暂缓该实例新增请求，后台 inspect 恢复，不把 Handler release 当作内核已释放的证据。ready_slots=0 只表示容量可能耗尽，不能直接重启；重启在受限 reconcile 中处理，业务请求不创建/反复重启实例。

## 6. KVM 最小运行支撑与凭据投影

### 6.1 唯一首版部署规格与预检

固定单机 Linux amd64 宿主、Ubuntu 24.04 amd64 guest、QEMU/KVM、OVMF UEFI、qcow2 backing+overlay。QEMU 由每账号 Docker runner 封装，runner 网络命名空间负责隔离固定转发端口；不同时实现裸宿主 QEMU 和另一套端口分配。

部署固定 runner image digest、guest base bytes/hash、QEMU/OVMF 版本、guest scripts/systemd units、kin-computer 与推理 payload 的 size/hash。上游 cloud image 的 current/latest URL 与 apt 当前包不构成版本锁；运行时不能一边创建账号一边不受控下载/升级 guest。guest 必需的 9p、virtio、Python、SSH、setpriv 等依赖应在发布镜像阶段准备并验收。

预检按实际 QEMU UID/GID 与 runner 权限进行：

1. Linux x86_64、CPU 虚拟化扩展与 KVM 驱动可用；外层也是 VM 时，必须验证实际 nested virtualization，而非只看配置声明。
2. /dev/kvm 是可用字符设备，降权后的进程、device cgroup/安全策略能访问。
3. 使用相同 runner/QEMU 完成 KVM VM 创建检查；上游只挂设备运行 /bin/true 的探测不足以证明可用。
4. 真实启动后通过 QMP query-kvm 等确认 enabled=true，并核对 guest boot、内核进程及设备信息。
5. 检查内存、vCPU、剩余磁盘、overlay backing 路径、OVMF 与 guest 模块。
6. 固定 -accel kvm，allow_tcg=false，不允许 KVM 失败后改用 TCG、普通 Docker slot 或宿主推理。

当前 macOS arm64 开发机可进行源码/控制面工作，不能作为 Linux amd64 KVM 验收证据。本阶段不启动虚拟机，后续 K1 需要满足以上要求的实际 Linux 环境。

### 6.2 创建、启动与实际请求拓扑

~~~mermaid
flowchart TD
    A[sub2api 原账号与请求服务] -->|本机管理 UDS| B[受限 runtime-helper]
    B --> C[每账号 Docker QEMU runner / dev-kvm]
    C --> D[QEMU -accel kvm / 独立 guest OS]
    A -->|HTTP / 实例专有宿主 UDS| E[runner socat]
    E -->|runner loopback / QEMU hostfwd| F[guest 固定 relay]
    F -->|guest 本地 UDS| G[guest kin-kernel 或 kin-codex-kernel]
    G --> H[guest CLI 或内核出站 / 指定代理]
~~~

helper 是唯一持 Docker 操作权限的新增组件，主 sub2api 不挂 Docker socket。管理接口只接受固定 schema 的 prepare、apply-config、inspect、drain/stop、remove；实例/账号/generation 必须匹配。apply-config 仅按期望版本应用凭据/出口配置，执行固定 guest 回执检查及必要的排空后内核服务重启，不重建 guest 磁盘。不接受任意镜像、宿主路径、shell 命令或 QEMU/Docker 参数。

最小创建顺序：

1. 原账号持久化 mode 后，建立唯一运行绑定；生成并保存实例 UUID、硬件身份与配置版本。
2. 固定基础盘 → 该实例 qcow2 overlay，创建独立 OVMF_VARS、cloud-init instance-id、NoCloud seed 和受限维护密钥。
3. 准备只读 payload、当前账号凭据投影、专有 home/run/kvm 目录和固定出口。
4. helper 幂等启动 runner；其 QEMU 使用 q35、KVM、指定 CPU/内存、virtio 磁盘/NIC，以及已持久化 SMBIOS/MAC/disk serial。
5. guest 通过 cloud-init/固定 bootstrap 运行 systemd；guest setup 挂载必要共享、设置 hostname/machine-id 后启动对应推理内核与 relay。
6. 检查 KVM accelerator → guest boot/硬件身份 → relay → 内核 health → 凭据/出口应用版本 → 原账号业务 probe；满足原准入与运行条件才参与调度。

PID 层次不能混写：runner PID1 是 entry；QEMU 是 runner 子进程；guest PID1 是 systemd；kin-kernel/CLI 是 guest 内进程。

参考传输链明确为：

~~~text
sub2api → 宿主可见 run/kernel.sock
  → runner socat
  → runner 127.0.0.1:7070
  → QEMU hostfwd 到 guest:7070
  → guest relay
  → guest /run/kin-guest/kernel.sock
  → kin-kernel → guest CLI
~~~

这不是通过 9p 共享同一个 Unix socket。9p 只共享文件；宿主和 guest 属于不同 OS 内核，必须保留字节转发桥。首版使用固定本机 relay，不建设远程节点系统。上游 relay 只是双向字节转发，没有账号管理、调度、凭据应用回执或 cancel 编排能力，这些不能误写成已有特性。

固定 7070/2222 分别处于各账号 runner 的网络命名空间，不向宿主公网发布 guest 推理或 SSH 端口；主服务只能访问对应实例的 UDS。内部 token 穿透到真正内核校验，每实例独立，文件 0600、私有目录 0700。guest relay 的监听范围必须限制于该实例的专用管理路径。

Claude 复用已核对的 guest 服务合同。**Codex 必须新增 guest 内 kin-codex-kernel 服务、guest 配置与单账号凭据、codex-kernel.sock 及固定 relay 目标**；不能连到宿主/runner 内的 Codex 进程。两个平台可共用相同 runner 转发端口，由账号平台固定唯一 guest socket 目标；若需保留不同宿主 socket 文件名，绑定必须精确对应，不能在请求中任意指定。

### 6.3 硬件身份、磁盘与恢复

实例创建时生成一次，持久保存：SMBIOS type 1 的 UUID/serial/描述字段、合法单播 MAC、virtio 磁盘 serial、hostname、guest machine-id、cloud-init instance-id。Codex device_id/installation_id 独立保存，不能与 SMBIOS UUID 或请求 ID 混用。

参考机制使用 kin-computer create --runtime kvm --vm-id ... --out ... 生成 identity.json / qemu-smbios.args，并由 machine spec 持久化 MAC、disk serial。首版可使用固定版本 kin-computer；其输出必须解析为允许字段，并保证 identity UUID 与真正 QEMU -uuid/type1 参数一致，不能把任意输出当 shell/QEMU 参数执行。这里只承诺已注入并实测的 SMBIOS type 1、NIC MAC 与磁盘 serial，不泛称已定制所有虚拟硬件属性。

| 操作 | 硬件身份与持久资产 | runtime 版本 |
|---|---|---|
| 内核服务/guest/runner 正常重启 | 保留身份、overlay、OVMF、home、账号归属 | 更新相应 generation/guest_boot_id，旧连接失效 |
| 重建外层 runner | 继续挂原实例目录；不重建 guest 系统盘 | 重新检查 boot、relay、凭据后 ready |
| 重试一次未完成创建 | 复用已保存身份与资源；不重抽指纹 | 按实际资源恢复 |
| 复制新账号 | 新实例、新硬件身份、新磁盘；不复制活跃绑定 | 全新 generation |
| 换基础盘、扩盘、重置 guest | 首版不开放自动流程；不匹配时停止准备并报需维护 | 不暗中创建新磁盘继续接单 |

上游 entry 在 base.id 或 disk_gb 变化时会删除 disk.qcow2 重建；**禁止照搬这段破坏性行为**。版本固定，普通重启不清盘。损坏/缺失身份也应保留诊断并拒绝启用，不自动重抽身份掩盖错误。

停机先阻止新业务并排空/取消，再 QMP system_powerdown，在有限期限内升级为 quit/终止；必须确认 QEMU 已退出且不再使用磁盘。删除账号时先持久保存清理意图与精确实例标识，失败保留记录；不删除其他实例或无关业务文件。

基础盘、overlay、OVMF、home、identity、凭据和恢复记录都是状态资产。仍被 overlay 引用的 backing image 不能按编译缓存/旧镜像规则删除；磁盘清理须检查引用关系。本期不提供运行中迁移、磁盘扩容或 VM 管理台。

### 6.4 sub2api 权威凭据 → guest 投影

新增 CredentialProjection，不新增 CredentialLifecycle：

1. 授权、手动更新、导入和刷新继续走原服务；DB 持久化成功才产生权威版本。
2. 请求准备调用原 TokenProvider/OAuthRefreshAPI 后重读权威账号。缓存 token 或 refresh 返回值不能替代持久化成功证明。
3. 用规范凭据内容的内部版本/摘要判定新旧，不只依赖刷新写入的 _token_version。按账号串行投影，旧任务不得覆盖新版本，摘要与秘密不对外暴露。
4. 固定 guest 模板提供本实例独有的只读凭据目录（例如专用 virtio-9p share）；共享目录中原子更新文件，guest 的缓存/挂载和可见性必须验证。不能仅单文件 bind，也不能将整个可写 home 当作只读凭据保证。
5. 建立“desired revision → guest 确认可见 → kernel 重载或排空重启 → applied revision + guest_boot_id + generation”的确认。回执只含版本与状态，不回传 token。参考字节 relay 不提供此功能，需通过 helper 的固定受限 guest 操作实现；不暴露通用 guest exec。
6. 首版未证明可靠热加载时，采用停止新请求、排空、重启 guest 内核服务后确认的保守路径；宿主文件写成功不等于 guest 内核已使用新凭据。
7. 优先只投影 access token 和必需 metadata，不给内核可消费的 refresh token。若 payload 必须持有 refresh token，必须证明能可靠关闭自刷新；只读文件不能阻止内存中刷新。
8. 无法满足单一刷新者、guest 应用确认或权威持久化合同的平台不开放 VM。此版本不引入 kernel→DB 回写、第二 refresh worker、spool 或凭据库。

Claude 使用 claudeAiOauth、毫秒 expiresAt，以及固定文件名/symlink；可写 CLI home/cache 与只读凭据 share 分开。Codex 顶层 accounts 数组只放绑定账号，稳定 device_id 与平台配置固定到该 guest。

原模型/额度查询和授权维护继续用 sub2api 服务及账号代理；本功能的隔离范围是推理执行。推理测试和初始业务准入必须走 guest 内核，不能普通 HTTP 成功就标 VM 就绪。

### 6.5 出口、运行状态与原初始准入

代理继续使用原 account.proxy_id。VM 启用须绑定通过 K1 的现有代理类型/配置；不支持或未应用时拒绝准备/调度。QEMU user networking/slirp 不会自动使用 SOCKS 代理，KVM 也不自动提供独占公网 IP。

固定 runner 隔离网络与出口组件/规则仅放行指定代理路径；涵盖 guest、runner、DNS 与 IPv6。首版 IPv6 禁用并验证，DNS 按固定受控方案走允许出口，不照搬 guest 直接使用 8.8.8.8 后宣称全部流量经过代理。代理故障时禁止 default bridge、宿主或直连回退。这里只接既有代理配置，不新增代理池产品。

proxy_id 变化后停止该实例新请求，排空并应用网络/config 版本，再恢复。QEMU 活着、guest 可达、relay 可达、kernel health、凭据已应用、slot 容量是不同事实；heartbeat/relay 故障只影响 runtime 可用性，不直接改成账号 OAuth 失效。ready_slots=0 不等于 guest 故障。

业务可调度仍要求原账号准入 AND runtime ready。原 Schedulable/InitialAdmissionPlan 语义不变；新 VM 的 runtime 条件起初为 false。reconcile 只推进运行状态，不能无条件 Schedulable=true 或重新启用管理员停用账号。

在原 AccountTestService.TestAccountConnection 增加 VM 分支，共用 kernel client/adapter。仅服务端内部初始准入 probe 可以访问已准备但尚未通过业务准入的 guest；不走公共 selector，不临时启用账号。只豁免“等待初始准入”原因，不豁免删除、管理员停用、过期或模型/权限限制，且实际占用内核容量。

无既有 InitialAdmissionPlan 时，复用原账号测试服务执行一次有限 VM 就绪验证，仅推进 runtime 状态；有计划时继续原计划，不创建第二套测试系统。probe 不挂用户 API Key 账单，沿用原测试结果与记录；上游实际调用成本如实记录。普通账号测试不变。

## 7. 数据与兼容合同

只增加两类持久数据，不预建 nodes、templates、jobs、events、VM 账单表：

- accounts.execution_mode：normal / vm_kernel；数据库及旧创建 API 默认 normal。更新缺字段保持原值，未知值拒绝；与 platform、type、既有 account_mode 独立。
- 一份账号附属 runtime 记录（拟名 vm_account_runtimes）：唯一 account_id、instance_uuid、runtime_kind=kvm、runner container_id/QEMU 观测、本机 scope、generation、guest_boot_id、desired_state/phase、稳定硬件身份与磁盘/base引用、固定载荷版本、期望/guest已应用凭据与出口版本、观测时间及脱敏错误。内部 token 留实例秘密文件，不放公开 DTO。

phase 至少区分 pending/preparing、kernel_ready（可进行受限 probe）、ready、draining、error/removed；busy 是容量观察，不能覆盖健康/管理员禁用状态。账号删除期间的清理记录必须可继续被 reconcile 找到，清理完成前不能丢失实例身份。

首版不开放已存在账号直接 normal↔vm_kernel 切换；新增账号选择模式，编辑保留模式。普通账号导入保持旧行为；VM 导入保留 mode，重建本机实例并待验证。复制不复用活跃 container/socket/token。导出不携带运行秘密、本机路径或可在另一部署直接认领的实例 ID。

所有模型、repository 手写映射、DTO、缓存/快照、导入导出均携带 mode。新功能默认关闭；未启用时普通部署不要求存在 helper/Docker/payload。先升级所有可能调度这些账号的进程、清旧快照，再开放 VM 创建。回滚旧二进制前必须持久禁用 VM 账号并排空、清缓存，不能只关 VM 开关让旧程序把它们当普通账号。

## 8. 最小实施任务与停止条件

| 顺序 | 工作包 | 产出 / 通过条件 |
|---|---|---|
| K1 | 固定载荷真实 PoC | Linux amd64 真 KVM；验证 query-kvm、独立 guest boot、SMBIOS/MAC/disk serial 与重启稳定性、磁盘不被重建；Claude/Codex 分别在 guest 内运行，验证 relay、真实请求/usage/取消/出口及 guest 凭据应用。冻结每平台版本与能力，不满足合同即停止 |
| K2 | 单次 KernelClient + 两个平台 adapter | 输入 envelope、控制事件过滤、尾部读取、用量映射、return matrix、错误分类、取消与超时通过协议 fixtures 和真实样本；无账号选择/扣费逻辑 |
| K3 | 固定 KVM helper、guest bootstrap、runtime 记录与 CredentialProjection | 单账号磁盘/身份持久化、QEMU/guest 启动、固定 relay、guest 应用回执、版本 fencing、排空/恢复/清理；无节点平台/独立刷新器 |
| K4 | 原账号 mode 与 Service.Forward 接入 | 在既有表单/API/模型/缓存传播 mode；selector/准入增加检查；原测试入口接入 probe；原成功/partial usage 与重试流程工作；不支持组合提前拒绝 |
| K5 | 双链路验收、文档与发布检查 | 第 9 节全部适用项通过，原普通功能保持，实际调用与收费证据可关联，功能默认关闭且可回滚 |

K1 先行。K2/K3 在合同冻结后可以分工，K4 接入以二者就绪为前提。不要先铺完表单、数据库和调度后才发现闭源载荷不能满足凭据或 guest 内运行合同。

不把前端完成、mock HTTP 成功、容器 running 或 health 200 当作 VM 完成。实施可按平台分阶段交付，但必须在入口清楚显示哪个平台已支持；不会用外层 runner 内进程、TCG 或普通 HTTP 作为未通过平台的替代验收。

## 9. 有效验收清单

| 验收组 | 必须证明 |
|---|---|
| 普通路径 | 老账号/旧 API/普通创建编辑导入、原初始准入、插件/WS/特殊功能、原价格与账单保持；关闭 VM 时无新运行依赖 |
| 原调度规则 | 混合普通/VM 账号遵守原分组、优先级、模型许可、RPM/并发；所有 selector/cache/sticky/fallback 路径排除不就绪和不支持的 VM；变更 generation 后旧连接不再使用 |
| 真实执行 | account_id、instance、generation、guest_boot_id、kernel version、attempt 可关联；KVM enabled、独立 guest OS，推理内核/CLI PID 确在 guest 内；请求到正确 guest UDS/端点 |
| KVM 身份/磁盘 | guest 实读 SMBIOS type 1、MAC、磁盘 serial 与持久配置一致；重启/重建 runner 不变；base/spec 不匹配不删除 overlay；活跃 backing 引用保留 |
| 流协议 | 文本、thinking、tool-only、长工具 JSON、完整上下文工具往返；message_stop→job_done→trailer→EOF 完整穿越双层 relay；不泄漏私有事件，不提前成功/释放 |
| 用量与账单 | 用同一业务用量分别经原路径/VM 结果进入原计费口径，价格/倍率/配额相同；cache totals 不翻倍、TTL细分不丢、mapped model/tier 正确；成功和 partial 各只入账一次 |
| 用量缺失 | HTTP 200 但无 usage、无 terminal、trailer 丢失不会生成伪零成功；部分已知用量不被丢弃；正常零用量与未知区分 |
| 错误与重试 | 明确未发出可原预算重试；提交后丢响应不重放；502 折叠的 provider 错误不误判为节点故障；流内错误不再补 JSON |
| 取消与容量 | 排队、首 token 前、输出中取消都有同 attempt cancel；账号许可释放与内核 slot 释放分别有证据；busy 不引起重启，未知取消结果不继续堆请求 |
| 凭据/代理更新 | 现有刷新和手动更新获得 guest applied revision/boot/generation 确认；并发旧投影不覆盖新版本，无第二 refresh 消费者；代理/DNS/IPv6 故障无直连 |
| 初始准入 | 未准入的 VM 可由受限管理 probe 验证，公共请求不能调用；健康恢复不能重新启用管理员停用账号 |
| 故障恢复与删除 | 重复创建不重复 QEMU/磁盘/身份；重启 inspect 找回绑定；QEMU/relay 断线按执行未知不重放；删除失败保留记录且核对 QEMU 退出 |
| 部署与回滚 | regional/特殊执行组合被提前拒绝且普通功能不受影响；导入/复制不认领旧实例；回滚前禁用与缓存失效有效 |

测试采用三层证据：纯协议 fixture 测 adapter；原 Handler/selector/usage 的定向回归证明接入语义；真实固定 payload 的集成验证证明运行链路。普通 HTTP mock 只能证明外围处理，不能替代第三层。

之后测试/构建遵循分支基线说明的临时 GOCACHE/GOTMPDIR、磁盘与 Testcontainers 会话清理纪律。纯上游 integration harness 的 os.Exit 跳过 defer 问题在实际容器测试前单独修复或明确补偿；非 CI Docker 不可用时成功退出可能只是跳过，必须核对实际执行。

## 10. 尚需 PoC 确认的技术门槛

以下是已知验证项，不是本版继续扩范围的理由：

- 固定 guest/QEMU/OVMF 组合、KVM 真可用性、9p/virtio 模块、硬件身份和磁盘持久化；闭源 payload 的 ELF/loader/libs、最小 home/seed、CLI 及 guest 非 root 权限。
- Codex 从控制面 spawn 改到 guest systemd 服务/专属 relay；Claude 已有 KVM guest 启动源码也仍要验证固定 payload 独立运行。
- 内核是否支持 access-token-only / 可靠关闭自刷新、是否热读取、哪些目录必须可写。
- 端到端 relay 的内部鉴权、cancel、accepted 信号、容量/恢复与尾部 metadata；guest 凭据应用确认不是原 relay 自带能力。
- 有工具/思考输出时的用量字段、内核内部自动重试、业务流量的实际出口。
- 并发与完整上下文工具往返不串账号；跨重启状态型续聊不在首版承诺内。

固定 deploy/payload.json 指向 payload-2026.10.19-kernel-cli，部署校验 size/SHA256、KVM runner/guest/base/guest脚本及匹配镜像 digest；不能自动跟随 latest。公开源码不能重建全部同版内核。使用/分发受参考项目 LICENSE 约束，商业使用需书面授权；本期不开发授权管理模块。

## 11. 固定版本证据索引

sub2api 路径均相对本工作区，行号按纯上游 v2.15.0，实施时以函数符号为准：

| 文件与基线位置 | 证据 / 必须覆盖 |
|---|---|
| backend/ent/schema/account.go；internal/service/account.go；admin_service.go:426、462 | 账号模型、创建/更新输入 |
| backend/internal/service/admin_account.go:431、463 | 原 InitialAdmissionPlan 与 Schedulable |
| backend/internal/repository/account_repo.go:172、620、1002、4093 | Create/Update/UpdateCredentials/mapper |
| backend/internal/handler/dto/types.go:218、347；mappers.go:242、479 | 列表/详情 DTO；不泄露实例秘密 |
| frontend/src/components/account/CreateAccountModal.vue、EditAccountModal.vue；src/types/index.ts；api/admin/accounts.ts | 原表单加 mode；默认普通 |
| backend/internal/service/gateway_forward.go:91、123、154、288、811 | Claude shared guard、普通执行前插点、beta/model、accepted callback |
| backend/internal/service/openai_gateway_forward.go:93、124、538、821 | OpenAI wrapper、turn admission、映射、档位策略 |
| backend/internal/service/openai_turn_admission.go:378；openai_codex_ticket.go:817 | 权威账号检查与普通 ticket gate 分界 |
| backend/internal/handler/gateway_handler.go:891、924、938、1055、1103、1135、2172 | 回调、分流调用、部分账务、已写保护及 count_tokens |
| backend/internal/handler/openai_gateway_handler.go:975、1050、1109、1140、1191、1652、3221 | 选路、HTTP continuation、并发 wrapper、partial usage、其他能力入口 |
| backend/internal/handler/openai_chat_completions.go:285 | 原 Chat Completions 入口必须排除未支持 VM |
| backend/internal/service/openai_account_scheduler.go:2362、2654、2722、2749；local_scheduler_snapshot.go | capability、fallback、sticky/previous、snapshot |
| backend/internal/service/gateway_service.go:607、623、701；openai_gateway_service.go:227、238、320 | 两种 usage/result、failover 错误与调度成功判据 |
| backend/internal/service/gateway_usage_billing.go:40、656；openai_gateway_usage.go:156、164、170、190 | 原账务输入/入口、regional guard、unknown usage、缓存互斥桶 |
| backend/internal/service/oauth_refresh_api.go:159、195、294；claude_token_provider.go:55；openai_token_provider.go:134 | 原刷新锁/持久化/版本、原 TokenProvider |
| backend/internal/service/account_test_service.go:376；scheduled_test_runner_service.go:169 | 复用测试与内部 probe 入口；原部分测试直接 HTTP，不会自动经过新增 Forward |
| backend/internal/handler/admin/account_data.go | 导入导出；复制与恢复检查 |
| backend/internal/repository/integration_harness_test.go:45、76、86、134 | TestMain 的 defer/os.Exit 清理问题 |

vm2api 的 KVM 创建和运行证据：

- [设备预检及其证据限制](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/vm/kvm-host.mjs#L66)
- [KVM runner 及挂载](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/vm/vm-runtime.mjs#L804)、[runner 镜像依赖](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/Dockerfile#L8)
- [overlay 重建风险](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/kin-kvm-entry#L107)、[QEMU/SMBIOS/MAC/disk serial](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/kin-kvm-entry#L279)
- [宿主 UDS 桥](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/kin-kvm-entry#L357)、[guest 字节 relay](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/guest/kin-kernel-relay.py#L7)
- [guest setup](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/guest/kin-guest-setup#L176)、[guest 内核启动](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/guest/kin-kernel-start#L1)
- [kin-computer 身份持久化](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/identity/slot-computer.mjs#L95)、[machine spec](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/vm/machine-spec.mjs#L272)

vm2api 的公开调用端证据：

- [UDS、内部认证与 cancel](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/go-worker-client.mjs#L80)
- [usage 字段合并与 committed](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/go-worker-client.mjs#L233)
- [CLI beta 通道与 envelope](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/go-worker-client.mjs#L430)
- [流控制事件、尾部读取与 usage](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/go-worker-client.mjs#L802)
- [参考 router 的重试行为](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/kernel-router.mjs#L267)
- [Claude socket 与健康/slot](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/rust-kernel-client.mjs#L8)
- [Claude 内核配置](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/rust-kernel-supervisor.mjs#L677)
- [Codex 内部端点](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/codex-kernel-client.mjs#L44)、[envelope](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/protocol/handle-codex.mjs#L737)、[宿主 spawn](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/transport/codex-kernel-supervisor.mjs#L209)
- [固定 payload](https://github.com/dofastted/vm2api/blob/v1.3.206/deploy/payload.json)、[LICENSE](https://github.com/dofastted/vm2api/blob/v1.3.206/LICENSE)

本纯上游分支没有原本地 ticket_borrow 模块；不移植本地定制代码，也不把其 continuation 兼容作为本功能前置。上游本身已有的 ticket/harvest、插件、WS 仍保留在普通模式，VM 按上述边界处理。
