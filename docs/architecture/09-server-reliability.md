# Server 可靠性与管理闭环约定

## 任务与订阅

任务筛选、页码与 `job_id` 进入路由查询。详情按 ID 单独鉴权读取，不要求任务出现在当前列表。读取使用 AbortController 和请求归属检查，旧结果、旧错误和旧 finally 不能替换新筛选。后台更新保留列表，详情独立更新；离开页面清理请求、WebSocket 和计时器。

订阅每页 100 条，提供真实总数、前后翻页和路由状态。展开运行记录和暂停/恢复/搜索后的刷新读取当前事实；失败显示错误，不伪装成暂无记录。编辑草稿不由后台刷新覆盖。

## 实时授权

`/api/v1/jobs/events/ws` 继续只接受浏览器会话与允许的 Origin，不把凭据放入 URL。发送前重新查询实际会话和当前授权；空闲时每 5 秒检查，后台校验不延长空闲有效期。权限改变要求重新建立订阅，撤销/过期/停用关闭连接。

连接有 15 秒 ping、45 秒读期限和 5 秒写期限；客户端断开即退出处理器并取消订阅。每个订阅最多缓存 32 个事件、512 个进度节流项，慢客户端不阻塞发布。数据库查询不在事件 Hub 锁内执行。

## 已有媒体多版本

作品身份、物理文件与版本显示名不能混为一谈。已有规范作品/季集前缀后面的版本名保留，包括 Emby 风格 ` - 版本`、技术参数以及用户明确全部保留产生的编号。正确目录里的版本不报错误；错误目录仍走预览确认，移动时保留各版本及对应字幕。

需要生成新规范名称时，共享解析器保留分辨率、片源、剪辑版、HDR/DV、视频编码、位深和音频参数。相同标签或大小不等于相同内容，不能自动去重。旧命名规则的修复计划需要重新预览，不能升级后直接覆盖/回收。

Server 的 Player 版本响应增加可选 `version_name`，仅含允许的技术标签或公开条目编号。电影的 `title` 带版本标签以兼容既有选择器，作品 `display_title` 和分集标题不变。旧 Player 尚不会读取新增的分集版本字段；Emby 原有 MediaSources 接入不变。

升级不会触发全库反复诊断或修改媒体。已经保存的问题列表需管理员点击重新诊断刷新；文件处理仍必须另行预览并确认。

## 历史资格、时钟与外部图片

Server 来源的继续观看先筛选未完成且进度小于 92% 的记录，再校验当前作品存在及库/Storage/账号权限，取 limit+1 判断还有没有更多。不能从最近 100 条历史截取，也不能用“恰好满一页”猜测还有下一页。列表共享按 `client_updated_at DESC,sync_key ASC` 的有界 keyset 遍历，过滤后计算页码/总数。

浏览器账号历史可显示同账号从 Player 中转的外部来源名称、标题和受控图片，但这些项不提供 Server 播放入口；它们不进入 Server 源媒体目录和 Player Server 源总览。数据源私有地址、ID、播放 token 均不进入浏览器 DTO。

上传播放事件超过 Server 当前时间五分钟时，以 `history_clock_ahead` 拒绝该行；批次内正常兄弟行继续提交。历史中已存在的异常未来时间不擅自覆盖，返回增量携带对应 warning。旧客户端如果已经跨过该 revision，仍需后续客户端排序/异常提示适配；不能宣称服务端已修复所有旧客户端排序。

v73 为外部历史增加 `poster_asset_id/backdrop_asset_id/title_logo_asset_id` 和账号所属图片表。新 API：

- Device Bearer：`PUT /api/v1/player/history/:key/artwork/:slot`，body 是 PNG/JPEG 字节；`GET /api/v1/player/history/artwork/:id`。
- 浏览器 Session：`GET /api/v1/media-libraries/history/artwork/:id`，要求 `media_libraries.read`。
- 只接收本账号有效外部历史。每张输入/重新编码后不超过 2 MiB，1600 万像素、边长 8192；最多两个并行解码，每账号 64 MiB / 512 张逻辑配额。PNG 保留透明度，不主动抓取任何用户 URL。
- 解码在写事务外，净化字节和历史关联/revision 在同一个数据库事务提交；替换失败保留旧图。删除历史会清理图片，普通旧进度上传不能伪造或清空资产 ID。图片读取 no-store/nosniff。

`history_artwork_upload_v1` 只表示接口可用。现有 Player 尚未实现上传及按资产 revision 合并，不能以 Server 完成冒充跨设备图片已端到端上线。数据库清理属于逻辑回收，不承诺 SQLite 文件立即缩小。

## 现有页面内的收藏与合集

入口仍为 `/discovery/library`。浏览器 GET favorites、collections、collections/:id/items 带 `page/page_size` 时返回 `list,total,page,page_size,has_more`；不带分页保留旧响应形状和上限。每页 1–100，页码 1–100000，存在性及权限在计数/分页前过滤。

