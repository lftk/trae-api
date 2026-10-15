# TRAE-008 代码职责

单 Go 模块，根 `package main`；组件依赖以数据边界而非文件数划分。

- `command.go`：仅声明 `urfave/cli/v3@v3.14.0` 的根命令、`api`、`auth`、`login` 及 flags/env 来源和 Action；不再保留旧 `parseCommand`、手写帮助/版本或第二次 argv 分发。
- `coordinator.go`：默认命令并行运行 API/Auth，任一失败取消并等待另一方；独立命令只启动所需组件。
- `main.go`、`http.go`、`models.go`、`upstream.go`、`types.go`：API 监听、请求校验、Chat/Models、模型路由和 SSE。API 每次请求仅解码固定文件中的窄 `apiCredential`，以其快照完成整次请求；模型缓存按访问快照摘要隔离。API 不持有 `authManager`，没有登录路由、刷新调用或失败后重放。
- `auth_service.go`、`auth.go`：Auth 独立 loopback Server，拥有浏览器回调、一次性状态、设备证明与受限续期；发布账号状态前持久化。跨站浏览器回调只在精确 `GET /authorize`、受信 Host 和回环 TCP 对端上例外放行；其它管理请求仍验证本机来源及同源。
- `credential_store.go`：固定路径 `os.UserConfigDir()/trae-api/auth.json` 的版本化 JSON。Auth 进程持有稳定 sidecar 排他锁并唯一写入；受限同目录临时文件同步后原子替换。API 只使用不含 refresh token、设备私钥的窄解码类型。格式、安全限制与未验证范围见 [凭证格式](CREDENTIALS.md)。
- `login_cli.go`：只联系 Auth loopback 地址，禁用本机管理请求的环境代理，不跟随重定向；不将网关 `TRAE_API_TOKEN` 发送给 Auth。

本轮删除的是旧自写命令解析/分发、单服务回调例外与 API 持有认证状态、主动续期和重放的耦合代码。旧“一个 listener、仅内存、不落盘”断言与新设计相反，已替换为隔离目录、双组件、只读 API 和原子文件专项测试；Chat/SDK/SSE、请求拒绝及仍适用的安全测试继续执行。API HTTP Server 不设全局短 `WriteTimeout`，保留长 SSE；客户端仍提供完整历史并自行执行工具。没有真实账号登录或付费模型调用证据。
