# 本地 vm2api KVM 流程实验

日期：2026-10-22。所有事件时间使用 UTC。任务：先操作本地 vm2api 的实际创建与启动流程，再调整 Sub2API 集成方案；本轮不修改业务实现。

**最终结果：已完成预检、创建、启动观察、停止、删除及独立清理核验。严格硬件 KVM 因缺少 `/dev/kvm` 拒绝创建；显式 TCG 路径创建并启动了独立 Linux guest，但 guest 等待 BOOT/UEFI 设备超时进入 emergency mode，SSH 与推理内核未就绪。本轮没有真实推理成功或 KVM 硬件加速成功的证据。**

## 实验对象与范围

- 控制面：本地既有 `vm2api-local`，镜像 `vm2api:1.3.206-local-style-restoration-20261021`。这是包含既有本地改动的部署，不冒充纯上游 tag；创建路由、VM runtime 和 KVM probe 的文件 SHA256 与当前本地源码相同。
- 固定参考：vm2api `v1.3.206`，提交 `894a78daba0361ecf825e5ec4a0707108cdbae9b`。
- Docker context：`colima-orderpilot-security-verify`；实际 Docker 执行端 Linux aarch64、4 CPU、约 6 GiB RAM，未提供 `/dev/kvm`。
- 控制面已配置 `DOCKER_DEFAULT_PLATFORM=linux/amd64`。实际启动使用 amd64 runner；不能仅凭 daemon 为 arm64 推断镜像选择必然失败。
- 独立测试槽：`vm-kvm-poc-20261022`，容器 `kin-kvm-poc-20261022`。Ubuntu 24.04、2 vCPU、2 GiB guest RAM、20 GiB 稀疏虚拟磁盘。
- 测试槽使用既有 `px-local` 出口，不激活为默认账号，不导入任何真实账号，始终不可调度。原有 `vm-02`、`vm-03` 不作为本次创建/删除对象。
- 面板登录使用部署已有配置，凭据和会话仅在测试进程内存中使用，每次调用后注销；报告不记录密码、cookie、token、私钥或凭据正文。

## 已执行的真实流程

