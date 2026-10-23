# Codex guest 实机验证记录（2026-10-22）

> 历史阶段记录：本文下列运行结果和阶段限制仅适用于记录时源码，不是本次业务复用修复的验收结论。固定并发 1、禁止 2FA 初始化、仅连接内历史等旧限制已调整；当前边界见 [业务复用修订说明](vm-kernel-business-reuse-20261022.md) 与 [实施状态](vm-kernel-implementation-status.md)。硬件 KVM 仍未验收。

本轮已在 **ARM64 原生 runner + x86_64 Linux guest + TCG** 中运行固定
Codex 内核，并完成真实 `gpt-6-luna` Responses 请求。此结论不代表 KVM
硬件加速通过，也不代表 sub2-api 完整账号调度、helper、计费和 WebSocket
链路已经通过实机验收。

证据目录：`.dev/vm-kernel/verification/codex-guest-native-20261022/`。
该目录是本地验证记录，不保证随仓库发布。本文仅引用脱敏结果文件；不包含
邮箱、登录密码、MFA secret、OAuth token、内部认证值或代理凭据。

## 登录与凭据归属

`login-events.jsonl` 记录第一次受控登录失败为 `service_rejected`；该错误
本身不能说明密码错误。后续 diagnostic retry 返回 `ok=true`，凭据导入
返回 HTTP 200、`ok=true`。登录材料未作为自动重登配置保存；本轮没有据此
开启第二套自动刷新器。账号服务负责 OAuth 获取与提交，guest 只消费投影。

登录成功与模型推理成功分开判断；后者由下面的真实终端事件和用量确认。

## Guest 执行位置与生命周期

`restart-fix-guest.json` 记录：

- guest 架构为 `x86_64`，具有独立 boot ID；QMP `query-status` 为 running。
- QMP `query-kvm` 的 `enabled=false`、`present=false`，因此实际是 TCG。
- guest PID 2579 的 executable 为
  `/opt/kin-payload/usr/local/bin/kin-codex-kernel`，ELF machine 为 62。
- executable SHA256 为
  `0ffec538a57afbe66f13832513ee4c2a27ecc970faa9367c7cb8a13551196dd3`，
  匹配 vm2api v1.3.206 固定 payload 的 Codex 内核。

这组证据将实际执行位置定位到 guest，不能只凭 runner 容器存活或 socket
文件存在作同样结论。该快照曾累计 16 次 service restart，属于修复过程，
不能将其描述成始终零重启的稳定运行。

发现并修复的生命周期问题是：`slot.env` 原来放在 systemd 管理的
`RuntimeDirectory` 中，服务 stop 时该目录会被删除，后续 start 丢失环境
配置。修复后配置与易失运行目录分离。`service-lifecycle.json` 的 stop/start
验证记录 PID 从 3025 变为 3177，`config_survived_stop=true`，
`runtime_removed_at_stop=true`，配置权限 `0600`。

另外，接入合同修正包括规范 Codex `chatgpt_account_id` 字段，以及按固定
内核合同发送双内部认证头。本文不记录任何实际认证值。

## 真实推理结果

| 脱敏证据 | 时间（北京时间） | HTTP | 终端状态 | 输入／输出／缓存 token | 耗时 |
|---|---|---:|---|---|---:|
| `native-internal-auth-fixed.json` | 14:58:32 | 200 | `response.completed` / `completed` | 20 / 8 / 0 | 3384 ms |
| `sub2-envelope.json` | 15:00:36 | 200 | `response.completed` / `completed` | 20 / 8 / 0 | 3623 ms |

两次请求的请求模型和返回模型均为 `gpt-6-luna`，包含实际文本 delta、完整
成功终端及权威用量，总 token 为 28，reasoning token 为 0。第二条验证使用
sub2-api 对应的请求 envelope；它证明该 envelope 与 guest 内核相容，**不应
写成已经经过完整 sub2-api HTTP handler、调度、数据库用量落账或 WS 会话**。

## 代理失败边界

`proxy-unavailable.json` 于 15:04:39（北京时间） 记录 HTTP 502，耗时 33 ms，
`completed=false`，没有成功终端或 usage。它证明此次不可用代理条件下，
应用请求失败，没有取得成功推理结果。

