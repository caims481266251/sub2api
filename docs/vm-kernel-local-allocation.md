# 本机 VM 资源分配、持久客户端身份与代理边界

日期：2026-10-22。本文记录分配器增强、单机网络及当轮 TCG VM 验证，并保留随后清空旧业务与 VM 数据的历史结果。用户之后已新建账号；后续自动初始化修复及真实 GPT 验证见 [VM 新建账号自动初始化修复](vm-kernel-creation-reliability.md)。不沿用上一轮业务压测作为本轮模型推理通过证据。

## 实现边界

账号仍使用 Sub2API 原账号、分组、API Key、并发、登录、准入及计费。新增分配器位于单机 helper，本地文件锁及持久账本约束同一 helper root 的资源归属，不提供多主机调度。新建实例自动获得两层独立网络租约及持久客户端身份；这些字段由 helper 生成，不接受账号请求指定。

Docker bridge 是宿主 Docker 层的 runner 网络；guest slirp 是 QEMU 内部虚拟机网络，两者地址不可混用，也不是为每个账号申请独立公网 IP。guest 仍经该账号配置的代理出站。

## 私有字段与地址分配

实例 manifest 的 `network` 对应 `State.Network *NetworkLease`，`client_identity` 对应 `State.ClientIdentity *ClientIdentity`。它们是私有控制面数据，不扩展账号 UI 状态。

| JSON 字段 | 含义与默认偏移 |
| --- | --- |
| `network.network_name` | helper 生成的专属 Docker bridge 名称 `sub2-vm-net-<instance UUID>` |
| `network.subnet` | runner 所用 Docker bridge `/28` 子网 |
| `network.gateway` | bridge 子网基址 +1 |
| `network.address` | runner 固定 IPv4，bridge 基址 +2 |
| `network.guest_subnet` | 同一分配槽的独立 guest slirp `/28` 子网 |
| `network.guest_gateway` | guest 子网基址 +2 |
| `network.guest_dns` | guest 子网基址 +3，仅作为 QEMU 保留地址，不启用 guest DNS |
| `network.guest_address` | guest 固定 IPv4，guest 子网基址 +4 |
| `network.released` | 资源删除确认后记录租约已释放；不是通过请求失败设置 |
| `client_identity.device_id` | 持久 canonical UUID，供实际内核配置使用 |

helper CLI `--network-pool` 默认 `10.240.0.0/16`，`--guest-network-pool` 默认 `10.241.0.0/16`。可配置等长、互不重叠的私有 IPv4 池，池前缀长度必须为 `/16` 至 `/28`，实际分配固定 `/28`。默认每个池提供 4096 个候选槽；已占用或冲突槽不可用，不承诺可创建同等数量的实际 VM。

分配时检查本地 `network-leases.json` 的持久保留项，以及 Docker 网络 inventory 中的已用网段。inventory 读取失败不当成空闲；池耗尽、归属冲突或账本损坏均拒绝分配。现有租约不会因重启、凭据刷新或变更池参数自动重新编号。CLI 地址池应由部署方避开实际环境中的冲突地址，不能据 Docker inventory 检查声称已检查所有外部路由。

Docker driver 创建带实例与账号归属标签的独立 bridge，禁用该 bridge 的容器间通信，runner 使用指定静态 IPv4。启动、观察及删除均核对网络归属、配置与容器实际成员关系，不采用同名外部网络。

QEMU 使用显式 `net`、`host`、`dhcpstart`、`dns` 参数，SSH 与 kernel 转发仍只监听 runner loopback 并指向该 guest IP。新 seed 写入无 DNS 的静态网络配置；guest 每次启动按固定 MAC 应用网络，兼容 cloud-init 不再次初始化的 overlay。控制面 observe 只读核验真实接口地址及唯一默认路由，并将其纳入 receipt；不会通过更改网络来掩盖漂移，也不通过模型请求判断 VM ready。

## 持久客户端身份与已有账号

新实例在外部创建前持久化 `client_identity.device_id`，后续启动、凭据或代理轮换保持此值。runner 写入或核对 `/slot/kvm/codex-installation-id`，再将相同值放入 Codex/Claude 的实际内核配置；已有文件冲突、非法 UUID 或符号链接均拒绝，不静默覆盖。

已有合法安装 ID 会迁入 manifest，规范化为小写带连字符 UUID，逻辑身份不变。manifest 已有身份时必须与安装文件一致；文件不存在时只写入 manifest 的已有值，不重新随机生成。guest receipt 核对实际配置的 `device_id`，关联当前进程与版本，在本地健康检查前后复核。

