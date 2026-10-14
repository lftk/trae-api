# TRAE-004 CN 协议差异（离线证据）

固定参考：magpie-community/plugins `9cfc18ae5294fac686ae360f3d72de450ef52e0e`，`packages/trae/index.mjs`。本文件描述实现依据，不代表真实账号或线上服务验证。

- `index.mjs:290-304, 620-689`：登录开始生成账号生命周期固定的 19 位数字 deviceId、16 字节十六进制 machineId 与 P-256 密钥；回调保留同一设备。原实现每次登录随机 UUID，无设备密钥。新密钥只在进程内，退出或注销即消失。
- `index.mjs:484-569`：续期首选 `/trae/api/v3/oauth/ExchangeToken`，签名输入为 `POST\n路径\nClientID\nRefreshToken\nUnix秒\n16字节hex nonce`，ECDSA SHA-256 DER/base64，公钥 SPKI PEM。SOLO_PC `en1oxy7wnw8j9n` / `0.1.64`，IDE_PC `ono9krqynydwx5` / `3.3.99`。参考插件任意 bound 失败后退到旧 `/cloudide/…`；这里**仅旧账号完全没有设备材料**走 legacy；材料不完整时拒绝，已绑定账号拒绝后不降级，避免令牌失去设备绑定。服务不持久化，因此正常新登录都走设备证明。
- `index.mjs:2363-2493`：模型先 POST `batch_get_detail_param`，无可用聊天分组时逐 function POST `get_detail_param`；从 `chat_v3`、`solo_work_lite`、`solo_agent`、`solo_agent_lite` 合并、按 config_name 去重。过滤非 chat、禁用、自定义槽位；优先带 `__dev` 的 function；有更大上下文的 `__max` 暴露为 `-max`。切换账号时旧发现结果不得发布到新账号缓存。旧 `/api/remote/v1/models` 不再使用。当前不复制参考插件的复杂名称冲突评分规则；具体同名误标需真实响应样本再补回归。
- `index.mjs:53-67, 704-739, 894-925`：模型网关 header 使用 SOLO CN `0.1.69` / `20260917`；请求使用发现所得 `function`、`model_name`，Max 用基本 `config_name`，输出上限同时约束于模型最大输出和上下文窗口，`user_message_context.model_info.prompt_max_tokens` 不为负。显式模型在开始流式输出前完成可用性校验。保持请求独立 ID，不复制插件稳定 conversationID。
- `index.mjs:425` 的 CN 登录失效码包含字符串/数字 `401`；模型发现 HTTP 200 + `code:401` 与 `1001` 一样进入现有一次受限续期，不让认证拒绝误入 detail fallback。
- `index.mjs:557-569, 901-907` 的持久化和会话语义不适用于本服务：单进程内存账号、客户端完整历史，认证拒绝且尚未输出时仅续期重试一次；网络或业务失败不重放。`/v1/models` 仍输出标准 OpenAI 对象，不公开设备密钥或内部 route 元数据。

验证使用 httptest 合成凭证：`trae004_test.go`、`trae004_more_test.go`、`trae004_fix_test.go`；旧发现 fixture 更新到 batch 格式，保留原断言。真实设备登录、实际模型列表及付费调用均未验证。
