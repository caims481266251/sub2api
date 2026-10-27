# Docker 内核模式

普通模式保持原链路。新建 OpenAI OAuth 账号可选择 `docker_kernel`；2FA、凭据刷新、分组、并发、计费由现有 Sub2API 负责。KVM/TCG 创建入口、QEMU runner、guest OS 构建和 guest relay 已移除。

```text
客户端 → 原有鉴权/调度/并发 → 原有内核协议与 WS 适配
       → Unix socket → 账号 Docker 容器内 GPT 内核 → 绑定代理 → 上游
```

## 部署构件

1. 使用仓库根目录执行 `docker build -f deploy/docker-kernel/container/Dockerfile -t sub2api-docker-kernel:local deploy/docker-kernel`。
镜像必须使用 Docker 宿主的原生架构，Python/nftables 不得整体跨架构模拟；ARM64 主机需要为固定 amd64 GPT 内核提供 binfmt 执行支持。helper 会拒绝镜像与宿主架构不一致的配置。

2. 准备已授权的固定内核 `/opt/sub2api-docker/payload/usr/local/bin/kin-codex-kernel`，记录 SHA256。内核不随源代码分发。

连接复用验收：在 Linux 容器内，以内核的非 root 用户执行
`python3 codex-connection-reuse-contract.py --binary /opt/kin-payload/usr/local/bin/kin-codex-kernel`
（脚本位于 `deploy/docker-kernel/tests/`，也可通过 `docker exec -i` 的标准输入执行）。
它仅使用虚构凭据和 loopback 模拟上游，不读取或修改业务配置。先验证普通客户端
3 次请求复用 1 条连接，再要求内核同样复用；退出码 0 才表示通过，退出码 1
表示不满足契约或执行失败，详情见 JSON 的 `reason` / `error_class`。测试内核和
临时文件在退出时清理。它是候选内核的独立验收入口，不自动替换或重启线上内核。

2026-10-24 核验：摘要为
`0ffec538a57afbe66f13832513ee4c2a27ecc970faa9367c7cb8a13551196dd3`
的内核 3 次请求建立 3 条上游连接，契约未通过。同日 GitHub 发布接口返回的
vm2api 最新发布 v1.3.226，其 kin-codex-kernel 附件摘要仍相同。
上游 `scripts/fetch-payload.mjs` 明确将该构件标记为 closed-source；没有该内核
源码时，无法在本仓库把它改为账号级持久 HTTP 客户端。不能把修改宿主 UDS
keepalive 或代理桥保活当作完成这一修复。

3. helper 的模板必须为 `container-v1`，包含固定镜像 digest、payload_dir 和文件校验清单。禁止使用旧 KVM 模板。
4. 运行现有 `vm-runtime-helper` 可执行文件，指定新的独立 root 和私有 socket；此兼容名称不再包含虚拟机运行实现。主程序不挂载 Docker socket。
5. 主程序通过现有 `VM_KERNEL_*` 部署配置接入 helper（保留配置名，避免无关配置重构）。内存默认 512 MiB，是上限，不是预分配；不再使用虚拟磁盘参数。

模板示例（必须替换摘要）：

```json
{"default":{"runtime":"container-v1","image":"sha256:<image-id>","payload_dir":"/opt/sub2api-docker/payload","artifacts":{"/opt/sub2api-docker/payload/usr/local/bin/kin-codex-kernel":"<sha256>"}}}
```

helper 示例：

```sh
vm-runtime-helper --enabled --root /opt/sub2api-docker/instances \
  --socket /opt/sub2api-docker/run/helper.sock \
  --templates /opt/sub2api-docker/templates.json --service-uid 1000 --service-gid 20000
```

应用配置：`VM_KERNEL_ENABLED=true`、`VM_KERNEL_HELPER_SOCKET=/opt/sub2api-docker/run/helper.sock`、`VM_KERNEL_TEMPLATE=default`、`VM_KERNEL_MEMORY_MIB=512`。应用保持非 root 且加入专用服务组，并挂载 helper 的实例/socket 目录到相同绝对路径。

## 行为边界

- 容器帐号的业务推理、连接测试、质量检测和凭证守护探测
  通过绑定到该帐号的容器内核执行，共用内核准入与并发限制。内核不可用或执行失败时，
  这些推理请求不回退到主服务直连。
- 固定内核尚未提供的管理接口继续使用原有实现，包括模型目录、额度查询与重置、
  点数和邀请、OAuth 刷新与重新授权。管理接口与推理探测分别处理；使用同一个
  帐号代理不代表请求已经经过容器内核。
- Responses 用量探测恢复使用帐号 OAuth 凭据及配置代理直接发起 HTTP 请求，
  读取上游响应头中的 5 小时 / 7 天用量窗口；不依赖容器内核返回额度头。
  保留 Docker 帐号快照过期刷新和原有探测节流。
