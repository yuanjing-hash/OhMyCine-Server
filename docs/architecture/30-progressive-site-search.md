# 慢速站点搜索与渐进结果

Server 网页、Player 入库及自动追更共用 SiteService。适配器仍在原有请求边界整体返回一页；每个已经完成、校验的名称查询是自然增量批次，不增加索引器fanout、自动翻页或其他来源请求。

## 搜索策略

迁移118仅增加可空的 `sites.search_timeout_seconds`，原 `timeout_seconds` 继续用于连接检测和下载。历史12秒配置不被覆盖，Torznab自动使用120秒搜索默认。管理DTO同时返回 `search_timeout_seconds` 和 `effective_search_timeout_seconds`；创建时空值/0表示自动，PATCH省略保留、显式0清除。显式值范围3–180秒。

| 边界 | 默认/上限 |
| --- | --- |
| Torznab单次逻辑查询 | 120秒；404/405回退共用剩余预算 |
| 共享限速等待 | 最多20秒，随后才开始新的查询预算 |
| Torznab同站聚合 | 300秒 |
| 其他原生/插件同站聚合 | 180秒；原生请求默认仍取原配置 |
| 交互搜索整轮 | 360秒，包含元数据准备及并发排队 |
| Follow搜索阶段 | 10分钟，保留租约保活和提交幂等 |
| 搜索HTTP响应/写入 | 390秒，普通API超时不变 |

子预算继承请求取消；不使用后台context继续已取消搜索。限速预测不能放行时不发送请求，也不通过重试重置共享桶。

## 安全的累计快照

每批是同站完整累计快照，按私有来源身份去重，不只按标题。不变条目的opaque token保持稳定。115较新帖子可以替换卡片，但不得删除已发布旧token或更换其分享凭据；已打开操作继续冻结原token，保留原15分钟TTL、撤权、预留/消费与提交复核规则。claim容量仍为5000；空间不足时明确标记不完整，不驱逐已发布或正在使用的claim。

JSON和Follow每站只消费最终组。正常完成过至少一个查询（包括真实空结果）后，晚期失败保留`status=success`及结果，最终附`partial=true`和`incomplete_reason`；全部查询未成功时为明确错误。空结果加未完成查询不是权威零命中。

## HTTP / SSE契约

沿用 `torrent-search`、`pt-search` 及 `media/{movie|tv}/{tmdb_id}/torrent-search` 的JSON和`/stream`路径；Player/网页共用权限与Handler。精确站点范围最多64个，不因失败扩大范围。

新增可选query `progress_version=2`。省略或1保持每站一次最终`site`，避免旧客户端错误累计；未知显式版本在流开始前拒绝。新客户端连接旧Server时，缺少`complete`的组按旧式最终结果处理，不自动复发JSON。

仍只有 `media/progress/site/done/error` 五种事件。10秒标准SSE注释心跳表示连接活动，不是业务结果。事件和心跳共用序列化writer，写入/flush失败取消上游。

v2 `site`增加：

```json
{
  "site_id": 6, "site_name": "Jackett", "site_type": "bt",
  "status": "success", "page": 1, "items": [],
  "progress_version": 2, "complete": false, "revision": 7,
  "queries_completed": 1, "queries_failed": 0, "queries_total": 3
}
```

`items`仍是原安全结果DTO，只有opaque token，没有私有来源ID/凭据。示例空数组只说明结构。

- `complete=false`为运行中累计快照；最终true只代表执行终结，不保证全部成功。
- `queries_completed`计格式有效的正常查询，含空结果；`queries_failed`独立计实际已尝试失败的逻辑查询；未尝试不计入二者。
- `revision`在同一搜索/站点递增。客户端还须按请求generation、站点和页隔离迟到事件。

`progress`保留 `total/pending/running/completed/succeeded/failed/result_count`，增加单调`event_sequence`及可选`site_detail`：

```json
{
  "total": 1, "pending": 0, "running": 1,
  "completed": 0, "succeeded": 0, "failed": 0, "result_count": 18,
  "progress_version": 2, "event_sequence": 9,
  "site_detail": {
    "site_id": 6, "site_name": "Jackett", "phase": "querying",
    "query_name": "Seven Samurai", "query_index": 2,
    "queries_completed": 1, "queries_failed": 0, "queries_total": 3,
    "result_count": 18, "elapsed_ms": 2400, "revision": 8
  }
}
```

阶段为 `preparing_media/queued/preparing_site/restoring_session/checking_session/waiting_rate_limit/querying/processing_results/finished`。元数据准备可只有根phase和未知站点总数。上游只能整批返回时只显示实际阶段、耗时，不伪造百分比或逐条数据。

始终满足 `total=pending+running+completed`、`completed=succeeded+failed`。站点只在最终累计一次；部分可用组属于succeeded。结果总数取各站最新累计条数，不叠加批次。done复用最后进度表示编排结束；error/断流保留已收到结果并标记不完整。

## 有界结果与错误

v2单事件最多384KiB、整流最多12MiB，其中4MiB预留最终快照/摘要。新行和替换行先按最终字节准入，触限为 `result_limit` 并保留已发条目；重复中间快照可以省略。用尽中间快照预算后仍发送小心跳，计入预留余量和整流硬上限，避免剩余慢查询被客户端当作空闲断流。不得截断JSON、放大Player的512KiB/16MiB硬限，或无声裁剪JSON/Follow结果。

区分 `request_timeout/site_budget_exceeded/rate_limit_wait_exceeded/search_budget_exceeded/cancelled/result_limit` 和既有认证/解析错误。诊断不包含完整URL、API Key、Cookie、原始响应或任意provider错误文本。

Follow可以用已证实的部分候选正常选路；无候选且搜索有失败/未完成时保存 `failed/follow_search_incomplete`，不保存no_match。已创建任务不换路由、不自动重投。

反向代理仍可能有独立绝对超时/缓冲。心跳和禁缓冲头不保证绕过它；生产代理修改、重启、发布及真实下载/转存须分别授权验收。
