# Homebrew tap seed

本目录为独立 `lftk/homebrew-trae-api` tap 的种子；Formula 版本、URL 和 sha256 仍是发布占位，发布前需更新。发布、推送或改系统服务配置须经用户批准。

```bash
brew install lftk/trae-api/trae-api
brew services start trae-api
trae-api login
```

服务启动后在同一台电脑运行 login，打开官方浏览器授权；不再使用本地登录网页。服务重启需重新登录。

只支持 `TRAE_API_ADDR` 和 `TRAE_API_TOKEN`。Formula 不注入任何状态目录；凭证和对话不落盘，旧文件保持不动。升级前从旧 LaunchAgent/启动环境移除其他 TRAE_API_* 变量，否则启动会明确报错。本仓库不会替用户修改 LaunchAgent、系统环境或删除旧数据。

授权回调与 API 共用 `TRAE_API_ADDR` 端口，8765 不再单独保留。单个非回环 IP 的拒绝策略已纳入本轮地址方案，独立 QA 对提交 `b33ac385` 的技术复测通过；但尚未发布，旧版本安装不等于具备单端口能力。服务日志默认写入 Homebrew 日志目录，日志不含凭证或完整回调 URL。Formula 仍是发布占位，真实账号登录与模型调用未验证。
