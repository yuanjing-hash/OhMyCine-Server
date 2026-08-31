# 贡献 OhMyCine Server

感谢参与 OhMyCine Server。提交前请先阅读 [DEVELOPMENT.md](./DEVELOPMENT.md)、[Server 架构](./docs/architecture/02-server-design.md) 和涉及安全能力时的 [安全设计](./docs/architecture/07-security-design.md)。

## Issue 与仓库范围

- Server/CLI 问题提交到 [OhMyCine-Server Issues](https://github.com/yuanjing-hash/OhMyCine-Server/issues)。
- Player 问题提交到 [OhMyCine Issues](https://github.com/yuanjing-hash/OhMyCine/issues)。
- 官方插件、SDK 或 Hub 问题提交到 [OhMyCine-Plugins Issues](https://github.com/yuanjing-hash/OhMyCine-Plugins/issues)。

报告问题时请提供可脱敏的复现步骤、预期/实际结果、Server 版本、操作系统和相关日志。不得提交 Cookie、Token、Passkey、密码、签名 URL 或真实媒体路径。

## 开发流程

1. 从最新 `origin/develop` 创建 `feature/*` 或 `fix/*` 分支。
2. 保持 Handler/Service/Provider 边界，补充与回归风险匹配的测试。
3. 若 API、配置或架构改变，同步更新 OpenAPI/架构文档和 Trellis spec。
4. 执行根 Go 门禁以及 `webui/` 的测试、typecheck、lint 和 build。
5. 提交 Conventional Commit，中文描述变更原因与影响。

## Pull Request

PR 目标分支通常为 `develop`，描述至少包含：

- 变更说明和边界。
- 关联 Issue。
- 执行过的测试和结果。
- 配置、数据库迁移、安全或兼容性影响。
- UI 变更截图。

不要在普通 PR 中创建、移动或覆盖发布标签。Server Beta 由受保护的 GitHub Actions 工作流从最新 `develop` 构建。
