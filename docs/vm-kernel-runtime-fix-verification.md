# VM 运行时启动修复与实测记录

> 历史阶段记录：本文下列运行结果和阶段限制仅适用于记录时源码，不是本次业务复用修复的验收结论。固定并发 1、禁止 2FA 初始化、仅连接内历史等旧限制已调整；当前边界见 [业务复用修订说明](vm-kernel-business-reuse-20261022.md) 与 [实施状态](vm-kernel-implementation-status.md)。硬件 KVM 仍未验收。

本文保留第一阶段空Claude guest实测记录。后续Codex启动、服务配置生命周期、规范凭据和双内部鉴权修复，以及最终镜像上私有Go client的真实推理与清理结果见 [Codex guest 实测](vm-kernel-codex-guest-verification.md)；下文“本轮”指第一阶段。

日期：2026-10-22。实现分支：`codex/vm-kernel-account-mode`，基线仍为纯上游 sub2-api v2.15.0。参考源码固定为 vm2api v1.3.206 / `894a78daba0361ecf825e5ec4a0707108cdbae9b`。

本轮已修复并实测独立 guest 的启动与内核健康链路。修复代码位于 `deploy/vm-kernel/upstream/`，未改原 vm2api 工作区或既有业务容器。**新运行包尚未接入 Sub2API 的 Go helper；真实账号推理、代理约束、WS、跨协议和计费验收仍未完成。** 此结论与 [原失败实验](vm2api-local-kvm-experiment.md) 分开保存，不用新结果覆盖旧证据。

## 问题、修复与因果证据

| 问题 | 已落实的修复 | 现场证据与限制 |
| --- | --- | --- |
| ARM64 Docker 执行端运行 amd64 QEMU；原 guest 等待 BOOT/UEFI 设备超时，进入 emergency | `build-native.py` 按 Docker **服务端**架构构建 QEMU 和工具，忽略继承的错误默认 platform；guest、固件和 payload 仍为 x86_64 | 同盘、同资源、相同 QEMU/OVMF 包版本，换原生 ARM64 QEMU 后约 61 秒完成 cloud-init，SSH 可用，全部目标挂载成功。原 QEMU ELF machine=62，原生为183。支持额外模拟层导致启动问题的判断；没有获得失败启动的完整 udev journal，不声称已定位每个设备超时的内部原因 |
| guest 能启动，但 CLI 连续 `SIGILL`，未创建真实 guest kernel socket | 将精确 `qemu64` 定义为固定 payload 的兼容别名：TCG 映射 `max`、KVM 映射 `host`；TCG 的 `host` 同样映射 `max`，其他显式型号保持 | native QEMU + qemu64 时 journal 出现 CLI signal 4，dmesg 为 invalid opcode；仅换 CPU=max 后内核 HTTP 200、healthy=true、ready_slots=20、cli_restarts=0。未反汇编，不声称具体哪条指令是最低要求；KVM/host 分支仅完成行为测试 |
| `kin-guest-exec` 用 `if ! run_ssh; then st=$?` 读到取反后的状态，SSH 失败可能变成成功 | 用普通 if/else 立即保存原退出码；普通远端失败原样返回；SSH 255、timeout 124 按既有约定映射为1 | 5 项测试覆盖成功、普通非零、SSH、timeout、交互及 detach；真实 guest 执行 exit 0/7 时外层分别返回0/7 |
| 上游 overlay 在 base ID 或容量变化时可能自动重建已有盘 | 原盘存在时校验元数据与实例身份；不匹配、状态缺失、符号链接时拒绝；保留已有盘和状态，要求显式恢复 | 7 项测试含10个符号链接子场景，逐字节检查保留；真实重建 runner 后磁盘、NVRAM 与身份保持 |

CPU 规则不区分默认 `qemu64` 和用户显式配置的同名字符串，因为上游环境变量没有这一信息。日志明确输出 requested、effective、accel、reason；没有启动失败后换 CPU 或加速器重试。

## 最终镜像和真实运行结果

执行端为本地 Colima Linux ARM64，无 `/dev/kvm`。测试资源为2 vCPU、2 GiB guest 内存、20 GiB sparse disk。测试账号是独立空 Anthropic 槽，没有导入凭据或激活调度。原有两个普通账号保持运行。

最终验证镜像为 `sub2-vm-runner:20261022-arm64-fixed`，本地 image ID：

```text
sha256:b82064cc29a6a7ce422148f723ac59a03bd964075a056d1077e98e0d4eb4413b
```

这是本地内容寻址的 image ID，不是已发布的 registry manifest digest。最终镜像在已完成构建的原生 runner 上叠加 CPU 补丁及来源文档，避免重复拉取大镜像；完整 `upstream/Dockerfile` 也包含全部最终源文件。实测容器中的入口、exec、README、source.json 哈希与工作区逐项匹配。

固定材料：

- 上游 guest image：`ghcr.io/dofastted/kin-kvm-ubuntu@sha256:2c12ba55e5df2c677972d235d88be5200e9dcb778fb27af7eeed48f7b93fdde7`。
- Guest base ID：`04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94`。
- QEMU：`1:10.0.13+ds-0+deb13u1`；OVMF：`2025.02-8+deb13u1`。
- 原始脚本来源目录：`docker/kin-kvm/`；原始哈希及本地补丁说明见 `upstream/source.json`，许可证随镜像保存。原始哈希不用于校验修改后的文件。

