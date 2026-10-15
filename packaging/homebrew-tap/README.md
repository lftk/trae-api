# Homebrew tap seed

本目录为独立 `lftk/homebrew-trae-api` tap 的种子；Formula 版本、URL 和 sha256 仍是发布占位，发布前需更新。发布、推送或改系统服务配置须经用户批准。

```bash
brew install lftk/trae-api/trae-api
brew services start trae-api
trae-api login
```

默认服务同时运行 API 与 Auth；在同一台电脑运行 login 联系 Auth 并打开官方授权。Auth 可恢复自己写出的固定凭证文件，不再要求每次重启登录；失效或刷新不确定时可能需要重新授权。

源码通过 cli 参数及环境来源配置 API 地址、Auth 地址和网关访问 Token，详见项目 README。Auth 将敏感状态持久化到固定用户配置目录，对话不落盘。Formula 不注入自定义状态目录；本仓库不会修改用户的 LaunchAgent、系统环境或清理旧数据。

当前源码默认 API 127.0.0.1:8723、Auth 127.0.0.1:8765，配置和固定 auth.json 的权限要求以项目 README 为准。Formula 仍是发布占位，旧安装包不保证包含此架构。当前开发快照独立复核未通过，见 ../../docs/KNOWN_ISSUES.md；真实账号登录、模型调用未验证。日志不得含凭证或完整回调 URL。
