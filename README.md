# trae-api

将 `trae-cli acp serve` 包装为 OpenAI 兼容的 HTTP 服务。

## 安装

要求 Go 1.23 及以上，并已安装、登录 `trae-cli`：

```bash
go install github.com/lftk/trae-api@latest
```

## 使用

启动服务：

```bash
trae-api
```

默认监听 `127.0.0.1:8723`。

常用配置：

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `TRAE_API_ADDR` | `127.0.0.1:8723` | 监听地址 |
| `TRAE_API_WORKDIR` | 临时目录 | 项目目录 |
| `TRAE_API_TOKEN` | 空 | 非本机监听时必填 |
| `TRAE_API_BIN` | `trae-cli` | CLI 可执行文件 |
| `TRAE_API_YOLO` | `true` | 是否以 `--yolo` 启动 ACP |
| `TRAE_API_DEBUG` | `false` | 是否输出请求、响应和 ACP 调试日志 |
| `TRAE_API_SESSION_IDLE_TIMEOUT` | `720h` | session 空闲过期时间 |
| `TRAE_API_IMPLICIT_SESSION_IDLE_TIMEOUT` | `30m` | 无 session ID 请求的隐式会话空闲过期时间，设为 `0` 禁用隐式会话 |
| `TRAE_API_SESSION_SCAN_INTERVAL` | `1m` | session 过期扫描间隔 |
| `TRAE_API_WARM_PROCESSES` | `4` | 后台预热的空闲 ACP 进程数，设为 `0` 表示不启动预热 |
| `TRAE_API_MAX_SESSIONS` | `100` | 稳定 session 数量上限 |
| `TRAE_API_MAX_PROCESSES` | `100` | trae-cli ACP 进程总数上限 |

未设置 `TRAE_API_WORKDIR` 时，服务创建的临时目录仅是 ACP 所需的隔离占位工作区，
并不代表调用方的真实项目。每个 ACP session 的首次 prompt 会自动加入工作区声明，
要求代理不要从该目录推断项目结构或操作其中的文件，而只使用对话中提供的上下文。
如果任务需要访问真实项目文件，仍须在启动服务时显式设置 `TRAE_API_WORKDIR`。

## API

```bash
curl http://127.0.0.1:8723/v1/models

curl http://127.0.0.1:8723/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"","messages":[{"role":"user","content":"请简单介绍一下你自己"}]}'
```

支持：

- `GET /healthz`
- `GET /v1/models`
- `POST /v1/chat/completions`
- 文本消息和流式响应（`"stream": true`）
- 使用请求头 `X-Session-ID` 复用会话；匿名请求不会自动返回可复用的 session ID
- Claude Code 可直接使用其 `X-Claude-Code-Session-Id` 请求头复用会话（Claude Code 2.1.86+）
- 无 session ID 的请求（如 VS Code Chat）通过消息历史指纹自动识别会话延续：请求的消息前缀与某个隐式会话的已记录 transcript 一致时，复用该会话的 ACP 进程，只发送新增消息；重放相同请求、编辑历史或新对话则创建新会话并携带完整消息历史。隐式会话在 `TRAE_API_IMPLICIT_SESSION_IDLE_TIMEOUT`（默认 30 分钟）空闲后回收，设为 `0` 恢复“每次请求创建并立即销毁临时会话”的旧行为

当前不支持图片等结构化消息。默认启动参数包含 `--yolo`，仅建议在受信任的本机项目目录中使用。

每个逻辑 session 都独占一个 `trae-cli acp serve` 进程。服务启动后会在后台预热
`TRAE_API_WARM_PROCESSES` 个已完成 ACP 初始化的空闲进程；稳定 session 优先领取
预热进程，领取后由后台补充池容量。设置为 `0` 时不启动预热，但首次请求会触发后台
创建。进程总数受 `TRAE_API_MAX_PROCESSES` 限制，达到上限时新的请求会等待已有进程结束；稳定 session 数量仍受
`TRAE_API_MAX_SESSIONS` 限制。稳定的
`X-Session-ID` 或 `X-Claude-Code-Session-Id` 会复用对应的 ACP session 和进程，
不同 session 可以并发执行 prompt。首次请求需要承担进程启动和 ACP 初始化耗时；
后续请求不会重复启动进程。没有稳定标识的请求每次创建请求级临时 ACP session 和
进程，prompt 完成或请求结束后会销毁该 session 及进程。服务只使用当前配置的一个
工作目录。

显式 session 当前仅存储在内存中，默认连续空闲 30 天后过期。若 ACP 声明支持
`session/close`，过期 session 会向 ACP 发送关闭请求。匿名请求使用完整的
`messages`，prompt 完成后立即从 client 的 ACP session 路由表中释放临时 session。
某个 session 的 ACP 进程崩溃只会使该 session 失效，下一次请求会重新懒启动该
session 的进程；正在进行的请求会收到 upstream/ACP 错误。服务重启后需要重新建立
session。

无 session ID 的请求应在每次请求中携带完整消息历史。显式 session ID 的首次请求
可以携带初始上下文，后续请求只需携带新增消息，历史由 ACP session 保存。服务端
不会保存或比较 transcript（隐式会话只保存消息序列的指纹）。若 ACP 不支持 `session/close`，临时 session 的本地
引用和对应的 trae-cli 进程都会在请求结束时释放。
