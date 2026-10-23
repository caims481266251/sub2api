# Sub2API 单账号 VM Kernel 部署

当前实现已将固定 vm2api `v1.3.206` 上游运行包接入 Go helper 的 `upstream-v1` 驱动。Sub2API 原账号通过 `execution_mode=vm_kernel` 使用独立 guest 执行推理，账号、分组、API key、选择器和原计费流程继续由 Sub2API 管理。固定上游提交为 `894a78daba0361ecf825e5ec4a0707108cdbae9b`，运行包来源与补丁见 [upstream](upstream/README.md) 和 `upstream/source.json`。

当前总览见 [实现状态](../../docs/vm-kernel-implementation-status.md)，前序首版真实结果见 [GPT 验收记录](../../docs/vm-kernel-gpt-acceptance-20261022.md)。

前序首版 GPT/Codex 的已列范围真实验收完成：24笔完成请求、24个唯一账单（基础8 + HTTP工具12 + WS工具2 + 两轮显式取消后的新连接恢复请求；断连不新增业务请求），input 47118、output 301、费用0.00545494、余额9.99454506。显式取消后generation 12→13、新连接恢复；断连后13→14，guest boot与进程证据更新并恢复ready；该断连场景没有再发成功业务请求。连续90秒十次ready均为true。host及guest均验证指定代理可通、直连TCP与DNS TCP53拒绝，IPv6/direct resolver关闭，access-only投影只读。原API停用后请求503且Docker停止；删除后账号404、容器不存在，tombstone/磁盘/身份保留且无新增账单。详细范围和证据见 [GPT验收记录](../../docs/vm-kernel-gpt-acceptance-20261022.md)。这些结果不代表所有故障路径均已实测；硬件KVM、Claude仍未验收。

当前 30 请求验收最新第四轮为 24 成功、6 条 HTTP 502，尚未全成功通过。client 在途、账号 lease、guest relay 连接峰值均为 30；24 条唯一账单与权威 usage、余额对账一致，费用 0.01061946。第四轮前后 generation 14、guest boot 不变且 ready 保持，结束 lease 全为 0。6 条失败仅确认 guest `hop_err`（`sent=false`、`retry=true`、`error sending request`），没有底层 source，不能归为具体超时；本地峰值不证明 provider 内部 30 路并行，本轮显式 TCG 也不代表硬件 KVM 性能。前面三轮 4/26、0/30、0/30 的成功/失败记录完整保留；独立 post-load 首轮 5/8 通过，HTTP/WS 续接和 Chat SSE usage 呈现失败，6 条唯一账单及余额一致，原失败报告保留。详细边界及证据见 [业务复用修订说明](../../docs/vm-kernel-business-reuse-20261022.md)，不与前序首版账务混用。


## 组件与数据路径

```text
原 Sub2API 公共 API / 账号选择 / 权限 / 计费
  → VMKernelService（账号实例绑定、版本与会话约束）
  → 私有 Unix socket → runner relay → guest relay → Codex kernel

Sub2API 控制面 → 私有 helper socket → Go helper → Docker runner / QEMU guest
```

helper 是 Linux root 的受限控制进程；主应用保持非 root，不挂载 Docker socket。公开请求不会创建 VM；后台控制面负责 prepare、inspect、版本变更及恢复。guest 仅消费 access-token 投影，刷新责任仍在 Sub2API，不能成为第二个 OAuth 刷新者。原 normal 账号和既有余额/usage_logs 计费体系不被替换。

Codex 凭据投影显式设置 `rpm=0`、`max_concurrency=0`，关闭内核的额外准入限制。正常请求只服从 Sub2API 原有账号、用户和密钥配置；上游明确报错后复用原有业务错误处理，不新增 VM 冷却策略。

VM 请求并发复用原账号调度配置，运行时不再增加固定单请求执行槽。业务请求和管理探测共享当前账号配置的并发上限；达到上限时等待任一请求释放后重新校验账号和实例，取消等待不会派发或隔离健康实例。该保护不重复获取原调度器的 Redis lease，账号并发不大于 0 时沿用不限制语义。私有运行状态 capacity=0 表示并发由账号调度器管理，并不承诺内核或主机拥有无限资源。健康观察可以与推理并行；凭据投影更新、未知执行结果恢复和删除必须先等待已有请求结束。取消结果尚无可靠的请求级排空确认时，暂停新请求并在其他在途请求结束后恢复实例，不能中断其他流来抢先重启。未知或部分完成的请求不可自动重放；失败不能切回宿主普通执行路径隐藏 VM 错误。

## 固定物料与加速策略

`upstream-v1` 模板使用不可变镜像：registry `name@sha256:...` 或本地 Docker `sha256:...` image ID；不要配置可漂移 tag。runner 工具架构必须匹配 **Docker server** 架构，不能以 helper 所在机器的 GOARCH 推断。guest/payload 仍为固定 x86_64；ARM64 Docker server 上的 native runner 可以显式 TCG 执行该 guest。

