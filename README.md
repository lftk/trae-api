# trae-api

单账号 Trae CN → OpenAI **Chat Completions** 网关，直连国内版 IDE 模型通道。不依赖 trae-cli/ACP，不执行客户端工具，不保存凭证或对话。

## 启动与登录

从当前源码构建（Go 1.25+）：

```bash
go build -o trae-api .
./trae-api                # 前台启动 API 服务
# 另一个终端、同一台电脑：
./trae-api login          # 给正在运行的服务登录
```

`login` 请求服务生成授权链接并打开官方 Trae 授权页面，不需要先访问本地登录网页。授权回调固定监听 **127.0.0.1:8765**；端口被占用时服务启动失败，不自动换端口。命令确认该次登录成功后退出，凭证仅留在服务内存，模型请求时自动刷新。服务重启后必须重新登录。

- 服务未启动：明确报错，不偷偷启动后台进程。
- 浏览器打开失败：打印官方授权链接，供手动打开；不打印令牌或携带凭证的完整回调 URL。
- 等待最多十分钟；收到本次 attempt ID 后，Ctrl+C/超时会尝试定向取消，迟到回调不能完成已取消/过期的登录。首次请求若已创建尝试但响应丢失，CLI 无法得知 ID，因此退出不保证撤销；残留尝试最长十分钟有效，新尝试替换旧尝试，不按“最新尝试”取消以免误伤其他 CLI。
- 同时发起多个尝试时，最新一次替换前一次；服务已登录时命令直接报告已登录。
- 网关鉴权失败时检查两端相同的 `TRAE_API_TOKEN`。Token 是网关鉴权信息，不是 Trae 账号凭证。
- 默认前台运行；Homebrew 服务种子见 [packaging/homebrew-tap/README.md](packaging/homebrew-tap/README.md)。已有发布版本不一定包含本实现。

## 配置

仅支持两个环境变量（服务与 login 使用相同配置）：

- `TRAE_API_ADDR`：默认 `127.0.0.1:8723`，支持 localhost 或字面 IP 的 host:port。非回环监听必须设置 Token。不能使用授权端口 8765。
- `TRAE_API_TOKEN`：默认空；设置后所有 API 和管理请求需 `Authorization: Bearer <gateway-token>`。login 自动从环境变量读取并发送，不打印该值。

其他 `TRAE_API_*` 配置变量明确报错，请从启动环境移除。模型/认证协议地址、授权端口、客户端版本、并发和超时均为内部默认值，不开放用户配置。无 Token 的全部 API 路由仅接受 localhost/字面回环 IP Host，并拒绝跨源请求；不解析 DNS、不信任转发 Host。实际授权回调监听器同样检查本地 Host，但不套用管理接口同源限制，允许官方跨站顶层授权跳转；仍要求当前有效的一次性 attempt。

## API

- `GET /healthz`：服务状态和是否登录，不调用模型。
- `GET /v1/models`：账号模型列表，缓存五分钟。
- `POST /v1/chat/completions`：文本、文本 content blocks、流式/非流式、`reasoning_content`、函数工具与工具结果。
- `/v1/responses`：明确返回 HTTP 501 unsupported，不转发到 Chat。无 Responses、previous_response_id、custom/namespace 工具或快照功能。

```bash
curl http://127.0.0.1:8723/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

有网关 Token 时加 Bearer 头。`model` 为空或 auto 时选择可用模型列表排序后的首项；明确模型名直接透传。标准 Chat 响应和 chunk 使用官方 `openai-go/v3` SDK 默认编码，仅补充 `reasoning_content` 扩展。

### 完整历史与客户端工具

**每次请求携带完整 messages 历史和所需 tools。** 服务不保存 Chat 显式或隐式会话，不合并历史；`X-Session-ID`/`X-Claude-Code-Session-Id` 不产生任何会话效果，不能只传新增消息/工具结果。每个上游请求使用独立 ID。

`reasoning_content` 本版仅保证响应输出，不保证 assistant 推理历史输入或往返保真；客户端完整历史要求针对受支持的文本和工具消息，不代表推理字段会回传上游。

支持标准函数 `tools`、`tool_choice`（auto / none / required / 指定函数）与 `parallel_tool_calls`。工具在客户端执行；客户端把返回的 assistant.tool_calls 和每个匹配 ID 的 role=tool 结果纳入完整历史后再次请求。网关不执行命令、不读取客户端项目、不伪造成功结果。流式参数按稳定 index 拼接；原生无 ID/index 续片跟随最近寻址调用，显式别名冲突报错。

不支持图片、托管工具、结构化输出、可调 reasoning 参数及非 null 的 `stop`；返回明确 HTTP 400，不静默忽略。temperature/top_p 只透传，IDE 不保证遵循。认证拒绝且没有模型输出时最多刷新重试一次；网络/业务错误或已输出请求不重放。提前刷新临时失败不永久废弃仍有效的令牌。

## 不落盘与旧版本迁移

凭证、刷新令牌仅在当前进程内存，历史完全由客户端提供。服务不读取、写入、恢复或清理任何 credential.json、sessions、conversations-v2 文件；不接触旧版本留下的文件。移除旧配置不会删除旧数据，请由用户自行管理。

## 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o trae-api .
```

所有测试使用合成凭证与本机模拟服务，不调用真实模型，不执行真实浏览器授权；离线通过不代表实际上游或真实登录已验证。协议来源及许可证见 [REFERENCES.md](REFERENCES.md)。
