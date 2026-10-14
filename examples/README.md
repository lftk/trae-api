# Chat Completions 客户端离线接入样例

仅使用 Python 3 标准库；不需要真实 Trae 账号、不访问真实模型、不执行工具。`mock_gateway.py` 只绑定 127.0.0.1，返回合成响应；演示客户端只接受本机 `http://127.0.0.1:<port>/v1`（或 localhost）地址。两个终端在仓库根目录运行：

```bash
python3 examples/mock_gateway.py --port 18723
# 第二终端；此值仅供本地 mock，不是真实密钥
TRAE_API_TOKEN=synthetic-token python3 examples/chat_client.py --base-url http://127.0.0.1:18723/v1
python3 -m unittest discover -s examples -v
```

输出有 `nonstream`、`stream`、`final` 三步。第一次非流式读取 `choices[0].message.content` 和非标准扩展 `reasoning_content`；第二次流式按 SSE `data:` 逐片读取 `choices[0].delta`，按 `tool_calls[].index` 拼接函数参数，直到 `finish_reason` 与 `[DONE]` 均收到；第三次非流式将此前全部 `messages`、助手原始 `tool_calls`、匹配 `tool_call_id` 的 `role=tool` **固定模拟文本**一并发回。样例从未调用 `sample_lookup`，也没有声称该工具成功执行。实际应用应自行授权和执行所需工具，并把其真实输出放入后续完整历史；不要让网关执行工具。`reasoning_content` 仅作为输出展示，不重传到历史（输入保真不受保证）。

## 接口和错误处理

- 真实网关先运行 `trae-api`，另一个终端运行 `trae-api login` 完成官方授权；每次重启需重新登录。设置了 `TRAE_API_TOKEN` 的网关与客户端需使用同一 token。不要把真实 token 写到命令行/仓库中；`--token` 只用于一次性调试，建议使用环境变量。
- 网关的 `/v1/models` 返回标准 OpenAI model list；模型 ID 以该接口当时的结果为准。样例指定 `model=auto`，由网关选择；新 `__dev`/`__max` 变体的可用性与路由待后端协议完成及离线复测，不能从此 mock 推定真实账号支持。这个示例仅调用 `/v1/chat/completions`，不是 Responses API。
- HTTP 401 可能是网关 token 错误，也可能是账号未登录/失效；检查 token 后执行 `trae-api login`。其它 HTTP 状态只打印状态码，避免服务端错误文本夹带令牌或消息。连接失败和 JSON 格式异常会报错；SSE 断流、缺少 `[DONE]` 或 `finish_reason`、流内 `error` 帧均视为失败，**不得**把部分结果当成成功或自动重放。客户端关闭环境代理、拒绝重定向和自动重试，避免本机网关令牌外泄。
- 不支持图片、托管工具、结构化输出、可调 reasoning 参数等请求。实际 `tools` 需每次按需传入，工具结果也必须留在完整历史中；不要只传最后一条消息或仅传工具结果。当前示例只演示单个函数；多个并行工具要按各自稳定 index 聚合并各自回传匹配 ID。

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