默认 acceleration 为 KVM，必须验证实际 QMP KVM 状态。TCG 只在两个条件同时满足时允许：模板 `acceleration: "tcg"`，helper 启动参数 `--allow-tcg`。不自动降级，不将 TCG 报告为 KVM enabled。

payload_dir 指向完整文件系统布局的根：例如 `<payload_dir>/usr/local/bin/kin-codex-kernel`，不能把二进制平铺到根目录。guest 通过 `/opt/kin-payload` 获取物料并建立预期执行路径。模板 artifacts 必须覆盖 payload 中每个普通文件并固定 SHA-256；新增未登记文件、符号链接、错误架构或摘要不符会拒绝。源脚本的上游摘要不是本地补丁后文件摘要。

模板结构示意（占位值需替换为发布物料的真实不可变引用与摘要）：

```json
{
  "default": {
    "runtime": "upstream-v1",
    "acceleration": "kvm",
    "image": "registry.example/sub2-vm@sha256:<image digest>",
    "payload_dir": "/opt/sub2-vm/payload",
    "artifacts": {
      "/opt/sub2-vm/payload/usr/local/bin/kin-codex-kernel": "<sha256; include every payload file>"
    }
  }
}
```

`upstream-v1` 通过固定物料、实际 preflight 与 guest receipt 校验；用户无需手动声明旧模板的 `kvm_contract_verified` 或 `credential_projection_verified`。

## 非 root 应用的权限与挂载

helper 使用 `--enabled --root <persistent root> --socket <absolute socket> --templates <private template map>`；需要共享服务访问时指定专用 `--service-gid <gid>`，并把应用用户加入该补充组。helper 为 socket 父目录设置 root:service-gid 0710，为 socket 设置 0660。默认未配置 service-gid 时 socket 为 0600。

应用还必须能遍历实例 run socket 的完整父路径，相关目录只授予专用组 execute 权限；仅改 socket 为 0660 而父路径没有 group-x 仍会 EACCES。凭据、internal token、manifest 私密内容不得因此获得组读权限。不要用递归 chmod 777 或递归开放整个 state root 解决访问问题。

主应用会打开 `<helper socket>.coordinator.lock`。helper 启动时自动准备该普通文件，使用 `--service-uid <实际应用 UID>` 指定属主，模式固定为 0600（默认 UID 0 用于 root 应用）。非 root 应用例如 UID 1000，应同时配置 `--service-uid 1000 --service-gid 20000`；无需手动创建锁。现有文件保留原 inode，仅校验/调整 ownership 和 mode，拒绝符号链接、硬链接和非常规文件。不得在运行中删除、替换或 truncate 重建，以免出现两个互不排斥的锁。清空运行数据必须先停止主应用与 helper，再由 helper 自动初始化。

启用 VM 时，主应用在开放 HTTP 服务前获取同一协调锁并保持到退出；获取失败会明确终止启动，不能再以普通健康检查通过掩盖 VM 调度器初始化失败。未启用 VM 的部署不受此启动检查影响。

所有访问同一 helper 的应用进程必须挂载包含 socket 与 coordinator lock 的同一个父目录，并保留相同绝对路径；仅挂 socket 文件不够。helper 返回的实例 KernelSocket 路径同样需要在应用中以相同绝对路径可达。第一版仅支持一个本地完整 Sub2API coordinator，不提供分布式 VM 调度。

`InternalToken`、KernelSocket 和代理认证信息是私有后端数据，不能出现在公开账号 DTO、普通日志、测试报告或终端输出中。主应用仅需要私有通信路径与协调锁，不需要 Docker 控制权限。

## 就绪、版本与恢复

控制桥 Apply 最多等待 480 秒，覆盖显式 TCG 首次启动。单账号后台任务有独立十分钟期限；冷启动/配置变更共享四个 bootstrap 许可，满额时下轮重试。健康观察使用独立十六个许可，不因冷启动等候而让已有 VM 的三十秒 readiness 过期。Stop 先封闭新任务，再取消并等待全部在途后台任务结束。

对已验证且binding/credential/proxy revision均未变化的实例，暂态只读观察错误采用最多三次有限观察：前两次发布不可调度状态并等待后续观察，第三次仍失败才进入原新generation恢复。成功观察清零计数；quarantine、deleting、版本变化不享受此缓冲。它不是推理重试，也不在观察失败期间沿用旧ready放行。

guest receipt 关联 generation、guest boot ID、credential/proxy revision、凭据摘要与实际内核进程身份。本地 guest/kernel 健康及当前 receipt 验证通过后发布 readiness，不再自动调用模型作为 VM 就绪门槛；原账号业务准入独立保留。业务请求使用控制面三十秒有效的本地就绪快照，不逐请求执行 helper inspect；派发前仍执行最终数据库账号状态、credential/proxy revision、路由及 session binding 校验。业务完成不会延长健康快照有效期。已知模型/provider 错误沿用原业务处理，不显示为账号 VM 故障，不触发实例重建。

