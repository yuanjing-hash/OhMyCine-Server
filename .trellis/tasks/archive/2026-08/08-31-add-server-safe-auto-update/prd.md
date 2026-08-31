# 增加 Server 安全自动更新

## Goal

让独立部署的 OhMyCine Server 能在管理端检查官方 Server Release，并由 Server 自动完成下载、校验、替换、重启、健康检查和失败回滚；整个过程不得覆盖 `.runtime` 中的数据库、配置、凭据、插件、缓存或日志。

## Background

- Server 已拆分到固定官方仓库 `yuanjing-hash/OhMyCine-Server`，Beta Release 使用 `server-vX.Y.Z` 标签。
- 官方 Beta 资产固定为 Windows x64 ZIP、Linux x64 tar.gz 和 `SHA256SUMS.txt`。
- Windows 本地运行二进制位于 `.runtime/windows/bin/ohmycine-server.exe`，运行数据位于同一 `.runtime` 下；Linux 支持独立二进制部署。
- 当前 Release 只发布 Beta；Stable 通道保留并只选择非 prerelease 的官方 Server Release，没有 Stable 时返回“暂无版本”，不能回退使用 Beta。
- 用户已确认按前述方案实施，并要求拆仓完成后直接增加 Server 自动更新。

## Requirements

### R1 官方来源与版本身份

- 更新源固定为 GitHub 仓库 `yuanjing-hash/OhMyCine-Server`，客户端不能配置任意仓库、URL 或镜像。
- 只接受 `server-vX.Y.Z` 标签、严格 `X.Y.Z` 版本和与平台精确匹配的固定资产名。
- Server Release 构建必须通过 linker 注入当前版本；开发构建明确显示 `dev`，不得把未知版本伪装成可比较版本。
- 支持 `beta` 与 `stable` 通道：Beta 可选择官方 prerelease 或 stable，Stable 只选择非 prerelease；草稿 Release 永远忽略。

### R2 有界下载与完整性校验

- GitHub API、Release 资产和重定向只允许 HTTPS 与固定 GitHub/Release Asset 主机集合。
- 所有请求具备连接/总超时、有限重定向、最大响应体和最大归档大小。
- 下载目标归档和 `SHA256SUMS.txt`，严格解析目标资产唯一校验值；SHA-256 不匹配时停止，不进入替换阶段。
- ZIP/tar.gz 解析拒绝绝对路径、`..`、符号链接/硬链接、重复目标、非预期大文件和解包越界；只提取目标 Server 二进制。

### R3 安全自替换与运行数据隔离

- 更新 staging、plan、状态和旧二进制备份只位于当前 `.runtime` 的专用 `updates/` 目录。
- 更新器只替换当前 Server 可执行文件；`.runtime` 其他内容、仓库源码、启动脚本和外部媒体路径永远不进入替换范围。
- Windows/Linux 均由 staging 中的新二进制以内部 helper 模式等待旧 PID 退出后执行替换，避免进程替换自身和 Windows 文件锁问题。
- 替换前保存旧二进制；新进程启动后必须在限定时间内通过本机 `/api/v1/health`。启动失败或健康检查失败时停止新进程、恢复旧二进制并重启旧版本。
- 任一阶段失败必须保留安全错误码和可重试状态，不得留下“已更新”假状态。

### R4 API、权限、审计与并发

- 提供 `/api/v1/system/update` 状态、检查、通道设置和安装动作，沿用标准响应 envelope 与 `Cache-Control: no-store`。
- 所有更新 API 仅允许 `system.admin`；UI 隐藏不是授权边界，service 再次校验管理员权限。
- 同一时间最多一个检查/安装事务；重复提交返回稳定冲突，不生成并行 helper。
- 通道变更、安装请求、替换成功、失败和回滚写入无敏感值审计/运行日志；API 与日志不得返回本地绝对路径、下载 URL、GitHub 响应正文或环境变量。

### R5 管理端体验

- 设置页显示当前版本、通道、最新版本、上次检查、运行状态和安全错误提示。
- 管理员可切换 Beta/Stable、立即检查和执行“下载并更新”；安装后页面自动等待 Server 恢复并重新加载状态。
- 只读安装目录、Docker/容器或部署管理型环境明确显示“由部署方式管理”，禁用安装但仍可显示当前版本；不得尝试强行替换。
- 本期不默认开启无人值守定时安装；“自动更新”指由一次管理员操作自动完成下载到回滚的完整自更新流程。

### R6 发布契约与文档

- Beta workflow 同时向 Windows/Linux 二进制注入版本，资产命名和 checksum 契约保持稳定。
- Release guard 覆盖 linker version、固定资产名和 checksum 资产。
- 更新 Server、安全设计、Web 管理端设计和路线图文档，明确自更新边界以及 Docker/只读部署行为。

## Acceptance Criteria

- [ ] 开发构建返回 `dev`，官方 Release 构建返回与 `server-vX.Y.Z` 标签一致的版本。
- [ ] Beta/Stable 选择、版本比较、草稿/错误标签/错误资产拒绝均有单元测试。
- [ ] 固定来源、重定向限制、体积限制、checksum 解析与 SHA-256 失败均有测试。
- [ ] ZIP/tar.gz 路径穿越、链接、重复文件、解包炸弹和错误二进制名被拒绝。
- [ ] helper 能在模拟安装目录中完成备份、替换、启动与健康检查；失败路径恢复旧二进制并记录 rolled_back。
- [ ] `.runtime` 内数据库、凭据、配置和任意哨兵文件在成功更新及回滚测试中保持不变。
- [ ] 更新 API 具备 auth、`system.admin`、no-store、并发冲突、安全错误映射和审计测试。
- [ ] 设置页支持通道、检查、安装和重连状态，权限、竞态、错误与响应式 UI 测试通过。
- [ ] Server Go 全量 test/vet/build/lint、WebUI permissions/test/typecheck/lint/build 和 Release workflow guard 全部通过。

## Out of Scope

- 自动更新 Player 或 Plugins。
- 从自定义 URL、第三方镜像、非官方 GitHub 仓库或本地任意包更新。
- 默认启用无人值守定时安装或在活动下载/整理过程中自行重启。
- 自动改写 Docker 镜像、Compose、systemd、NAS 套件或只读容器；这些环境只报告由部署管理。
- 回退或重写数据库 schema；本任务不新增业务数据库迁移，Release 仍需保持迁移向后兼容。
