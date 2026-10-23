# Docker 内核模式：实现边界与调用链路

日期：2026-10-23。当前分支已实现 Docker 模式替换 VM 模式；未执行线上部署或数据迁移。

普通模式仍默认。新建内核账号使用 `docker_kernel`，首版支持 OpenAI OAuth / GPT；账号管理、2FA、凭据刷新、分组、并发、调度、协议转换与计费复用现有 Sub2API。旧 VM 的 QEMU、guest OS、relay、模板构建与发布工作流已删除。

## 1. vm2api 的实际实现

分析对象为本地 vm2api（v1.3.206-11-g9566b4cf），并用 v1.3.206 标签核对关键运行位置；两者的以下关键行为一致。

- `src/lib/vm/runtime-kind.mjs`：运行类型为 docker 或 kvm，缺省 docker。
- `src/lib/vm/vm-runtime.mjs`：每个槽位建立一个 Docker 容器，绑定独立 home、run 目录、身份文件和网络，设置非 root 用户、只读根目录、资源上限。
- `src/lib/vm/slot-runtime.mjs`：统一启动槽位并确保推理内核就绪。
- Claude：容器内执行 `kin-kernel --gateway-worker --config /run/kin/kernel.json`。
- 本地 GPT：账号容器执行 `sleep infinity`；`codex-kernel-supervisor.mjs` 在管理服务所在进程环境中通过 `spawn` 启动 Codex 内核。它不是通过 `docker exec` 启动在账号容器里；管理服务本身可能运行在容器中。
- `codex-kernel-client.mjs`：通过账号独立 Unix socket 和内部 token 调用 `/internal/v1/codex/responses`。
- `codex-kernel-supervisor.mjs`：配置凭据路径、代理、持久 device_id；清除继承的代理环境变量，避免出现另一条出口。
- `egress.mjs`：透明出口以代理为单位组织 bridge 和转发；本地 GPT 使用内核配置中的代理。不能把其账号容器的 IP 或 MAC 等同于 GPT 请求实际使用的网络身份。

Docker 节省的是每账号完整 guest OS、QEMU、虚拟内存和系统服务开销。镜像大小、容器内存上限和实际 RSS 是不同指标；不能依据默认 512m 配置推断每账号实际占用。

## 2. 最小实现

一个 Docker 账号对应一个容器，容器主进程直接执行固定的 `kin-codex-kernel`。复用 `vmruntime` helper、`vmkernel.Client`、账号运行协调服务及 HTTP/SSE/WS 适配；保留这些共享包及配置名，避免另建业务链路。

- helper 为账号分配实例目录、持久 device_id、machine-id、MAC、独立 bridge 子网和地址。
- 固定镜像与 payload SHA256 在启动前校验，创建账号时无需安装操作系统或下载镜像。
- 入口读取现有 access 凭据和代理投影、设置代理出口白名单，再降权执行内核。容器不接收账号密码、TOTP 或 refresh token。
- 只读根目录；独立 home/run；凭据目录只读挂载。内核 socket 不发布 TCP 端口。
- helper 核对健康、配置版本、容器 ID、StartedAt 和内核进程启动标识。只有准备成功才可调度；容器退出应立即报错，不等待完整启动超时。
- 默认 512 MiB 是容器内存上限，不是预分配。并发跟随账号配置，不增加内核模式专属限流。
- 上游调用错误继续走原业务处理，不直接显示为容器异常。主服务重启接管健康容器，容器重启保持账号身份。

## 3. 完整请求路径

```text
客户端 HTTP / SSE / WS
 → Sub2API 原有鉴权、密钥、分组、额度、模型映射
 → 原有调度与账号并发
 → Docker 账号的就绪及版本检查
 → 现有协议转换 / WS 适配
 → vmkernel.Client → 账号 Unix socket
 → 容器内 kin-codex-kernel → 绑定代理 → OpenAI
 → 原路返回内容、事件、usage
 → 原有账单、日志与错误处理
```

请求入口不创建容器。凭据或代理变化沿用现有排空在途、更新配置和重新验证流程。WS 指现有客户端入口适配，不承诺新增上游原生 WS。唯一 device_id 不等于唯一 TLS 指纹，容器也不提供独立 Linux 内核。

## 4. 删除及兼容边界

- 删除旧 VM 运行实现、KVM/TCG 开关、虚拟磁盘与 guest 第二层网段、QEMU/guest 专属脚本和测试、旧设计文档。
- 保留普通模式、共享协议和业务代码、测试报告与历史验收记录。
- 不迁移旧 `vm_kernel` 数据，不自动停删旧线上实例，不把旧账号降级为普通模式。旧账号不能被新 Docker 调度器使用。
- 新 SQL 仅扩展 execution_mode CHECK，未执行 UPDATE。线上数据库尚未应用该结构变更。
- 新 helper 必须使用独立 root 与 `container-v1` 模板；不能指向旧 VM 状态目录。

## 5. 验证与部署

代码验收包括账号模式约束、HTTP/WS/续接/usage 回归、运行时全套单测及前端账号/2FA/状态测试、类型检查和生产构建。

真实运行验证使用独立 helper、临时容器和既有授权 GPT 凭据；验证直接内核请求与重启身份。不把直接内核成功等同于 Sub2API 公共接口、计费和并发验收。线上主程序尚未替换，公共接口 Docker 验收需要单独部署测试环境。

部署步骤与模板见 [deploy/docker-kernel/README.md](../deploy/docker-kernel/README.md)。

### 本轮真实运行结果

2026-10-23，ai 服务器独立测试 helper（未替换线上主程序）：

- 已缓存镜像后的账号冷启动：1.82 秒；Docker 实例就绪。
- 请求前容器内存：9.406 MiB（512 MiB 为上限，未测压测峰值）。
- gpt-6-luna：HTTP 200、response.completed，输出 DOCKER_OK。
- helper 重启接管通过；容器停止/启动后 device_id 保持，进程绑定标识改变。
- 测试结束后，按实例名称确认容器和独立网络均已删除，临时凭据状态目录已清理。
- 公共业务接口、2FA 新建 Docker 账号与并发压测尚未做线上验收。