- 容器启动即执行常驻内核的生命周期保持不变。上游传输默认采用 vm2api 的
  prefer_websocket，启动默认值和每次请求保持一致；部署可设置
  VM_KERNEL_UPSTREAM_TRANSPORT=http，以每请求参数覆盖为 HTTP，无需逐请求重启内核。
  这不修改公开 WS 接口的准入开关，也不保证固定内核实际采用 WS 或跨请求复用上游连接。
- 主程序到内核的 UDS HTTP 连接按账号、socket、实例、代际、boot ID、内部令牌及
  凭据/代理版本隔离复用。身份变化淘汰旧池，但保留已经开始的请求；服务停止释放本地池。
  每池最多保留 32 条空闲连接，空闲超时 90 秒，最多保留 256 个池；这些仅限制缓存资源，
  不限制活动连接和账号业务并发。取消控制请求保持独立短连接，避免被业务流占用。
  已提交请求不因 Keep-Alive 失效而自动重放。
- 固定内核的 WebSocket 路径可能用通用 SSE 名称 event: response 包裹事件；
  适配层从 JSON type 读取具体事件，并按既有规则校验完成、用量和响应 ID。
  codex.response.metadata 与 response_headers 使用同一响应头白名单，保留
  turn-state 和限流头，不公开内部计时、私有元数据或认证信息。

- 首版开放 GPT；Claude 的协议转换代码仍被共享链路使用，不开放未经验收的 Claude Docker 账号创建。
- 内核是容器主进程；固定入口设置代理出口规则后降权并移除全部能力。每个账号独立目录、持久 device_id、bridge 子网和地址，只开放绑定代理出口。内核 socket 不发布 TCP 端口。
- SOCKS5 代理在容器内由无特权、本地异步桥接转发；它不实施业务并发、RPM 或冷却规则。桥接仅在上游 TCP/SOCKS 建隧道阶段对瞬时断连做最多三次连接尝试；认证拒绝和目标主动拒绝不重试。SOCKS CONNECT 返回一般、网络或主机不可达时，仅在未发送模型数据前重试。隧道建立后只透明双向转发，不缓存、不重放模型请求。诊断日志仅记录阶段、尝试次数与结果，不写代理地址和认证信息。
- 建连重试间隔为 200 ms、500 ms，附加最多 50 ms 抖动，仍最多尝试三次；调用方在等待 SOCKS CONNECT 时断开会取消建连及退避。TCP 保活使用 15 秒空闲、15 秒探测间隔、3 次失败，帮助回收失效连接；它不能修复上游 TLS 主动断连。双向转发保留背压和半关闭语义，读写错误只记录方向和错误类别。
- 内核一侧发送 EOF 后，代理桥允许反向响应继续排空；连续 30 秒没有成功交付尾部数据时回收该隧道，每次成功交付会续期。计时也覆盖阻塞写入，但不作用于内核侧仍开放的静默请求，不改变正常长推理期限。收尾不重放请求、不调整账号并发、不重启容器。
- 流读取失败或下游拒收时，主服务先关闭原内核执行流，再尝试独立取消通知。取消接口返回 404/405/501 记为 `unavailable`，不能当成取消确认或原请求结束；仍支持提供该接口的内核，不猜测替代路由、不根据一次 404 永久缓存能力。
- 固定 Codex 内核目前对 SOCKS 拒绝和 TLS 断开均返回 `upstream_transport`，没有机器可读的“业务请求未发送”证明。桥接不解析或重放 TLS；主服务不能根据该错误、日志中的 `sent=false` 或尚无客户端输出来自动重试模型请求。必须先扩展并验证内核错误协议，才能启用 TLS 失败后的安全重试。可用 `tests/codex-socks-contract.py --failure socks|tls` 对固定二进制进行无真实凭据、无外部转发的故障验证。
- 主服务重启只检查并接管健康容器。凭据更新等待在途完成后应用；socket 健康与配置版本一致后才能重新接收请求。上游报错不直接变成容器故障。
- 不迁移旧 VM 账号，不接管、停止或删除旧实例。旧模式拒绝新建；旧记录不自动降级到普通模式。数据库变更仅扩展 execution_mode CHECK，保留所有行。
- 默认 helper 状态目录与旧部署不同。未获得迁移指令前不得指向旧 VM root。
- 内存和启动速度由实测说明，不能用内存上限或镜像体积代替实际占用。Docker 不提供独立宿主内核或独有 TLS 指纹。

## 容器独立 User-Agent