Session + CSRF 下新增 `PUT favorites/:itemId`、`POST collections`、`PATCH/DELETE collections/:id`、`POST collections/:id/items`、`DELETE collections/:id/items/:itemId`、`POST collections/:id/reorder`，前缀为 `/api/v1/media-libraries`。只能修改本人的手工合集，TMDB 自动合集只读；删除合集/成员不是删除文件。

重命名参数 `name,revision`，排序参数 `item_id,before_item_id,revision`；空 before 表示移到末尾。冲突返回 409，界面保留草稿并可刷新；不上传全合集重排。分页卡片按库/作品批量读取聚合和最新识别元数据，不逐项加载完整季集/文件/转移详情。

## 仪表盘与站内通知

`GET /api/v1/dashboard/operations` 为 Session + dashboard.read 的分区 read model，读取既有数据库事实，不探测 Provider。已接入媒体数量、存储最近检测、连接最近健康、任务状态、计划数量、下载采样；每区独立失败，空间/速度未知仍为 null。存储仅首 50 项并标 `has_more`；下载速率仅两分钟内新鲜样本，不标成全局实时速率。流水线/近期入库等额外仪表盘卡片保留待接入状态，不谎称整个后端不存在。

`GET /api/v1/notifications?page=1&page_size=24` 和 `POST /api/v1/notifications/:id/read`（`occurrence`）使用 Session + CSRF 和当前 jobs.read_own/all 权限。v74 receipt 持久化用户/任务/问题 occurrence/已读时间，同一任务不因进度事件堆叠通知；新的失败重新未读。已读不修改任务，恢复状态取当前任务事实，链接只指向准确 job_id；重启保留 receipt。范围限于队列失败/待处理与曾读任务的恢复，不声称已有独立连接故障推送或无限历史回放。

后台刷新每 15 秒兜底，WS 事件 250 毫秒合并，隐藏/退出/卸载停止；收到刷新中事件会补刷新。没有 Jobs 权限的仪表盘用户保留轮询，但不创建无权限任务连接。任务详情支持焦点恢复、Tab 限定在对话框和 Escape 关闭。

## 大库性能边界

已有目录原子发布保持 128 本地 worker、Provider 并发约束和一次完整事务。文件名解析移到事务外，未变条目仅批量推进代号/时间，删除旧索引和识别复用更新按 500 条分批，但仍在同一个事务提交。不能把“分批 SQL”说成“分批可见”。Provider ID/识别关联改变时仍必须完整更新。

十万条新目录的隔离并发测试证明读取不出现半成品，同时也发现长写事务阻塞历史写入和 heartbeat。这是尚未解决的验收项，不能宣称大库非阻塞。generation 隔离与短事务切换设计已获实施批准；当前新存储基础仍未接入全部业务读写，不能启用转换或换成部分目录分批发布。测试入口 `OMC_RUN_PERFORMANCE=1` 下的 `TestReadModelPerformance` / `TestPublicationContention`，全部使用临时 SQLite 与合成文件描述，无网络和实际媒体操作。

## 诊断提交、识别与修复预览

整库诊断入队后直接返回同事务建立的接收回执，进度另外读取。进度查询失败不再把已接受任务显示成提交失败；入队真正失败时也不能用第二次无归属写入覆盖以前的诊断结果。界面内展示可恢复读取错误并继续轮询，不关闭窗口。用户现场出现的 HTTP 500 尚未取得运行日志定位，不能把故障注入测试当作现场具体原因的证明。

“识别失败或无匹配”只筛选当前窗口。单项手动识别通过 `GET /api/v1/media-libraries/:id/recognitions/:token` 精确加载；保存不执行物理修复。人工识别会重建受影响的问题标识，返回原列表后通过 `POST /api/v1/media-libraries/:id/structure/selection-status` 核对已勾选 token（最多 5000 个），仅清除失效选择，保留其余跨页草稿。核对失败保留草稿、禁止预览，重试只读核对而非再次保存。

“保留全部文件（自动区分重名）”不表示已经证明每份文件内容不同。预览初始展示 50 个文件，可通过 `POST /api/v1/media-libraries/:id/structure/selection-preview/items` 分页检查全部最终名称：原名保留、改名移动或进入可恢复回收站。两项 POST 都要求浏览器 Session、CSRF、scan 权限和 no-store；确认 token 放 body，不进入 URL。文件路径仅展示受控相对路径，回收站不暴露内部物理位置。

v76 为私有修复草稿增加有界展示 JSON（8 MiB），旧草稿的原确认内容保留，缺少文件明细时须重新预览。查询明细不重建计划、不调用 Provider、不入队；最终执行仍须单独确认并复验当前来源、规则、代次和文件事实。
