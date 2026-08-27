# trae-api

将 `trae-cli acp serve` 包装为 OpenAI 兼容的 HTTP 服务，让任何 OpenAI 客户端（VS Code Chat、Claude Code、自写脚本等）都能使用 Trae CLI 作为后端模型。

## 安装

前置要求：

1. 安装并**登录** `trae-cli`（登录一次即可，服务本身不处理认证）。安装方式见 [Trae CLI 官方文档](https://docs.trae.cn/cli)；首次运行 `trae-cli` 任意命令会引导完成登录。
2. Go 1.23 及以上。

安装服务：

```bash
go install github.com/lftk/trae-api@latest
```

## 使用

启动服务（默认监听 `127.0.0.1:8723`）：

```bash
trae-api
```

然后像使用任何 OpenAI 兼容服务一样调用：

```bash
# 查看可用模型
curl http://127.0.0.1:8723/v1/models

# 发起对话（文本消息、流式响应都支持）
curl http://127.0.0.1:8723/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"","messages":[{"role":"user","content":"请简单介绍一下你自己"}]}'
```

其他入口：

- `GET /healthz`：健康检查
- `POST /v1/chat/completions`，`"stream": true` 时返回流式响应
- 请求头 `X-Session-ID`：复用会话，续聊时只需发送新增消息；Claude Code 可直接使用其 `X-Claude-Code-Session-Id` 头复用会话
- 无 session ID 的请求（如 VS Code Chat）需每次携带完整消息历史，服务会自动识别并复用对应会话

### 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `TRAE_API_ADDR` | `127.0.0.1:8723` | 监听地址 |
| `TRAE_API_WORKDIR` | 临时目录 | 项目目录，访问真实项目文件时必须设置 |
| `TRAE_API_TOKEN` | 空 | 非本机监听时必填 |
| `TRAE_API_BIN` | `trae-cli` | CLI 可执行文件路径 |
| `TRAE_API_YOLO` | `true` | 是否以 `--yolo` 启动 ACP |
| `TRAE_API_DEBUG` | `false` | 输出请求、响应和 ACP 调试日志 |
| `TRAE_API_SESSION_IDLE_TIMEOUT` | `720h` | session 空闲过期时间 |
| `TRAE_API_IMPLICIT_SESSION_IDLE_TIMEOUT` | `30m` | 无 session ID 请求的隐式会话空闲过期时间，设为 `0` 禁用 |
| `TRAE_API_STATE_DIR` | `$XDG_STATE_HOME/trae-api`（无则 `~/.local/state/trae-api`） | 状态目录，保存会话映射以支持重启后续聊；显式置空关闭 |
| `TRAE_API_WARM_PROCESSES` | `4` | 后台预热的空闲 ACP 进程数，设为 `0` 关闭预热 |
| `TRAE_API_MAX_SESSIONS` | `100` | 稳定 session 数量上限 |
| `TRAE_API_MAX_PROCESSES` | `100` | trae-cli ACP 进程总数上限 |

## 注意事项

- 未设置 `TRAE_API_WORKDIR` 时，工作区是隔离的临时目录，代理不会操作真实文件；任务需要访问项目文件时，务必显式设置 `TRAE_API_WORKDIR`。
- 默认以 `--yolo` 启动（自动批准工具操作），且默认只监听本机回环地址；如需对外监听必须设置 `TRAE_API_TOKEN`。仅建议在受信任的本机环境中使用。
- 每个会话独占一个 `trae-cli` 进程，首次请求需要等待进程启动和初始化，后续请求复用进程。
- 暂不支持图片等结构化消息。
