# trae-api

单账号 Trae CN → OpenAI Chat Completions 网关。API 与 Auth 职责独立，默认同进程运行；`login` 可直接启动所需 Auth，无须先启动另一命令。Auth 维护固定凭证文件，API 只读访问快照。对话无状态，不执行客户端工具。

> 当前为按用户指示合并的开发快照，独立复核未通过。刷新取消和发送前栅栏写失败仍有已知边界问题，见 [已知问题](docs/KNOWN_ISSUES.md)；不得把合并或测试绿灯当作验收通过。

## 启动

Go 1.25+，在仓库根目录：

```bash
go build -o trae-api .
# 全新机器：终端一直接授权，成功后保持这个进程在前台执行续期
./trae-api login
# 终端二：启动只读凭证快照的 API
./trae-api api
```

或者终端一直接运行 `./trae-api`（API 与 Auth 同进程），再在终端二运行 `./trae-api login`；后者验证并复用已有 Auth，授权结束即可退出。查看命令：`./trae-api --help`、`./trae-api login --help`。

命令分发、参数、帮助和版本由 `urfave/cli/v3` 处理。API 默认 `127.0.0.1:8723`，Auth 默认 `127.0.0.1:8765`；回调使用 Auth 实际绑定的 loopback 地址，不通过 API。无独立 `auth` 命令或别名。`login` 若自行启动 Auth，授权完成后仍占用当前终端并负责续期，Ctrl+C 停止后 API-only 只能读取现有令牌直到失效；如默认进程已运行 Auth，`login` 经本机实例校验后安全复用并在授权后退出。端口由其他程序占用会明确报错，不会把普通健康页当作 Auth；不创建后台守护进程。Auth 从有效凭证状态恢复，过期、失效或刷新状态不确定时可能需要重新登录。尚未发布安装包，不保证旧 Homebrew 包含当前源码行为。

## 配置

参数由 CLI 库读取，以各子命令 `--help` 为准：

- `--api-addr` / `TRAE_API_ADDR`：API 地址，默认 `127.0.0.1:8723`，非回环监听必须配置网关 Token。
- `--auth-addr` / `TRAE_API_AUTH_ADDR`：Auth 回环地址，默认 `127.0.0.1:8765`，供默认进程和 `login` 自启或安全复用使用。
- `--api-token`：网关 Token 参数；真实秘密优先从 `TRAE_API_TOKEN` 环境变量读取，避免出现在进程参数中。
- `TRAE_API_TOKEN`：默认空；设置后 API 请求需 `Authorization: Bearer <gateway-token>`。login 只联系本机 Auth，不发送此网关 Token。

网关访问 Token 与 auth.json 中的 Trae 上游凭证不同。API 不再提供 Auth 管理或回调路由；Auth 独立检查 loopback Host/来源、管理请求同源和一次性授权。浏览器回调不携带网关 Token。模型/认证协议地址、客户端版本、并发和超时不开放配置。

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

设置 Token 时另加 Bearer 头。**每次请求传完整 messages 和所需 tools**；工具由客户端执行并回传匹配 ID 的结果，网关不保存历史、不执行工具。reasoning_content 仅保证输出。图片、托管工具、结构化输出、可调 reasoning 参数等不支持。API 对上游鉴权拒绝不续期或重放；续期由独立 Auth 负责。

**本机离线接入演示**：[examples/README.md](examples/README.md)（Python 标准库 + 本地 mock，含流式、推理字段、工具调用和错误场景）。

## 凭证文件与旧数据

Auth 唯一写入 `filepath.Join(os.UserConfigDir(), "trae-api", "auth.json")`，目录 0700、文件 0600。文件包含敏感凭证及续期状态，不要提交或分享。API 逐请求只读访问 Token、有效期及必要标识；续期仅在 Auth 中发生。原子更新、重启恢复和故障限制见 [凭证契约](docs/CREDENTIALS.md)。测试必须隔离配置目录，macOS 需独立 HOME。既有 credential/history/session 文件及备份不自动迁移或清理，对话仍不落盘。

## 开发验证

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o trae-api .
```

所有测试使用合成凭证与本机模拟服务，不调用真实模型，不执行真实浏览器授权；离线通过不代表实际上游或真实登录已验证。协议来源及许可证见 [REFERENCES.md](REFERENCES.md)。