已有 `network=nil` 的 legacy 账号继续使用原网络，不自动附加新网桥或改 guest 地址。需要独立网段时，运维可在停用并排空请求后，通过私有 helper `prepare` 操作显式传入 `migrate_network=true`，以新 generation 执行迁移，保留磁盘和原 device ID。该参数不开放为账号业务输入；失败保留可重试的迁移状态及已分配租约，不自动回滚身份或把地址释放给另一账号。默认不传该参数时不会迁移。旧 helper/template 没有 `client_identity` 时，runner 保留原安装 ID 行为作为兼容路径。新 helper 会持久化该字段。

该身份是内核配置中的稳定设备 ID。独立 VM、网段与 device ID 不意味着每账号具有唯一 TLS 指纹，也不保证更换公网出口；TLS 行为仍取决于固定内核和网络栈。

## SOCKS5 与失败处理

VM 继续支持 HTTP/HTTPS 代理，并接受 `socks5` / `socks5h`。helper 将内核投影中的 `socks5` 规范化为 `socks5h`，要求目标域名由 SOCKS 服务端解析；不改写用户保存的原账号代理配置。代理服务器本身的域名使用受控 IPv4 投影解析，guest 直接 DNS 仍关闭。

runner 出站防火墙只允许投影中的代理 IP/端口及必要本地通信。代理不可用、SOCKS 认证失败或连接异常时不回退直连，不临时开放 DNS。SOCKS URL 被接受与投影正确，不等于当前环境真实代理认证和模型调用已验收。

网络租约在外部创建前持久化。Docker 创建或删除结果 unknown 时保留租约，不能因超时假定资源不存在并把地址分给另一账号。只有确认容器与归属网络均不存在后才释放；停止实例不释放地址。

模型调用失败沿用原业务错误与计费处理，不因 provider 错误重建 VM 或轮换身份。固定同步内核非 200 响应在限定范围完整读取后，`Settled` 只表示本地 handler 已结束，`SafeToRetry` 仍为 false。读取失败、截断或已派发但结果未确认仍保留 unknown 隔离与排空约束，不自动重放，也不释放网络租约。

## 当前验证状态

- Python 运行包测试本轮已完成 69 项并全部通过，耗时 11.784 秒。新增覆盖网络参数、真实地址/路由观察、seed 静态配置、旧 overlay 配置路径、设备 ID 持久与冲突拒绝、receipt 绑定等。报告为 `.dev/vm-kernel/verification/allocator-identity-20261022/python.json`，这是隔离 mock/fixture 验证，不是 QEMU 或真实模型验收。
- shell 语法检查与相关文件 `git diff --check` 通过；上游 `source_sha256` 保持固定原始来源摘要，补丁说明及 `local_patch_sha256` 描述当前本地运行包。
- 主任务已确认 allocator/service 新 Go 定向验证通过。Docker live 验证通过两个实例的独立网段、静态 IP 重启保持、foreign attachment 拒绝启动及占用网络删除保护；完整日志以主任务报告为准。
- Go 网络、身份、Docker、原账号代理协议定向测试通过；分配器与身份竞态测试、受控迁移竞态测试通过。额外回归覆盖旧版本创建结果未知且未写入 ContainerID 时不提前分配新网络。
- 已将原测试实例实际迁到新版 TCG runner：generation 14 → 15，helper 返回 `kernel_ready`；runner 使用 `10.240.0.2/28`，guest 使用 `10.241.0.4/28`。硬件身份与安装设备 ID 的迁移前后摘要一致。旧 overlay 中的 setup/control 脚本已更新并核对摘要；新版 observe 会核验实际地址、唯一默认路由及内核 device ID 才返回 ready。
- 固定 GPT 内核已连接本地 SOCKS5 测试服务器，验证用户名密码认证、CONNECT 和 ATYP=3 域名目标 `chatgpt.com:443`。测试服务器在握手后拒绝转发，这是协议握手验收，不是使用真实 SOCKS5 出口完成 GPT 推理的证明。
- 随后用户明确要求清空全部业务数据，已删除上述测试实例、磁盘、网络、租约、OAuth 测试凭据，以及本部署的全部业务表数据；保留 schema 迁移历史、管理员登录和基础服务配置。账号、分组、密钥、代理、用量、账单、订单及普通用户计数均为 0；运行中及停止的 VM 容器、专属网络和实例 manifest 均为 0。
- 新版服务健康检查及管理员登录通过。清空设置后，原 Sub2API 管理员使用确认记录也已清空，管理接口返回原有 `ADMIN_COMPLIANCE_ACK_REQUIRED`，需用户在首次进入后台时重新确认。未代替用户提交确认。
- 当前验证为本机 TCG；本轮没有新增硬件 KVM 或清空后的真实 GPT 推理验收。可审查脱敏报告位于 `.dev/vm-kernel/verification/allocator-identity-20261022/`。
