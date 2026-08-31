# OhMyCine Server 开发规范

## 仓库边界

本仓库只负责 OhMyCine Server 和 `omc` CLI。Player 位于 [OhMyCine](https://github.com/yuanjing-hash/OhMyCine)，官方插件、Plugin SDK 与 Hub 位于 [OhMyCine-Plugins](https://github.com/yuanjing-hash/OhMyCine-Plugins)。仓库之间通过版本化 API、Registry 和 Release 产物协作，不使用相对源码依赖。

```text
OhMyCine-Server/
├── cmd/server/       Server 入口
├── internal/         私有业务实现
├── pkg/              Provider、下载器、媒体服务器和元数据客户端
├── webui/            Vue 管理端与嵌入适配模块
├── cli/              omc CLI
├── docs/             Server/CLI 架构文档
└── .github/          Server/CLI CI 与发布流程
```

## 分支和提交

- `develop`：日常集成和 Beta 来源。
- `main`：Stable 来源。
- 功能和修复从最新 `origin/develop` 开始，验证后合回 `develop`。
- Commit 使用 `<type>(<scope>): <中文描述>`，例如 `fix(server): 修复下载任务恢复`。
- Server 发布标签固定为 `server-vX.Y.Z`。

## 后端约束

- Go 1.23+、Gin、GORM、SQLite、zerolog。
- Handler 只做输入、认证上下文和响应转换；业务逻辑放在 Service。
- 外部调用和长任务接收 `context.Context` 并设置明确超时。
- Provider、网盘、下载器、媒体服务器和站点差异封装在接口实现内。
- 凭据使用 AES-GCM envelope 加密；日志、API、URL 和任务摘要不得泄露秘密。
- 文件移动、清理、STRM 和上传必须限制在配置根目录内。
- API 使用 `/api/v1/`，普通响应使用 `{code,message,data}`。

## Web 管理端约束

- Vue 3 Composition API、`<script setup>`、TypeScript、Pinia、UnoCSS。
- 权限常量由 `internal/authz/catalog.json` 生成到 `webui/src/auth/generated-permissions.ts`。
- 浏览器存储不得保存 Cookie、Token、签名直链、Provider Header 或本地绝对路径。
- API 类型、Go DTO 和交互状态需要端到端保持一致。

## 本地验证

从仓库根目录运行：

```powershell
go mod verify
go test ./...
go vet ./...
go build ./cmd/server

cd webui
npm ci
npm run permissions:check
npm run test
npm run typecheck
npm run lint
npm run build
go mod verify
go test .
```

Windows 完整隔离门禁使用 `./test.ps1`。测试必须使用独立临时数据库，不得删除现有 `.runtime` 数据。

## 发布

Server Beta 只能从最新远端 `develop` 手工触发 `.github/workflows/server-beta-release.yml`。Action 构建嵌入 WebUI 的 Windows/Linux 二进制、校验 SHA-256，并发布 `server-vX.Y.Z` prerelease。旧 monorepo Release 仅用于历史下载，新版本只从本仓库发布。