| 时间 | 操作 | 实际结果 | 证据含义 |
| --- | --- | --- | --- |
| 05:07:16 | `GET /api/panel/vms/create-options`，保持 `allow_tcg=false` | HTTP 200，`kvm.ok=false`，错误为 Docker 映射 `/dev/kvm` 时设备不存在 | 严格硬件 KVM 条件不满足 |
| 05:07:16 | `POST /api/panel/vms/create`，`runtime_type=kvm`、`start=false` | HTTP 409，`kvm_unavailable`；未创建实例 | 停机创建也执行能力检查，不能绕过硬件要求 |
| 05:08:51 | 通过原面板 API 临时设置 `vm.allow_tcg=true` | 预检变为 `kvm.ok=true, accel=tcg` | 上游原生兼容路径实际可用 |
| 05:08:51–05:11:31 | 原 `POST /api/panel/vms/create`，`start=true` | 应用自行拉取官方 runner、准备磁盘/seed/身份并启动容器；HTTP 200，状态 running，`schedulable=false/no_credential` | 无需自建离线模板即可进入原项目 VM 启动链；running 尚不等于 guest ready |
| 05:11:31 | finally 恢复 `vm.allow_tcg=false` 并复查 | 原设置已恢复；严格预检再次报告设备不存在 | 临时兼容开关未作为持久业务配置留下 |
| 05:12:53 | 检查实际 runner | 标签 `kin.vm.runtime=kvm`；环境 `KIN_KVM_ACCEL=tcg`；无 KVM 设备映射 | runtime 形态与实际加速方式是不同字段 |
| 启动后 | 直接查询该实例的 QMP | `query-status: running=true`；`query-kvm: enabled=false, present=true` | QEMU 已运行，KVM 编译支持存在但硬件加速未启用 |
| 05:14:26 起 | guest console | Ubuntu 24.04.5 LTS，Linux `6.8.0-147-generic`，x86-64，识别 virtualization=qemu | 已启动独立 guest 系统内核，不是普通 Docker slot |
| 05:14:54 | 原 `test-chat` 接口，16 token 上限 | HTTP 200 外层包裹，业务结果 `ok=false, no_credential`，0 ms | 空槽没有真实凭据；没有实际模型调用成功证据，也没有据此制造计费样本 |
| 观察期间 | 原 guest SSH helper 与现有内核健康调用路径 | SSH 尚未 ready；健康调用报 `slot worker timeout`，未取得内核 ready 证据 | 超时不能证明 guest 推理内核已收到健康请求 |
| 约 05:18:45 | guest 启动约 7 分钟后 | 等待 BOOT/UEFI 设备超时，`boot.mount`、`boot-efi.mount`、`local-fs.target` 失败，进入 emergency mode | 明确失败在 guest 系统启动阶段，尚未到推理内核 |
| 05:21:23 | 原 `POST /api/panel/vms/:id/stop` | HTTP 200；独立 inspect 为 exited、running=false、PID=0 | 测试 guest 已停止 |
| 停机后 | 对基础 qcow2 与测试 overlay 做只读扇区检查 | 两者均读到 GPT 头和分区条目；15 号分区 FAT32 标签 `UEFI`，16 号分区 ext 标签 `BOOT` | 排除这两份磁盘中标签缺失；尚不能解释 guest 设备别名为何未及时就绪 |
| 05:33:53 | 原 `DELETE /api/panel/vms/:id` | HTTP 200，删除测试槽 | 生命周期删除操作完成；仍需独立核验资源 |
| 05:34:27 | Docker、进程、文件系统、面板与 health 交叉核验 | 测试容器/目录/记录/观察进程与 QEMU 均不存在；出口绑定恢复；原业务服务继续运行 | 已核验实际清理，未依赖 DELETE 外层成功判断 |

内部健康请求返回 `ok=false, status=0, error="slot worker timeout after 2000ms"`。无凭据 test-chat 的失败发生在业务入口，不代表已经请求 guest 或上游模型。

## 实际机制与本轮停在哪一层

固定源码的创建链是：面板 `create-options/create` → `probeLocalKvm` 选实际加速方式 → 创建账号附属记录、机器身份及目录 → `startSlotReady`/VM runtime 准备 runner → runner 创建 qcow2 overlay、OVMF 状态、SSH key、cloud-init seed → QEMU 启动 guest。

guest 正常启动后的预期链是：cloud-init 安装服务 → `kin-guest-setup` 挂载 9p 共享并准备 payload/配置 → `kin-kernel.service` 启动内核 → guest relay 监听 7070 → QEMU 转发至 runner → socat 提供控制面可访问的 `kernel.sock`。请求再由该 socket 进入 guest 内核；socket 由 runner 提前建立，所以它存在不代表 guest 内核已监听。

本轮实测走到了 **QEMU → 独立 Linux → local-fs 启动失败**。之后的 cloud-init 安装、9p、kernel health 和推理链仍是源码描述与待验证项，不能记为实测成功。

