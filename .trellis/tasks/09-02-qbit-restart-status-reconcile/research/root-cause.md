# qBittorrent 重启后状态矛盾根因

## 活动数据库证据

活动库：`.runtime/windows/data/ohmycine.db`。查询仅以 SQLite read-only 模式执行。

截图中的两条 qBittorrent 任务都是：

- `download_tasks.phase = downloading`
- `download_tasks.provider_status = downloading`
- 进度和下载速度有最新值
- `download_tasks.last_error_code = downloader_unavailable`
- `jobs.status = failed`
- `jobs.last_error_code = worker_lease_expired`
- `jobs.attempt_count = 109`

download 队列策略为：

- `max_attempts = 5`
- `lease_seconds = 30`

最后一次尝试之前存在大量每约 11 秒一次的 `retry_wait/downloader_unavailable`。第 109 次尝试恢复了 qBittorrent 遥测，但其 Worker 租约后续过期，队列因总尝试数早已超过 5 而把 Job 写成最终失败。

## 代码链路

- `DownloadWorker.failureRetryable()` 把临时不可用返回为 `RetryAt`。
- Scheduler 通过 `QueueService.RetryLater()` 把 Job 写为 `retry_wait`；每次再次 Claim 都会累加 `attempt_count`。
- `QueueService.RecoverExpiredLeases()` 使用总 `attempt_count >= max_attempts` 判定租约中断是否终态失败，没有区分之前的正常延迟重试和连续 Worker 崩溃。
- `DownloadWorker.persistTelemetry()` 只更新 DownloadTask，不修复关联 Job。
- `DownloadService.ListScoped()` 分别读取 DownloadTask 和 Job，Web UI 优先使用 `job_status` 显示“失败”，所以直接暴露出两层状态的矛盾。

## 修复约束

- 不能通过前端映射遮盖数据不一致。
- 不能重新 Submit 已存在 ProviderTaskID 的任务。
- 用户明确要求：已有真实 qBittorrent hash 后，断连恢复属于继续监测原任务，不是队列下载重试；普通断连轮询不应创建新 Job attempt。
- 存量错误状态需要可自动收敛，否则新逻辑只能保护新任务。
- 只能恢复临时断连/租约中断导致的假终态，不能重开真实下载、识别、转移或入库失败。
