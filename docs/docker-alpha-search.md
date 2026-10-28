# Docker 账号独立搜索

2026-10-28。

Docker OpenAI 标准 OAuth 账号支持现有 `/alpha/search`、`/v1/alpha/search` 与 `/backend-api/codex/alpha/search` 入口。原故障是账号能力判断排除了 `alpha_search`，导致纯 Docker 分组在调度阶段没有候选账号。

## 调用链

```text
现有 AlphaSearch handler：鉴权、分组、选号、并发、RPM、预算
  → ForwardAlphaSearch：当前账号准入、模型映射、搜索参数清理
  → 主服务 HTTP 客户端 + 所选账号 OAuth + 所选账号绑定代理
  → https://chatgpt.com/backend-api/codex/alpha/search
  → 原有状态处理、错误换号、原始响应及按次计费
```

该实现参考 VM2API 的独立搜索路径。搜索不经过 kin-codex-kernel；Responses、Chat 和 WS 推理仍由容器启动时运行的常驻内核处理，不新增进程、socket 或镜像依赖。

## 边界

- 搜索参与原有账号调度与并发控制。Docker 未就绪、账号停用、限流或分组/路由变更时不发出请求；不会因开放搜索能力绕过这些检查。
- 只支持合法 Docker OpenAI 标准 OAuth 账号；不借此开放 Docker 的其他非推理端点或特殊认证模式。
- 仅使用账号绑定代理，不进入票据、插件或代理备用/直连路径。目的地址固定，禁用重定向；代理错误由原有业务错误流程处理，不写入容器健康状态。
- 使用账号 OAuth access token 和 ChatGPT account ID。UA 沿用 `vmkernel.DefaultCodexUserAgent`，originator 和 version 从同一个 UA 派生；调用方和普通账号身份覆盖不影响该请求。
- `x-codex-turn-metadata` 原样透传，不额外改写其会话/turn ID。不发送 Responses 专用 session、turn-state、Beta 或 Lite 请求头。
- 保留原有模型映射，删除 `prompt_cache_key`、`prompt_cache_retention`、`store`。其余搜索请求字段保留，不转换为推理请求。
- 原始 JSON 响应字节（包括 `encrypted_output`）保持不变。仅成功搜索返回 `WebSearchCalls=1`，错误、无效 JSON、读取失败或取消不产生成功搜索计费。
- 账号认证、限流和换号策略沿用 Sub2API 现有独立搜索逻辑，不另建 VM2API 的 OAuth 刷新/调度体系。特别是独立搜索 401 不直接把账号永久置错。
- 请求取消传递给现有 HTTP transport；继续使用本项目的 HTTP 超时/响应读取策略，不添加 Docker 搜索专属大小或业务并发限制。

搜索和推理可以共用账号及出口，但分别由 Go HTTP 客户端和内核发送；相同 UA 不等于相同 TLS 特征，亦不能据此认定上游连接复用。

## 验证入口

`backend/internal/service/openai_docker_alpha_search_test.go` 覆盖真实调度选择、就绪状态、身份/参数与原始响应、按次成功标记、错误不计费、代理绑定、插件隔离、取消和发送前账号变更。现有 AlphaSearch、计费、代理和 VMKernel 回归继续使用原测试。

本地真实调用记录见 `.dev/diagnostics/20261028-docker-alpha-search/`；正式发布需包含主程序改动，本补丁不要求重建账号容器镜像。