源码入口：[创建接口](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/admin/panel-routes.mjs)、[VM runtime](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/vm/vm-runtime.mjs)、[KVM 能力探测](https://github.com/dofastted/vm2api/blob/v1.3.206/src/lib/vm/kvm-host.mjs)、[runner](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/kin-kvm-entry)、[guest 安装](https://github.com/dofastted/vm2api/blob/v1.3.206/docker/kin-kvm/guest/install-guest.sh)。

本地原有 `vm-02`、`vm-03` 实际是 Docker slot，本轮没有把其“可用”当作 KVM 通过的证据。`runtime.type=kvm` 描述 VM 运行形态，`accel=kvm/tcg` 描述加速方式，`kernel-ready` 描述服务状态；三者需要分别判断。

## Guest 启动失败的证据边界

串口日志确认根分区 `vda1` 已挂载，随后 systemd 等待 `/dev/disk/by-label/BOOT` 与 `/dev/disk/by-label/UEFI` 超时，导致 `/boot`、`/boot/efi` 和本地文件系统目标失败。读取停机磁盘得到：

| 项目 | 基础镜像 | 本轮测试 overlay |
| --- | --- | --- |
| 虚拟容量 | 3.5 GiB | 20 GiB |
| backing | 无 | `/opt/kin-kvm/base.qcow2` |
| 分区 1 | ext，`cloudimg-rootfs` | 相同 |
| 分区 15 | FAT32，`UEFI` | 相同 |
| 分区 16 | ext，`BOOT` | 相同 |

读取使用已停止测试实例的只读挂载与 `qemu-io -r`，未挂载文件系统、未修复或写入磁盘。检查了 GPT 签名并解析分区条目，未做 CRC、备用 GPT 一致性或完整文件系统校验。最初使用 `qemu-img dd` 的诊断脚本未正确取得目标扇区，其 `GPT header missing` 是诊断工具失败；改用有偏移/长度校验的读取后已确认 GPT 与标签存在，不能据此声称 guest 分区表损坏。

目前仍未取得 guest `/etc/fstab` 和完整 journal，尚无法确定是 udev 事件/别名生成、设备探测、启动时序还是其他原因。宿主为 arm64、runner/guest 为 amd64，并使用 TCG，这是实验环境事实，不能直接推断为根因；也不能仅靠延长超时或跳过挂载就宣布修复。下一次最小复现应沿设备事件与挂载依赖定位。

## 运行物料

此次应用实际取得的 runner：

- 引用：`ghcr.io/dofastted/kin-kvm-ubuntu:24.04`。
- RepoDigest：`ghcr.io/dofastted/kin-kvm-ubuntu@sha256:025256ec41e32bf037dcad27f833905972c5f85fcf280d9de8fb96d707d48068`。
- amd64 平台 manifest：`sha256:2c12ba55e5df2c677972d235d88be5200e9dcb778fb27af7eeed48f7b93fdde7`。
- image ID：`sha256:09f5aa530e7b3fb9eb4c37bfa84101aa21ffe6081a235bea6ea108da03d5b986`；Docker 报告 Size `905288140` bytes。
- runner、guest-exec、guest-setup、install-guest 的实际 SHA256 已记录到 `runner-materials.json`，并核对与固定参考脚本相符。

Colima 内既有的 `kin-kernel`、`kin-codex-kernel`、`cli-node`、glibc loader/library 与 `v1.3.206/deploy/payload.json` 匹配，payload release 为 `payload-2026.10.19-kernel-cli`。本轮复用这些文件，没有重新下载推理 payload。

## 已发现、必须影响移植方案的问题

1. **不能只按容器 running 或 socket 存在放行账号。** 创建 API 已成功时，guest 仍在开机；runner 会先建立宿主 relay socket。Sub2API 需要分别观察 runner、guest、内核和账号准入。
2. **上游 guest-exec 会把部分 SSH 错误报告为退出 0。** 本轮实际出现 stderr `Connection timed out during banner exchange` 与 exit code 0。固定脚本使用 `if ! run_ssh ...; then st=$?`，取到逻辑取反后的状态。复用时应局部修正并检查返回内容，不能把 exit 0 当作 guest 命令完成证据。
3. **硬件 KVM 和软件模拟应分别记录。** 相同 KVM runtime 形态可以使用 TCG；本轮 QMP 明确证明 KVM disabled。TCG 生命周期证据不能替代 KVM 性能验收。
4. **原项目已实测能自动准备物料并启动 QEMU/独立 Linux。** Sub2API 应优先复用 runner、guest 脚本和固定 payload；先解决本次启动失败、验证后续内核链，再接账号和调度，收缩自建 QEMU/cloud-init 启动器。这里不把“guest 开始启动”表述成“完整运行链可用”。
5. **原项目运行物料仍需固定版本。** 可变镜像 tag 会影响复现，应固定平台 manifest digest、payload hash 和脚本 hash，不能仅写版本名。
6. **本次本地出口实验不证明强制代理。** 测试用 `px-local`；Sub2API 的代理失败禁止直连约束需要另做真实验证。guest 首次安装还可能访问包源，复用时需处理初始化依赖与代理策略的关系。
7. **磁盘检查必须看 Docker 实际执行端。** Mac 尚有约 107 GiB，Colima 却仅余约 1.8 GiB。只检查 Mac 磁盘不足以保证 VM 能启动。
8. **不能照搬基础镜像变化就重建 overlay 的行为。** 原 runner 对 base ID/磁盘容量变化会重建 overlay；Sub2API 必须保留已创建账号的磁盘和稳定身份，变化应拒绝并报告明确原因或走受控升级，不能自动丢弃实例数据。

## 证据文件与已完成收尾

脱敏证据保存在 `.dev/vm-kernel/verification/local-vm2api-kvm-20261022/`：

- `panel-calls.jsonl`：真实面板请求、HTTP 与业务状态。
- `runtime-observations.jsonl`、`runner-materials.json`、`runner-start.log`：运行方式、镜像和启动记录。
- `guest-watch.jsonl`：有界 guest console / SSH 观察。
- `guest-console.log`、`guest-final.json`：最终 guest 日志及失败状态。
- `disk-labels.jsonl`、`disk_labels.py`：基础盘与 overlay 的只读分区、类型和标签核验。
- `cleanup-final.json`：独立容器、进程、目录、业务运行和磁盘空间核验。
- `image-cleanup.json`、`dangling-image-cleanup.json`：明确归属 vm2api、无容器引用的旧编译镜像清理记录；保留当前版本、一个回滚版本以及仍有容器引用的镜像。
- `panel_probe.py`、`tcg_cycle.py`、`watch_guest.py`：不含凭据的复现实验工具。TCG 开关由 finally 恢复；这些脚本不是产品运行组件。

05:34:27 的最终核验确认：

- `kin-kvm-poc-20261022` 与临时只读诊断容器均已不存在；Docker 执行端没有 `qemu-system` 进程；本轮观察 PID 已退出。
- 测试槽目录与 JSON 均不存在，`px-local.bound_vm_ids` 恢复为 `vm-02`、`vm-03`。
- `vm.allow_tcg=false`，默认 runtime 仍为 docker；严格 KVM 预检再次返回设备不存在。
- vm2api 控制面、原有两个 slot、Sub2API/Redis/PostgreSQL 均继续运行，启动时间未被本轮改变；控制面 `/health` 为 HTTP 200。服务 health 不代替 provider inference。
- macOS 可用 `112133104 KiB`（约 106.9 GiB），Colima/Docker 可用 `1746808 KiB`（约 1.67 GiB）。20 GiB 是测试盘虚拟容量，不等于实际占用。

没有运行 Go 构建或测试，未创建 Go 缓存。按项目归属和容器引用核对后，清理了旧 vm2api 镜像标签及明确归属的未引用构建镜像，保留当前运行版本、必要回滚版本和仍有容器引用的镜像。此次官方 runner 保留为下一轮可复用的单份依赖（Docker Size 约 905 MB）；已删除无凭据测试实例磁盘。未执行全局 Docker prune，未删除共享数据卷、数据库、其他槽位或备份。

本轮只新增/调整实验与方案文档和本地脱敏证据。vm2api 源码仍干净；原 Sub2API 工作区三份未提交文件的合并 diff SHA256 仍为 `36d69013ecf3c3c433c1cf6c5c8afe3ff3356cc690a1ba8cf4c370877ad7f583`。功能分支既有未提交实现保留，未提交或推送代码。

后续方案见 [VM 运行时调整计划](vm-kernel-runtime-adjustment-plan.md)。