OpenAI Docker 帐号在 helper 首次准备时获得一个持久桌面 UA，保存在
`manifest.json` 的 `client_identity.user_agent` 与 `user_agent_template`。
当前可分配池包含 **1,113** 个不同组合：7 个明确版本的 Codex CLI
（0.159.0 至 0.164.0，含 0.162.1）×
（78 个 macOS 版本 × arm64/x86_64 + 3 个 Windows build × x86_64）。
格式例如 `codex_cli_rs/0.164.0 (Mac OS 15.1; arm64)` 或
`codex_cli_rs/0.164.0 (Windows 10.0.26100; x86_64)`；不附加账号 ID、
邮箱、UUID、哈希或 Sub2API 标记。版本表是兼容性配置，不能删除已有条目或改写其 UA。

- 同一个 helper 状态目录内强制一容器一 UA。分配在全局锁内检查并原子持久化；
  helper 的独占目录锁阻止第二个进程同时分配。容器重建、主程序/helper 重启、
  凭据轮换及代理更换均复用原分配。帐号删除且 helper 确认容器移除后，
  UA 归还池中供新帐号复用；删除失败、结果未知或尚未完成时继续占用。
  墓碑保留历史身份，但确认删除后的重复删除/检查不会重新占用已归还 UA。
- 不同服务器/独立 helper 根目录没有共享分配中心，因此不保证跨服务器不重复。
  本实现最多支持 1,113 个未释放的独立分配；用尽明确返回
  `client_user_agent_pool_exhausted`，不会退回重复 UA。新增容量必须扩展明确模板。
- 新帐号及迁移优先选择最新的兼容客户端版本；已有兼容身份保持稳定。
- 启动配置与每次 Responses 调用均使用同一 UA。调用方无法覆盖 UA 或安装 ID；
  安装 ID 来自原有持久 `device_id`，不改变会话、线程、请求 ID 的语义。
- 旧容器通过既有排空/应用配置流程补齐 UA；0.150.0–0.158.0 的历史身份
  也在请求排空后迁移到兼容池，优先保留原 OS、版本和架构，保持设备 ID 不变。
  历史模板仍可校验，但不再分配；兼容身份保持稳定，不因每次请求而轮换。
  迁移前池耗尽或代理解析失败时，仅在重新确认原健康绑定和帐号状态后退避，
  保留原本可用的模型；不清除模型级冷却，也不自动重放已发送的业务请求。
  主程序、helper 和容器镜像需配套更新；仅修改源码不会更改现有运行容器。
- Mac/Windows 表示模拟的客户端配置，不改变容器的真实 Linux 系统，也不证明
  TLS 指纹等同真实 Mac/Windows；内核不支持的管理接口仍遵循既有行为边界。

## 验证

后端：vmruntime 全套单元测试、账号执行模式、内核 HTTP/WS/续接/计费回归。前端：账号创建、2FA、编辑、状态与类型检查。真实验证应使用独立测试 helper 和独立实例，不变更现有线上账号。

## 状态边界

Docker 标签只反映本地容器进程和内核 socket 健康。请求准入仍独立核对凭据、代理及原有账号规则；代理被禁用、过期或 OAuth 刷新失败，不会停止健康容器。代理配置在停止旧进程之前完成校验与地址解析。隐私设置失败和上游 401/429/5xx 继续由原业务模块处理，不写入 Docker 健康状态。

已发送但无法确认结束的本地内核请求禁止自动重放，但不隔离整个账号或重启容器。取消接口的响应只记录为控制请求确认，不能据此宣称原请求已结束。容器恢复由独立健康检查、配置变化或实例绑定变化驱动；单次模型请求错误不触发恢复。响应头超时复用原有 gateway 配置，OpenAI 默认不额外设置 30 秒超时。

## 配套在线更新（2.15.4 起）

每个 tag 的 `docker-kernel-tools-VERSION-linux-ARCH.tar.gz` 包含同版本 helper、原生架构 Docker 镜像归档、版本/摘要清单、模板格式说明和宿主更新代理。私有模型 payload 继续使用服务器上已校验的文件，不公开上传。

首次升级旧部署需要 root 安装 `host/install-update-agent.py`，指定已有 helper socket、独立 Docker 实例目录、模板和服务 UID/GID。只更新主程序不能替代这一次引导安装。

后续后台更新先校验主程序校验和，再由固定仓库的本机更新代理预装同 tag 运行包；任一文件缺失、损坏或版本错误都会在替换主程序前失败。后台显示需重启后，重启的新程序在监听业务端口之前激活运行包。激活必须获得旧进程持有的协调锁，之后重建属于本实例目录的账号容器，保留身份、网络租约、凭据和用户业务数据。helper 健康和版本验证失败会恢复旧 helper 配置，主程序停止启动并显示具体错误；可选择上一完整版本回退，不会宣称更新成功或接受错误环境的请求。

发布流程先创建 draft，上传所有运行包后再公开 Release，避免后台提前看到只有主程序的版本。首次从 VM 切换 Docker 不迁移旧 VM 实例。