Debian 与 guest image 按 digest 固定，QEMU/OVMF 固定包版本。APT 源和其他依赖未做 snapshot 固定，构建不是位级可复现，也不能保证旧包离开仓库后仍可重建。

最终容器于 `2026-10-22T06:23:55Z` 启动，环境仍传入 `KIN_KVM_CPU_MODEL=qemu64`；日志确认 `effective=max accel=tcg`。`06:25:10Z` 观测：

| 项目 | 实际结果 |
| --- | --- |
| Guest 架构 | x86_64，独立 Linux guest |
| 根分区、BOOT、UEFI | `/dev/vda1 → /`、`/dev/vda16 → /boot`、`/dev/vda15 → /boot/efi` 全部成功挂载 |
| 共享目录 | home、run、payload 的9p挂载可见 |
| Guest 内核 socket | `/run/kin-guest/kernel.sock` 存在；同时取得该服务的真实 HTTP 响应 |
| 原 vm2api client 健康调用 | HTTP 200、reachable=true、healthy=true、20 ready slots、0 CLI restarts |
| 原始健康业务字段 | `ok=false`、`credential_state=missing`；凭据文件确实不存在 |
| 代理健康字段 | `proxy_configured=false`、`proxy_required=false`；本次不提供强制代理成功证据 |
| 非法指令 | 当前 guest boot 的 dmesg 中 invalid opcode/SIGILL 事件为0 |
| 实际加速 | QMP query-kvm：enabled=false、present=false；属于TCG，不能写作硬件KVM通过 |
| 重建 runner 后身份 | machine-id、SMBIOS UUID、instance-id、SSH 公钥摘要保持；磁盘和NVRAM inode保持；guest boot ID变化 |

20 ready slots 是固定上游 payload 的报告，**不改变 Sub2API VM 账号并发固定1的边界**。空槽进程健康与账号授权是两项独立结果，不能把 `ok=false` 改成成功来绕过准入。

## 代码验证

4组针对性测试共20项通过：guest-exec 5项、overlay 7项、CPU选择3项、native builder 5项。测试实际执行生产 Bash 函数或脚本，并以受控 SSH、timeout、qemu-img、Docker 命令替身验证输入输出；它们不是硬件KVM测试。Shell语法、补丁空白及镜像文件哈希检查通过。真实 guest 另完成健康、挂载、重启身份与远端退出码验证。

本轮未改 Go/frontend 业务代码，没有运行或新增 Go 构建缓存。既有 HTTP/WS fixture、前端和后端测试成绩见 [实现状态](vm-kernel-implementation-status.md)，不把它们重复计入本轮真实 guest 结果。

本地脱敏证据保留在 `.dev/vm-kernel/verification/local-vm2api-kvm-fix-20261022/`：最终镜像与补丁哈希 `final-image.json`、最终启动 `final-runtime-switch.json`、健康与 guest/QMP `final-runtime-observation.json`、身份和退出码 `restart-and-exit-verification.json`、console与清理记录。该目录是本地证据，不是编译缓存。

## 接入边界与下一步

当前 Go helper 仍调用旧自建 runner 合同；新包所需的环境变量、挂载、SSH身份、probe/observe/apply 和凭据路径均不同。只改镜像名无法工作。最小接入改动及前置证据见 [上游 runner 适配合同](vm-kernel-upstream-adapter-contract.md)。

接下来的实施顺序是：匹配平台的真实 native 请求与凭据/代理合同 → 窄运行时适配及 actual accelerator 观测 → 接入原 helper 生命周期与 revision → 两平台 HTTP/跨协议/WS、取消和原计费全矩阵。账号管理、原调度、授权刷新和 RecordUsage 继续由 Sub2API 持有，不引入 vm2api 的业务管理系统。

当前已完成的是启动故障修复与空 guest 的真实内核健康验证；硬件 KVM、Codex guest、真实凭据消费及更新、代理失败禁止直连、真实 HTTP/WS usage 和计费均未在本轮验证。原 helper 的生产KVM、凭据与身份闸门保持有效。

## 清理

已通过原面板 API 停止并删除测试账号；核对当前及两个对照容器均停止/PID=0后删除。宿主进程检查确认本轮QEMU为0，VM目录、账号记录和测试代理绑定均已移除，`vm.allow_tcg=false`。原 `vm2api-local`、`kin-02`、`kin-03` 和现有Sub2API容器的启动时间保持，两个应用 `/health` 均为HTTP 200 / ok。

删除已确认属于本轮且无容器引用的旧runner镜像与临时tools标签，保留最终镜像和一个已验证的前序镜像供复查/回滚。最终构建上下文及Python测试临时目录已清理；没有Go缓存或Testcontainers会话。未执行全局Docker清理，保留证据日志及业务数据。收尾空间：Mac约108.29 GiB，Docker执行端约1.65 GiB。

原Sub2API工作区的未提交diff哈希仍为 `36d69013ecf3c3c433c1cf6c5c8afe3ff3356cc690a1ba8cf4c370877ad7f583`；原vm2api工作区保持干净。本轮修复和文档均留在独立功能分支，未提交或推送。
