# Upstream runner 与 Go helper 当前接入合同

`upstream-v1` 已实现，接入固定 vm2api `v1.3.206` 的 runner/guest 物料。部署入口见 [部署 README](../deploy/vm-kernel/README.md)。

本轮 GPT/Codex 的已列范围真实验收完成：24笔完成请求、24个唯一账单（基础8 + HTTP工具12 + WS工具2 + 两轮显式取消后的新连接恢复请求；断连不新增业务请求），input 47118、output 301、费用0.00545494、余额9.99454506。显式取消后generation 12→13、新连接恢复；断连后13→14，guest boot与进程证据更新并恢复ready；该断连场景没有再发成功业务请求。连续90秒十次ready均为true。host及guest均验证指定代理可通、直连TCP与DNS TCP53拒绝，IPv6/direct resolver关闭，access-only投影只读。原API停用后请求503且Docker停止；删除后账号404、容器不存在，tombstone/磁盘/身份保留且无新增账单。详细范围和证据见 [GPT验收记录](vm-kernel-gpt-acceptance-20261022.md)。这些结果不代表所有故障路径均已实测；硬件KVM、Claude仍未验收。

## 业务与权限边界

保留原账户 execution_mode、选择器、分组/权限、Forward、跨协议、WS 与 RecordUsage。VM 是原账号的执行方式，不新建一套账号/计费系统。公开请求不触发 VM 创建；控制面持有刷新责任，将 access-token-only 凭据和代理 revision 投影给 guest。拒绝 refresh token，guest 不执行第二套账号刷新。

保留 account/instance/generation/guest boot identity、凭据和代理 revision、drain/delete 与资源 ownership。并发跟随原账号配置；业务与管理探测共享 runtime 活跃计数，在达到配置上限时等待且重新复核，不重复原 Redis lease。helper 私有 capacity=0 表示不增加固定单执行槽，公开状态使用账号配置容量。未知、取消或部分完成请求不得以重试或普通模式回退掩盖。旧连接不能越过 generation 或账号绑定变更继续使用。

主应用非 root 且不挂 Docker socket。helper 为 Linux root，提供私有 UDS；使用专用 service-gid 的 socket 0660、父目录0710及实例路径 group-x，仅允许连接/遍历，不开放凭据与 token 的读权限。coordinator lock 由部署预创建为应用 UID:0600，并保留 inode；各进程挂载同一个父目录及绝对路径。InternalToken、KernelSocket、代理认证和凭据不能进入公开 DTO 或普通日志。

## 驱动职责

| 操作 | 当前合同 |
| --- | --- |
| Preflight | 固定 immutable registry digest 或本地 image ID；runner 架构匹配 Docker server；全部 payload 文件 SHA-256 校验；验证实际 accelerator |
| Ensure | 固定 upstream entry、资源、UID/GID、身份和 share 映射；验证容器 ownership；保持磁盘和启动布局，失败 inspect 不视作缺失 |
| Inspect | 分别验证容器/QMP、实际 accelerator、guest boot、内核进程及 applied receipt；不能创建或隐式恢复 |
| Apply | 更新投影，通过 guest 读取摘要/revision，启动或重启预期内核，确认进程变化和健康后产生 receipt；480 秒内未确认则不可调度 |
| Stop / Remove | 保留归属与版本检查、先 drain 再停止、确认实例状态；持久身份/磁盘/tombstone 保留，不做全局资源清理 |

模板 `runtime="upstream-v1"` 使用完整 payload 根布局，例如 `payload/usr/local/bin/kin-codex-kernel`。guest `/opt/kin-payload` 挂载提供执行物料及 loader/libs，不接受把某一个 ELF 平铺到 payload 根来代替完整布局。上游 `source_sha256` 记录原始固定提交文件；本地补丁与最终物料摘要应区分。

默认 KVM，真实 QMP 必须报告 KVM enabled。只有模板 acceleration=tcg 与 helper --allow-tcg 同时开启才允许 TCG；TCG 不挂 /dev/kvm，不伪造 KVM enabled。host runner 可以是 Docker server 原生 ARM64，guest/payload 保持 x86_64；helper GOARCH 不能代替执行能力检查。legacy 模板双 verified 声明不用于 upstream-v1，发布物料与运行时校验承担该合同。

## 控制面调度与版本收据

inventory 使用短时观察，不等待某个慢 VM 启动。每账号后台任务去重，独立十分钟期限；四个 bootstrap 许可跨轮询共享，满额直接下轮重试。健康观察独立最多十六并发，因此冷启动不会占满健康检查通道。准备 mutation 前再次读取数据库，阻止旧凭据/路由配置在管理员修改后应用。Stop 先关闭 enqueue，再取消并等待轮询及后台任务，防止退出时追加工作。

对已验证且binding/credential/proxy revision均未变化的实例，暂态只读观察错误采用最多三次有限观察：前两次发布不可调度状态并等待后续观察，第三次仍失败才进入原新generation恢复。成功观察清零计数；quarantine、deleting、版本变化不享受此缓冲。它不是推理重试，也不在观察失败期间沿用旧ready放行。

guest receipt 包含当前 generation、boot ID、credential/proxy revision 与 credential digest，并绑定内核实际 PID/可执行文件等进程身份。Apply 的 pending 标记防同一投影轮询重复 restart；新进程及 health 确认后才写 applied receipt。Inspect 要求 receipt 与当前进程匹配。内核 service 的 Restart=no 保证恢复经控制面 fencing；relay 的独立 restart 不代表内核版本可以不受约束变化。

credential_path 指向规范 Codex accounts-array 投影，配置不复制长期凭据。投影私密文件权限保持0600；guest 仅读取 access token。控制桥不得在超时/异常时输出原始凭据或完整命令环境。

## 升级与持久性

不可变镜像引用与实例物料布局必须一致。旧 overlay、NVRAM、维护密钥及 bootstrap 状态不能自动迁移或混用；升级需要明确退役、排空和重新绑定，保留需要恢复的旧磁盘。禁止自动重建 overlay 来“修复”布局或身份错误。单机 coordinator lock 的 inode 不能在运行中替换，这也不是跨主机分布式锁。

## 证据分层

[Codex guest 验证](vm-kernel-codex-guest-verification.md) 记录 guest/private client 真实推理；它不等于公共 handler 与数据库账单通过。基础八次 JSON/SSE/WS 请求、十二次工具闭环、取消和断连各有独立 harness，见 [验收清单](vm-kernel-full-acceptance-checklist.md)。本轮结果已汇总于GPT验收记录；未列明的故障路径仍不视作已实测。

partial usage 的确定性覆盖包括 `vm_kernel_forward_integration_test.go` 的 `TestVMKernelForwardPartialIsAuthoritativeAndNeverFailover` 与 `vm_kernel_ws_integration_test.go` 的 `TestVMKernelWSUDSCancelKeepsAttemptAndPartialUsage`：前者验证 UDS 部分结果的权威 usage 和禁止 failover，后者验证取消保留 attempt/partial usage。它们是 fixture 服务层证据；真实取消若没有产生 usage，不能反向证明带 usage 断流的所有账单场景已完成。

网络强制隔离、DNS/IPv6、代理中断、guest drain、重启持久性和旧连接 fencing 需要分别取证。TCG 通过不代表 KVM 硬件通过；GPT 通过不代表 Claude 通过；health 和 receipt 成功也不单独证明当前凭据被实际推理消费。