这不是网络层防绕过验收：不能据此声称 guest 防火墙已强制禁止全部直连、
DNS 绕过或其他出口。该硬约束需要独立网络证据。

## 最终镜像与私有 Go client 验证

`final-image.json` 记录最终镜像：

- tag：`sub2-vm-runner:20261022-arm64-codex-fixed`
- ID：`sha256:e84386026bc7e2f03c7f3bae557bba038db8b3df45aa1d588a1c624f89dbf435`
- 架构：`arm64`；临时 build context 已清理。

`final-runtime-switch.json` 记录 15:05:06（北京时间） 已切换到该镜像并复用原 guest
磁盘，actual accelerator 为 `tcg`，requested CPU 为 `qemu64`，expected
有效 CPU 为 `max`。QEMU 包为 `1:10.0.13+ds-0+deb13u1`，OVMF 为
`2025.02-8+deb13u1`。

上述两条 envelope 推理成功发生在最终镜像切换之前。随后新增的
`final-image-guest.json` 于 15:05:45（北京时间） 确认最终镜像已经启动成功：

- image ID 与上述 `e843860…` 完全一致；guest 为 `x86_64`。
- guest boot ID 为 `151c54da-7fe8-46e5-993e-a883a047046a`，不同于切换前。
- Codex kernel PID 为 826，服务 active/running，`NRestarts=0`；relay PID
  为 827，同样 active/running、`NRestarts=0`。这是该观察时刻的计数，
  不是长期稳定性或压力测试结论。
- PID 826 的 executable SHA256 仍匹配前述固定 Codex payload。
- QMP 显示 guest running，KVM enabled/present 仍均为 false。

`go-client-live.json` 于 15:06:04（北京时间） 补充了最终镜像上的真实请求验证。
测试使用实际 `backend/internal/vmkernel.Client` 源码编译 Linux ARM64
可执行程序，在 runner 中经 guest relay 发起请求；不是 fake-kernel fixture。
结果为 `ok=true`，`accepted`、`dispatched`、`has_usage`、`completed` 均为
true，终端为 `response.completed`，模型 `gpt-6-luna`，输入 20、输出 8、
缓存 0、总 token 28，耗时 4369 ms。

因此最终镜像上的私有 Go client → guest relay → guest Codex 内核 → 实际
模型推理链路已经取得成功证据。它**仍未经过完整 VMKernelService/helper
生命周期、账号调度、公开 HTTP handler、原有数据库计费落账或 WebSocket
多轮会话**；这些层不能借用本次私有 client 成功作为真实集成验收结论。

## 源码验证与清理

新增 guest engine/lifecycle 6项测试通过；加上第一阶段20项运行包测试，共26项。Go VM service/client专项通过（1.831s/0.974s）；最终双鉴权头与取消测试的vmkernel竞态检查通过（1.384s）。新增Messages测试fixture曾因不完整终端导致失败，修正fixture后通过，未放宽生产解析。私有Go client实测另单独编译运行，结果如上。

所有Go测试/构建使用独立临时GOCACHE/GOTMPDIR并在退出后确认清理，保留GOMODCACHE。用于实测的临时Go源码目录已删除，二进制随测试容器移除；没有启动Testcontainers会话。

`cleanup.json`确认测试账号目录/记录、代理绑定和`vms`/`codex_login_configs`/`codex_login_jobs`中的本实例行均已清理；两个测试容器已停止删除，Colima `/proc`核查没有本实例QEMU或Codex进程。`vm.allow_tcg=false`。四个原业务容器启动时间保持，vm2api与现有Sub2API的health均为HTTP200/ok。

`image-cleanup.json`确认仅保留当前`…-codex-fixed`与一个已验证的回滚`…-fixed`标签；旧的两个本轮runner标签已删除，没有全局Docker prune。收尾空间：Mac约108.24GiB，Docker执行端约1.65GiB。保留脱敏报告与请求记录。

原Sub2API工作区三处ticket-borrow未提交修改的整体diff SHA256仍为`36d69013ecf3c3c433c1cf6c5c8afe3ff3356cc690a1ba8cf4c370877ad7f583`；原vm2api工作区未修改。实现与文档留在`codex/vm-kernel-account-mode`，未提交或推送。
