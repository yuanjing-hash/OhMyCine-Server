# 修复 qBittorrent 断连监测与 OhMyCine 标签清理

## Goal

已经提交到 qBittorrent 的任务在 qBittorrent 暂时关闭后应保留原 Job 和原 torrent hash，连接恢复后直接继续监测原任务，不产生新的下载重试尝试，也不重复提交。OhMyCine 为安全提交创建的专属 qBittorrent 标签在完成身份交接后应自动删除，不在 qBittorrent 中积累无用标签。

## Background

- 活动数据库的问题任务已有 qBittorrent hash，qBittorrent 恢复后进度和速度都在更新，但 Job 因旧的 `RetryLater` 和租约计数逻辑被写成 `failed/worker_lease_expired`。
- 问题 Job 的 `attempt_count=109`，其中绝大多数是 qBittorrent 断连期间的 `downloader_unavailable` 轮询，并不是 109 次下载提交。
- qBittorrent 提交时使用唯一 `omc-<task-id>` 标签解决“上游已成功、响应丢失”的不确定性；当真实 hash 已经安全写入 DownloadTask 后，这个专属标签已完成使命。
- 当前 qBittorrent adapter 只会创建/查询标签，没有调用 qBittorrent Web API 的 `torrents/deleteTags` 清理标签定义。

## Requirements

1. 当 DownloadTask 已有真实 qBittorrent hash 时，`Get()` 的可重试断连错误必须由当前 Worker 原地等待并继续查询；不调用 `QueueService.RetryLater()`，不新增 Job attempt，不将任务显示为“重试下载”。
2. 等待 qBittorrent 恢复时必须响应 Server 停机、任务暂停和取消，不得形成无法终止的阻塞；Scheduler 的租约保活仍是 Worker 存活事实。
3. qBittorrent 恢复后必须使用原 ProviderTaskID/hash 继续获取遥测，不得再次 Submit，不得改变保存路径或已下载文件。
4. 只有尚未取得 ProviderTaskID 的提交失败，才能使用队列延迟重试；这类重试仍必须依靠稳定标签先尝试接管，避免重复 torrent。
5. Server 进程重启导致的真实 Worker 租约恢复允许重新 Claim 一次，但必须直接继续原 qBittorrent hash；普通 qBittorrent 断连轮询不能累加 Claim/attempt。
6. 存量 `failed/worker_lease_expired` 假终态若关联 DownloadTask 仍有真实 hash，必须自动恢复监测并收敛 Job/DownloadTask 错误，无需用户点击下载重试。
7. qBittorrent 明确返回认证失败、任务不存在或 provider failed 时，不得伪装成断连等待；分类、识别、转移和入库终态错误不得被清除。
8. qBittorrent adapter 必须提供可选的 OhMyCine 受管标签清理能力，仅允许删除当前 DownloadTask 持有的精确 `omc-<task-id>` 标签，绝不删除用户标签或其他任务标签。
9. 当真实 qBittorrent hash 已持久化后，Server 应删除该任务的 qBittorrent 标签定义；删除成功后持久化清理完成事实，确保同一 Worker 不重复调用。
10. 标签清理失败是可重试的维护问题，不得让下载、做种或入库失败；Server 后续重试时仍仅针对该精确 OhMyCine 标签。
11. 对数据库中仍有标签记录、但已有真实 hash 的存量 qBittorrent DownloadTask，Server 升级后应有界地补做同样的精确标签清理。

## Out of Scope

- 不为历史 115 错误入库开发自动恢复、批量搬迁或专用恢复按钮。
- 历史 115 文件由用户在 115 中手工移到对应媒体库最终目录，后续仅依赖现有生活事件或“立即增量”识别。
- 不删除 qBittorrent torrent 任务或下载数据，不批量删除无法证明属于 OhMyCine 的标签。

## Acceptance Criteria

- [x] 已有真实 hash 时，qBittorrent `Get()` 连续多次返回可重试不可用，后返回活跃任务：Job 一直保持同一次运行，`attempt_count` 不增长，Submit 调用为 0。
- [x] 断连等待期间取消 context 能立即退出，不泄漏 Worker，不伪造完成或失败。
- [x] 没有 ProviderTaskID 的提交暂时失败仍可延迟重试，且每次 Submit 前先用稳定标签接管可能已存在的 torrent。
- [x] Server 重启和存量假终态恢复均复用原 hash，不重复提交。
- [x] qBittorrent 明确 provider failed、认证失败、任务不存在以及下游终态错误保持原语义。
- [x] 真实 hash 持久化后，qBittorrent 收到一次针对精确 `omc-<task-id>` 的 `deleteTags`；用户标签不在请求中。
- [x] 标签清理失败不阻断下载，但保留后续有界重试条件；成功后不重复删除。
- [x] 存量可证明的 OhMyCine 任务标签可在升级后被有界清理。
- [x] 相关 Go 测试、`go test ./...`、`go vet ./...`、Server Web UI 测试/类型检查和嵌入构建通过。

## Key Decisions

- qBittorrent 断连后的“再次查询原 hash”是监测恢复，不是下载重试；它不创建新 Job attempt。
- 只有 Server 进程/Worker 真实中断才走队列租约恢复。
- `omc-<task-id>` 标签的唯一用途是提交幂等接管；真实 hash 已持久化后即可删除标签定义。
- 历史 115 错误文件只走手工移动 + 现有增量识别，不纳入本任务。
