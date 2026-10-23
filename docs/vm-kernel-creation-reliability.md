# VM 新建账号自动初始化修复

本轮目标：新装或清空本部署运行数据后，账号管理中新建 VM 账号能够自动完成实例准备；不依赖运维临时创建文件。原账号、代理、并发、调度和计费保持原逻辑。

## 发现并修复的问题

1. **清空后的 coordinator 锁丢失。** 原清理流程删除 `run` 目录；helper 恢复 socket 后目录为 root:service-gid 0710，非 root 应用不能创建锁，VM 服务初始化直接返回 nil，HTTP 健康检查却仍通过。
   - helper 现在在开始监听前自动准备 `<socket>.coordinator.lock`。`--service-uid` 指定应用 UID，权限 0600。
   - 重复启动保留原 inode，不截断、不替换；拒绝符号链接、硬链接、FIFO 和目录。
   - 主应用启用 VM 时，在开放 HTTP 服务前获取并持有协调锁；获取失败明确退出，不能再隐藏成普通健康服务。未启用 VM 时不受影响。
   - 本地部署已配置 `--service-uid 1000 --service-gid 20000`。

2. **实际网卡被误判不存在。** 新 VM 已经获得正确的 IPv4 地址，但 `ip -j -4 address show` 不包含 MAC 字段，按 MAC 选择网卡的验证必然失败，导致 per-boot setup 停止。
   - 改为读取 `ip -j address show` 的完整链路信息，再筛选 IPv4 地址和默认路由。
   - 保留严格地址/路由核验，没有忽略验证错误或放开直接出站。
   - 新增回归测试模拟实际 iproute2 的 `-4` 输出缺少 MAC，而不是用不符合真实工具行为的 fixture。

## 验证范围

- Go 定向单元测试和 race 测试通过：自动准备、重复初始化、锁互斥、危险路径拒绝、VM 开关和启动 guard。
- 真实 Linux helper 连续三轮从空目录启动；UID 1000 应用均可访问 socket 并加锁；helper 重启期间锁 inode 不变，第二个应用不能获取锁。
- 在本地部署尝试启动第二个主应用进程，进程在开放服务前退出并报告 `vm_kernel_coordinator_already_running`。
- Python 运行包测试 70 项通过，包括 iproute2 输出回归。
- 用户账号 #1 保留原业务记录、代理 #1、并发 100。对从未初始化成功的 VM 更换失败的首次启动磁盘，保留实例、客户端身份和网络，使用新版镜像完整运行 cloud-init 和 per-boot setup；最终管理 API 返回 `kernel_ready / ready=true`。
- 通过原账号测试接口 `/api/v1/admin/accounts/1/test` 调用 `gpt-6-luna`，收到 `VM_OK` 及 `test_complete / success=true`，耗时约 4.89 秒。
- 应用和 helper 同轮重启验证通过：账号恢复 `ready=true`，实例、设备身份、网络、VM boot ID 与锁 inode 均保持一致，没有重新创建 VM。结果见本轮 `restart-result.json`。

这是一轮本机 TCG 验证，不代表硬件 KVM、多主机调度或任意第三方代理均已验证。源码中的初始化故障已修复；外部代理、凭据或宿主资源实际不可用时仍应正确报告失败，不能把状态强制改为正常。

脱敏证据：`.dev/vm-kernel/verification/creation-startup-20261022/`。部署脚本保留业务数据；未重新清空用户手动添加的账号。
