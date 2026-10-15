# Chat Completions 客户端离线接入样例

仅用 Python 3 标准库和本机 mock；不登录真实账号、不调用模型、不执行工具。仓库根目录开两个终端：

```bash
python3 examples/mock_gateway.py --port 18723
# 另一个终端（synthetic-token 仅供 mock 测试）
TRAE_API_TOKEN=synthetic-token python3 examples/chat_client.py --base-url http://127.0.0.1:18723/v1
python3 -m unittest discover -s examples -v
```

输出依次为 `nonstream`、`stream`、`final`。非流式读 `message` 和扩展字段 `reasoning_content`；流式按 `tool_calls[].index` 拼参数，必须收到 `finish_reason` 与 `[DONE]`。末轮重发完整 `messages`，包括助手 `tool_calls` 和匹配 ID 的 `role=tool` **固定模拟文本**；`sample_lookup` 从未真正执行。真实工具须由客户端自行授权执行并回传结果。推理内容只展示、不回传（输入保真不保证）。

## 接口和错误处理

- 真实网关的启动、登录、配置和模型 ID 规则见 [项目 README](../README.md)。示例仅调用 Chat，使用 `model=auto`；mock 不能证明某账号支持特定模型。
- HTTP 401 可能是网关 Token 错误或尚未登录。示例只显示状态码，不打印服务端错误正文；关闭环境代理、拒绝重定向和自动重试，避免泄露 Token。断流、流内错误或缺少完成标记均报错，部分输出不算成功。
- 每轮需要完整历史和所需 `tools`；并行工具各自按 index 聚合并回传匹配 ID。示例只演示一个函数。

## 本机负面场景

```bash
# 以 synthetic-token 启动 mock 后，错误 token：HTTP 401
TRAE_API_TOKEN=wrong python3 examples/chat_client.py --base-url http://127.0.0.1:18723/v1
# 停掉旧 mock，带 --logged-out 重启：HTTP 401（账号需要登录）
python3 examples/mock_gateway.py --port 18723 --logged-out
# 停掉旧 mock，带 --disconnect 重启：SSE 缺少完成标记，退出码非零
python3 examples/mock_gateway.py --port 18723 --disconnect
```

独立 mock 只验证客户端接线，不代表实际登录、设备签名、模型发现、真实推理或后端新模型路由已经验证。
