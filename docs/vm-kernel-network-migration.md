# 旧 VM 实例的受控网络迁移

`migrate_network` 是私有 helper Request 的运维参数，不是账号 API 参数，也不是业务请求入口。常规账号请求仍由 Sub2API 原有调度、并发和计费链路处理。

调用前，运维执行者必须停止应用接收新请求，并等待该账号全部在途请求结束。helper 自身无法证明应用已经排空，因此不能把此参数加入自动业务重试路径。

对仍使用旧网络、manifest 中 `network` 为空的实例，发送 `operation: "prepare"`、`migrate_network: true`，并使用严格大于已有 manifest generation 的新 generation。使用原 account_id、instance_id 和固定模板配置；不创建新业务账号，不修改分组或密钥。若仅更换模板对应的镜像，由运维更新 helper 模板映射，不改变实例的 immutable spec。

helper 按以下顺序执行：

1. 持久保存新的 generation 和 `ready: false`，读取并保存已有客户端安装身份。
2. 停止旧容器，确认旧容器删除成功。任何未知结果都使迁移失败，此时保留原 container_id，且不分配新网段。
3. 清空 container_id 并持久保存，再分配持久的独立 Docker 与 guest 网段。
4. 在原实例目录创建新 runner，保留原虚拟机标识、客户端身份、凭据和磁盘。只有实际网络、guest 和内核验证完成才恢复 ready。

成功后的同 generation、同内容 prepare 重放只检查现有实例，不重复删除或分配网络。失败重试应使用更新的 generation；已经分配的租约继续复用，不能为重试释放或更换租约。迁移过程不设置账号删除标记，也不调用业务账号删除流程。

运维验收必须检查 helper 返回的 ready、实际 Docker 地址、guest 地址及身份一致性，再恢复应用入口。错误或超时不是迁移完成证明。保留磁盘依赖 DockerDriver.Remove 只删除容器及归属网络，不删除实例 bind mount 数据目录。