第二轮实测发现 cloud-init 覆盖 `/etc/hosts`，禁用 DNS 的 guest 因而丢失代理域名映射；同一代理通过 IP 直连可完成 CONNECT 200 与 TLS。当前 guest setup 排在 cloud-init 之后，seed 设置 `manage_etc_hosts: false`，apply 写入、observe 校验实际代理解析投影。此校验不请求模型，也不开放额外 DNS 或直连路径。

第三轮临时部署的 stdin 文件传输失败曾留下空 guest 网络脚本。改为参数传输及 SHA 校验后，源码、镜像、seed、实际 guest 文件一致，apply/getaddrinfo 与公共业务 smoke（HTTP 200、completed、usage）通过；第三轮失败不能归为 hosts 修复源码无效。第四轮恢复 24 条成功，余下 6 条失败仍须独立诊断。

固定同步内核的非 200 响应在限定 64 KiB 内完整读取后，以 `Settled=true` 表示本地 handler 已结束；它不表示上游没有执行，`SafeToRetry` 仍为 false。读取失败、截断、超限或其他未确认结果仍保留 unknown 隔离边界。该修复防止已知错误结果触发无意义的 VM 重建；第二轮 generation 12→13 的误重建记录保留。

`kin-kernel.service` 使用 `Restart=no`，内核崩溃恢复必须经控制面和新 generation；relay 可自行重启，但不能据此承认内核进程悄悄更换。inspect 不创建实例；未知观察不会被当作“实例不存在”。配置变更与删除遵守单账号锁、drain 和版本约束。

runner 镜像、磁盘布局、基础镜像、身份不隐式迁移。已有 overlay 需要升级时，先明确退役旧绑定并确认无在途请求，再按发布流程创建新绑定；不能在旧磁盘上静默混装新启动布局。停止/删除保留持久身份、磁盘和 tombstone，不自动清理业务数据或备份。

## 验收与历史路径

本轮 vmkernel/service 定向及竞态检查通过；代理投影 Python 验证报告记录运行包 53 项通过及追加 receipt 回归后的 bridge 9 项通过。报告位于 `.dev/vm-kernel/verification/business-reuse-20261022/minimal-fix/settled*.log` 与 `.dev/vm-kernel/verification/proxy-hosts-fix-20261022.json`。这些结果不替代真实业务验收，也不能消除第四轮仍有 6 条失败的事实。

当前补齐 HTTP 对 VM 本地续接的账号筛选、历史首次发布的正常 Redis cache miss，以及 VM Chat SSE 的终态 usage chunk。普通 OAuth 限制、续接归属及代际校验和原记账语义保持；Chat usage 输出沿原网关规则，不依赖客户端是否显式请求。接点修复已部署，完整 `post-run2` 为 5/8 通过，三条失败由应用与 guest 日志确认是上游连接 `hop_err`，WS 跨连接续接已成功。随后固定四项 `post-targeted`（HTTP seed/continuation、Chat SSE usage、Messages JSON）4/4 通过，4 条唯一账单、费用 0.00025306、余额一致。八类功能已分轮覆盖成功，不能称单轮 8/8，不能覆盖原始失败或宣称连接波动消失。

最新应用镜像 `sha256:6a54d2868d8b…`、二进制 SHA-256 `de2b8f867d8d…` 已部署健康，完整摘要见 `minimal-fix/deployment-settled.json`；`presentation-final.log`、`continuation-final.log` 确认最终接点定向 Go 验证通过。30 压测第四轮是此前并发/状态版本的 24/30 结果，后续接点补丁只运行上述两轮功能验收，没有重跑 30 路。

按 [完整验收清单](../../docs/vm-kernel-full-acceptance-checklist.md) 区分基础 JSON/SSE、工具闭环、WS 续接、显式取消、断连、计费和 guest 排空。代理配置存在不等于网络强制隔离成立；代理断路、直连/DNS/IPv6 边界应单独取证。

本轮收尾状态：账号 active、schedulable、并发 30，VM ready=true、实际 TCG，generation 14 与 guest boot 保持；应用 healthy、restart count 0。`minimal-fix/final-state.json` 与 `cleanup.json` 保存最终状态及清理证据：独立构建缓存已移除，宿主剩余 106.91 GiB，无运行中的 Testcontainers；正常业务数据、账务证据与 VM 磁盘保留。剩余上游连接波动仍是验收限制。

本目录顶层旧 `build-template`、离线 artifacts、自建 runner 和相关 verified gate 属于 legacy 驱动背景；当前 `upstream-v1` 不要求用户走该旧路径。更详细的当前实现合同见 [adapter contract](../../docs/vm-kernel-upstream-adapter-contract.md)。
