# OhMyCine Server 开发路线图

> 标记：`[x]` 已实现并验证 · `[~]` 已实现但仍需特定环境验证 · `[ ]` 计划中

## 仓库与发布

- [x] Server 从旧 monorepo 保留历史迁移到独立 `yuanjing-hash/OhMyCine-Server`。
- [x] Go Server 模块扁平化到仓库根，`webui/` 保持嵌套 Go 模块边界。
- [x] `omc` CLI 归属 Server 仓库。
- [x] Server CI 独立执行 Go、WebUI、权限漂移和 Release Guard 门禁。
- [x] Server Beta 使用 `server-vX.Y.Z` 命名空间并只从最新远端 `develop` 发布。
- [x] 提供固定官方 Release、SHA-256 校验、原子替换、健康检查与失败回滚的 Server 管理员自更新。
- [~] 使用下一组真实官方 Beta 资产完成 Windows/Linux 跨版本升级与回滚演练。
- [ ] 增加 Stable 发布、容器镜像和 NAS 部署验证。

## 媒体流水线

- [x] Connections → Storage Destinations → Category Rules 三层模型。
- [x] Discover → Download → Transfer → Import → Notify 持久任务流水线。
- [x] 本地、115、OpenList/Alist、CloudDrive2 等来源/目标抽象。
- [x] qBittorrent、Transmission 与 115 离线下载路由能力。
- [x] 跨数据源本地暂存、刮削和目标上传；同一 115 保持云端快速路径。
- [x] 自动分类统一使用 `电影` / `电视剧` 一级目录。
- [x] 自动监听 115 生活事件、来源目录回收和定时清空回收站。
- [~] 扩展更多云盘 Provider，并完成真实账号环境的端到端验证矩阵。

## 媒体库与播放

- [x] 本地/云端媒体库扫描、目录浏览、海报墙、详情、删除和重新刮削。
- [x] 115 万级目录采用受控分页并发、128 个本地 worker、5,000 条 staging/checkpoint、原子基础目录发布与后台增量 TMDB 识别；默认日志可按 scan run 追踪阶段、媒体动作和安全错误分类。
- [x] 已有媒体库采用作品目录优先的分层识别：作品级证据固定上限，季集逐文件解析，新增集复用作品身份；`Season/SNN/第N季/Specials` 上下文支持安全裸数字集号且单集失败不降级整部剧。
- [x] 大库目录结构诊断采用持久 latest-wins 后台任务和 128 个本地规划 worker；单项异常/目标冲突分类隔离，管理端展示进度并保持“诊断 → 预览 → 明确确认 → 修复”的只读安全边界。
- [x] STRM 全量/增量同步、无效项清理和空目录收敛。
- [x] 签名 302、115 播放 lease 和 Emby 网关。
- [x] Emby/Jellyfin 刷新通知及持久 revision/outbox。
- [x] Player device Bearer API、目录、详情、搜索、播放和媒体变更长轮询。
- [x] Player 用户隔离的历史、收藏、手工合集，以及首次/后续扫库的 TMDB 自动合集对账。
- [ ] 完成更多客户端和反向代理组合的兼容性验证。
- [~] 在真实 115 大目录复验一分钟基础目录目标与账号风控退避；fixture 已验证 12,171 条基础发布低于 60 秒，Provider HTTP 并发硬上限为 32。

## 发现、搜索与追更

- [x] TMDB 海报搜索、TMDB ID 搜索和多语言标题聚合资源搜索。
- [x] 站点选择、快速全选、BT/PT 类型约束和资源 claim。
- [x] BT/RSS/Torznab/PTTime 等站点适配器与渲染器桥接。
- [x] 自动追更缺集计算、偏好过滤、搜索和正常下载流水线提交。
- [ ] 继续扩展站点适配器与风控浏览器兼容矩阵。

## 插件与管理

- [x] GitHub Registry、包校验、权限预览、WASM 安装/更新/回滚/卸载生命周期。
- [x] 在线媒体插件 Gateway、同源图片/播放、历史与进度。
- [x] 多用户、角色、权限目录、审计和结构化运行日志。
- [x] Server Web 管理端覆盖连接、媒体库、下载、整理、追更、插件、用户和设置。
- [ ] CLI 完成 Server 生命周期、媒体库、下载、插件和诊断命令。
- [ ] 按权限隔离第三方插件能力，并持续扩展可审计 Host API。

## 长期质量要求

- Player 保持独立可用；Server 只通过版本化 API 提供增强能力。
- SQLite 是默认数据库；其它数据库只作为后续可选部署形态。
- 凭据始终加密保存并从日志、API、URL 和任务摘要中剔除。
- 文件操作限制在配置根目录，STRM/删除提供明确边界与收敛行为。
- 每次功能状态变化同步更新本路线图和相应 Trellis executable spec。
