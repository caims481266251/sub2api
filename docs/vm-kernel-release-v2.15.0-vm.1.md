# Sub2API v2.15.0-vm.1

基于上游 v2.15.0 的 VM 内核模式预发布版。普通账号仍为默认模式；VM 账号通过独立 guest 调用固定内核，账号管理、授权、分组、API key、调度和计费复用 Sub2API 原有业务入口。

## 功能

- 新增账号执行模式、后台实例准备、就绪检测及失败恢复。
- GPT Responses JSON/SSE、WebSocket、工具调用和协议转换接入原网关。
- 为账号分配独立实例、网段、地址和持久客户端身份，支持 SOCKS5 代理出口。
- 并发跟随原账号配置；Codex 投影显式设置 rpm=0、max_concurrency=0，关闭内核的额外准入配置。
- 已知模型调用错误不作为 VM 健康异常；上游限流和本地内核容量拒绝分别分类。

## 安装包

提供 Linux amd64、arm64 两种包，内含带嵌入式前端的 sub2api、vm-runtime-helper、部署脚本、来源记录、许可证和 BUILD.txt。通过 checksums.txt 校验下载。

解压后阅读 deploy/vm-kernel/README.md。主应用与 helper 分开运行，helper 需要 Linux Docker/QEMU 环境及受限共享 Unix socket。迁移包含 296_account_execution_mode.sql。

安装包不含业务数据库、账号凭据、代理密码、已有 VM 磁盘、guest 基础镜像或内核 payload。首次部署须按 deploy/vm-kernel/upstream/README.md 准备固定 runner 镜像、单独的内核 payload、不可变摘要及模板，再启用 VM_KERNEL_ENABLED。普通模式默认不启用 VM。

guest/payload 为 x86_64。硬件 KVM 需要兼容的 Linux x86 主机和可用 /dev/kvm；arm64 主机仅可显式配置 TCG，此包不声称提供 ARM 硬件 KVM。runner 的上游许可随包保留。

## 验证与已知边界

- 本地真实 GPT 业务入口进行了 100 worker、300 秒测试，总耗时 308.88 秒；账户、用户、API key 租约及 guest relay 连接峰值均为 100。
- 共完成 1578 次：295 成功、1283 失败（公共返回聚合为 upstream_error）。296 条唯一计费记录包含 1 次预热，费用 0.01884894；临时用户和 key 已清理。
- 所有 VM 健康采样均 ready，结束账号 schedulable=true，租约归零。但失败退避期间并未持续维持 100 条在途连接，不能宣称持续 100 在途成功验收通过。
- 先前独立内核诊断观察到 429 usage_limit_reached 和后续 503 no_capacity；这不是本轮 1283 次错误的逐条归因。本轮还观察到 PostgreSQL too many clients already，压测结束后恢复健康，数据库连接容量仍需处理。
- 本地实测为 TCG；硬件 KVM、Claude 完整真实业务验收尚未完成。本版本为预发布，不更新稳定版 latest。

本版本不承诺每个账号具有唯一 TLS 指纹：独立持久身份不等同于每个网络库版本生成唯一的 TLS/HTTP 指纹。
