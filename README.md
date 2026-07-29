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
- 使用响应头 `X-Session-ID` 复用会话

当前不支持图片等结构化消息。默认启动参数包含 `--yolo`，仅建议在受信任的本机项目目录中使用。
