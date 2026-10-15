# trae-api

单账号 Trae CN → OpenAI Chat Completions 网关。凭证仅在内存，对话无状态；不执行客户端工具。

## 启动

Go 1.25+，在仓库根目录：

```bash
go build -o trae-api .
./trae-api             # 终端一：前台服务，默认 127.0.0.1:8723
./trae-api login       # 终端二：为运行中的服务打开官方授权页
```

回调固定监听 `127.0.0.1:8765`，端口占用则服务启动失败。登录等待最多十分钟；CLI 取得本次 attempt ID 后，取消或超时会尝试定向撤销。若首个响应丢失，CLI 无法得知 ID，残留尝试最多十分钟有效；已取消、过期或被替换的回调不能安装凭证。服务重启后须重新登录，CLI 不会偷偷启动服务。浏览器打开失败时可手动打开命令打印的官方授权链接；不打印带凭证的回调。Homebrew 服务种子见 [安装说明](packaging/homebrew-tap/README.md)，已发布版本不一定包含当前源码。

## 配置

仅支持两个环境变量（服务与 login 使用相同配置）：

- `TRAE_API_ADDR`：默认 `127.0.0.1:8723`，支持 localhost 或字面 IP 的 host:port。非回环监听必须设置 Token。不能使用授权端口 8765。
- `TRAE_API_TOKEN`：默认空；设置后所有 API 和管理请求需 `Authorization: Bearer <gateway-token>`。login 自动从环境变量读取并发送，不打印该值。

其他 `TRAE_API_*` 配置变量明确报错，请从启动环境移除。模型/认证协议地址、授权端口、客户端版本、并发和超时均为内部默认值，不开放用户配置。无 Token 的全部 API 路由仅接受 localhost/字面回环 IP Host，并拒绝跨源请求；不解析 DNS、不信任转发 Host。实际授权回调监听器同样检查本地 Host，但不套用管理接口同源限制，允许官方跨站顶层授权跳转；仍要求当前有效的一次性 attempt。

## 接入

- `GET /healthz`：健康状态及是否登录。
- `GET /v1/models`：当前账号模型列表（缓存五分钟）。以返回 ID 为准；满足上游元数据条件时可含 `<基础ID>-max`。`__dev`/`__max` 是上游模型名后缀，不是必然可用的客户端 ID。
- `POST /v1/chat/completions`：文本、流式/非流式、`reasoning_content` 输出扩展、函数工具及工具结果。`model=auto` 或空值选可用列表排序首项；显式模型须在账号可用列表内。
- `/v1/responses`：HTTP 501 unsupported；无 Responses、previous_response_id 或服务端会话续接。

```bash
curl http://127.0.0.1:8723/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"auto","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

设置 Token 时另加 Bearer 头。**每次请求传完整 `messages` 历史和所需 `tools`**；网关不保存历史，`X-Session-ID` 等头不续接会话。客户端自行执行工具，将助手 `tool_calls` 及每个匹配 ID 的 `role=tool` 结果放入后续完整历史；网关不执行或伪造工具结果。`reasoning_content` 仅保证输出，输入推理历史不保证保真。支持文本和函数工具；图片、托管工具、结构化输出、可调 reasoning 参数等不支持。认证拒绝且未输出时最多续期重试一次；网络/业务失败或已输出请求不重放。

**本机离线接入演示**：[examples/README.md](examples/README.md)（Python 标准库 + 本地 mock，含流式、推理字段、工具调用和错误场景）。

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
