# Implementation Plan

- [x] **建立基线与迁移**：补齐权限、acquisition、scheduler、structure run 所需模型与 SQLite 迁移，为旧角色和旧定时设置编写幂等迁移测试。
- [x] **授权核心**：实现统一 Authorizer、用户 allow/deny、资源 scope、模板复制和动态 Player capabilities，更新权限目录、路由、领域服务、Web UI 与测试。
- [x] **搜索协调器与进度协议**：抽取 Server 级共享并发器、每站 timeout、取消和稳定结果聚合，让标题与 MediaIdentity 共用；扩展 SSE、保留 JSON，更新契约文档和测试。
- [x] **Acquisition 投影**：建立下载/transfer/follow 的幂等阶段更新，提供 Player 安全查询接口，并把下载、订阅接入冻结配置与状态记录。
- [x] **目录诊断/修复**：拆分 read/diagnose/preview/repair/runs；修复 move 结算、旧 managed artifact、空目录、未托管残留和 Windows 错误分类，覆盖本地与 115。
- [x] **统一 Cron**：实现五段 parser/preview、definition/run、队列投递与策略；迁移现有定时项，增加目录诊断/可选修复，完成统一 UI。
- [x] **集成与回归**：更新架构与 specs，完成 Server Go/WebUI 全量质量门，验证未授权、部分失败、重启恢复、修复残留和旧计划不双触发。

## Validation

- `go test ./...`
- `go vet ./...`
- `go mod verify`
- `go build ./cmd/server`
- `go build -tags webui ./cmd/server`
- WebUI permissions check、189 tests、typecheck、lint、build、嵌入 Go module tests
