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
| `TRAE_API_SESSION_IDLE_TIMEOUT` | `720h` | session 空闲过期时间 |
| `TRAE_API_SESSION_SCAN_INTERVAL` | `1m` | session 过期扫描间隔 |

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
- 使用请求头 `X-Session-ID` 复用会话，session ID 为随机 UUID
- Claude Code 可直接使用其 `X-Claude-Code-Session-Id` 请求头复用会话（Claude Code 2.1.86+）

当前不支持图片等结构化消息。默认启动参数包含 `--yolo`，仅建议在受信任的本机项目目录中使用。

服务进程内会懒启动一个共享的 `trae-cli acp serve` 进程；每个逻辑 session
在该连接上拥有独立的 ACP `SessionId`。因此进程生命周期与 ACP session 生命周期
是分离的：稳定的 `X-Session-ID` 或 `X-Claude-Code-Session-Id` 会复用对应 ACP
session，而没有稳定标识的请求每次创建新的 ACP session，但不会创建新的 trae-cli
进程。服务只使用当前配置的一个工作目录。

session 当前仅存储在内存中，默认连续空闲 30 天后过期。若 ACP 声明支持
`session/close`，过期 session 会向 ACP 发送关闭请求；否则至少删除本地映射并记录
警告。共享进程崩溃会使该进程上的所有 session 一起失效，下一次请求会重新懒启动
进程；正在进行的请求会收到 upstream/ACP 错误。服务重启后需要重新建立 session。
