# 代码布局与简化边界

单模块、根 `package main`。核心实现按自然职责保留七个 Go 文件：

- `main.go`：标准库 `FlagSet` 解析、配置、单个 HTTP listener/server、服务并发门和请求生命周期。先绑定端口再生成回环回调 URL；关闭时取消在途请求并优雅退出。
- `auth.go`：内存账号、设备证明续期、浏览器授权及登录管理处理；一次性授权与取消/替换保护不变，不再独立监听回调端口。
- `login_cli.go`：运行中服务的本地登录 CLI、浏览器打开和取消。
- `models.go`：Trae 请求头、模型发现与路由元数据缓存；账号代次隔离不变。
- `upstream.go`：请求转换、SSE/工具增量解析及受限认证重试。动态协议事件形状保留兼容解析。
- `types.go`：Chat 请求输入归一化、工具历史校验、内部消息/用量类型。SDK 输入校验与原始 content/schema 约束都保留。
- `http.go`：OpenAI Chat/Models HTTP、SDK 响应编码与流式输出，含 Host/Origin 与 Token 防护。精确 `GET /authorize` 单独允许回环来源的官方浏览器跳转，不扩大其他路由的鉴权例外。

TRAE-007 运行约定：本机监听使用 loopback；带 Token 的远程访问使用 wildcard (`0.0.0.0` 或 `[::]`) 并仍让授权回调走同端口的 loopback。显式绑定单个非回环接口无法在单 listener 下同时接收 loopback 回调，因此配置阶段拒绝并提示 wildcard + Token；不启动第二服务或隐式扩大监听。单 Server 不设置全局 `WriteTimeout`，避免截断长 SSE。

实际删掉的重复路径：请求体原先先索引原始字段，随后 `decodeChatRequest` 再索引一次；现在沿用第一次校验后的字段索引。HTTP 和直接消息 JSON 解码共享文本块与 tool-call 缺省值归一化。流式 delta 不再携带无人读取的 `Tools`；错误 JSON 的未调用 `set` 方法及仅为测试存在的 `buildIDEBody` 包装已移除，测试直接调用生产构造函数。

原先的 `config.go`、`server.go`、`login_http.go`、`openai_requests.go`、`openai_types.go` 按上述职责并入已有文件；这只是布局合并，不作为代码删减量。历史测试按认证、CLI/进程、并发/边界、HTTP/SDK、模型与 SSE 合并，保留原测试与子测试名称和断言。兼容性回归另覆盖 `stop`、`n`、reasoning、response_format、文本块和错误类型。

**不可为减法删除**：流式输出前模型预检、其后的账号代次重读、共享续期和凭证绑定隔离、请求和 SSE 的拒绝/终止路径。它们分别防止提前 HTTP 200、串账号、重复令牌消耗和不完整回复。保留客户端提供完整历史与客户端执行工具的边界；无新增持久化或真实账号验证。
