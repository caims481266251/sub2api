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

- 首版开放 GPT；Claude 的协议转换代码仍被共享链路使用，不开放未经验收的 Claude Docker 账号创建。
- 内核是容器主进程；固定入口设置代理出口规则后降权并移除全部能力。每个账号独立目录、持久 device_id、bridge 子网和地址，只开放绑定代理出口。内核 socket 不发布 TCP 端口。
- SOCKS5 代理在容器内由无特权、本地异步桥接转发；它不实施业务并发、RPM 或冷却规则。桥接仅在上游 TCP/SOCKS 建隧道阶段对瞬时断连做最多三次连接尝试；认证拒绝和目标主动拒绝不重试。SOCKS CONNECT 返回一般、网络或主机不可达时，仅在未发送模型数据前重试。隧道建立后只透明双向转发，不缓存、不重放模型请求。诊断日志仅记录阶段、尝试次数与结果，不写代理地址和认证信息。
- 主服务重启只检查并接管健康容器。凭据更新等待在途完成后应用；socket 健康与配置版本一致后才能重新接收请求。上游报错不直接变成容器故障。
- 不迁移旧 VM 账号，不接管、停止或删除旧实例。旧模式拒绝新建；旧记录不自动降级到普通模式。数据库变更仅扩展 execution_mode CHECK，保留所有行。
- 默认 helper 状态目录与旧部署不同。未获得迁移指令前不得指向旧 VM root。
- 内存和启动速度由实测说明，不能用内存上限或镜像体积代替实际占用。Docker 不提供独立宿主内核或独有 TLS 指纹。

## 验证

后端：vmruntime 全套单元测试、账号执行模式、内核 HTTP/WS/续接/计费回归。前端：账号创建、2FA、编辑、状态与类型检查。真实验证应使用独立测试 helper 和独立实例，不变更现有线上账号。

## 状态边界

Docker 标签只反映本地容器进程和内核 socket 健康。请求准入仍独立核对凭据、代理及原有账号规则；代理被禁用、过期或 OAuth 刷新失败，不会停止健康容器。代理配置在停止旧进程之前完成校验与地址解析。隐私设置失败和上游 401/429/5xx 继续由原业务模块处理，不写入 Docker 健康状态。

已发送但无法确认结束的本地内核请求仍保留防重复执行保护；它不被显示成容器故障。实际本地健康检查失败才影响健康标签。
