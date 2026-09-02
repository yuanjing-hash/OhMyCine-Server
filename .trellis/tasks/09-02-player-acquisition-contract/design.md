# Design

## API

- `GET /api/v1/player/discovery/acquisitions?page=1&page_size=30`
- `POST /api/v1/player/discovery/downloads` 新增可选 `expected_tmdb_id`、`expected_media_type`。

列表由 `AcquisitionService.List` 查询当前 owner，逐项复用 download/transfer/follow 持久化状态重建 durable projection，标题取关联下载任务或追更订阅的安全展示字段。目标库 ID 继续执行资源授权过滤；下载百分比、字节、速度、ETA 与传输文件进度只来自安全投影。

## Identity Guard

`SiteService.BindExpectedIdentity` 在 token 仍可用且归属当前 actor 时通过 TMDB `IdentitySearchNames` 获取权威名字，复用 `mediaIdentityResultMatches` 检查 claim 标题，然后把 claim 标成 `direct_id/verified`。Handler 在调用 `Download` 之前完成该步骤；任何失败都发生在 claim reserve、种子获取和下载器提交之前。

## Compatibility and Security

- 新字段可选，旧客户端合同不变。
- result token 仍是 actor-bound opaque claim，Server 不接受站点 ID、torrent ID 或 URL 作为身份替代。
- 列表只返回当前 owner 且经过媒体库 resource scope 过滤的字段。
