# 在线插件目录缓存与 Player 系统历史

Server 自动为启用的在线插件媒体库维护分类与首屏目录。SQLite migration 117 增加 `plugin_catalogue_snapshots`、`plugin_online_media_identities`、`plugin_online_playback_receipts`，并给旧分页缓存增加 `scope_key`。迁移清理旧的临时栏目缓存，不改账号凭据或已有用户观看记录。

目录键由库/连接、实际 package SHA256、配置 JSON 和凭据版本组成；普通健康时间和连接 revision 不使目录反复失效。Runtime generation 与 active package ID 是短事务提交栅栏，阻止停止或更换来源后的旧请求写入。相同 package/config/account 的重启可复用原始目录 DTO，但新进程重签导航 token、重新注册当前 Host 海报引用。

导航新鲜期 12 小时，栏目首屏 15 分钟，最后一次成功数据最多保留 24 小时。过期但仍可用的目录立即返回，并唤醒一个后台刷新器。启动异步预热 root、第一层分类与默认栏目；flat root feed 和 hierarchical root feed 也预热。每分钟检查新启用或变更的连接，以及访问过的待刷新栏目。只维护固定缓存，不新增用户 Cron 配置。每个预热操作单独释放 WASM 调用，前台请求存在时让出，不遍历所有筛选组合。一个分类失败不阻止其他分类。关闭时先取消并等待预热结束，再关闭插件 Runtime。

单份 DTO 最多 512 KiB；快照最多每库 128 条、全局 4096 条与 48 MiB；分页缓存最多每库 128 条、全局 512 条与 16 MiB，总正文额度 64 MiB。最多 32 个不同目录刷新 flight，相同 key 合并；首次失败和已有快照的后台失败都有限额退避，1–32 分钟。失败不写入返回正文，不覆盖最后有效目录。私密或 URL 形态分页 cursor 不落盘，因此仅带这种 cursor 的旧快照不提供后续分页。

缓存只保存稳定公开的目录字段、可公开的无 query HTTPS 图片描述和 opaque 公共身份。禁止保存 Cookie、账号、会员权益、签名播放地址、带凭据 query 的图片、Host 临时 ref 或签名导航 token。每次缓存命中仍检查当前用户的在线库读取权限及精确启用的库、连接、插件包；海报重新进入现有 Host 域名、授权、公网 DNS、重定向与 MIME 验证。物理媒体库的正整数 resource policy 不表示插件虚拟 UUID；这里沿用既有在线访问边界，不重解释该政策。播放和本地下载仍每次向插件实时验证具体视频权益，目录缓存不能放行播放。

生产绑定 `PlayerHistoryService.SetPluginService` 后，bootstrap 增加 `online_playback_history_v1`，在线媒体库摘要增加 `systemHistorySupported: true`。未绑定时不宣称该能力。新版 Player 对旧 Server 的缺省值为 false，除非插件已声明上游 `playback.progress_sync`，不会反复请求不支持的上游接口。

`POST /api/v1/player/online-libraries/:libraryId/items/:workId/progress` 先保存当前系统用户的观看记录。插件声明 `playback.progress_sync` 时才另调用上游；上游失败不撤销本地系统事实。没有该能力仍返回兼容的 `{ "accepted": true, "remote": false }`。该 ACK 不遍历整个历史或请求全部海报；同步增量由既有 `/player/history/sync` 提供。

身份严格使用 `online-version|encodeURIComponent(libraryId)|encodeURIComponent(workId)|encodeURIComponent(segmentId)|encodeURIComponent(versionId)`，canonical sync key 是该完整 token 的 SHA256。Server 在当前来源 scope 下用权威 detail 校验具体分集和版本，并覆盖客户端标题、海报和集数猜测；空 variants 可由实时播放再解析。未提供官方集数的彩蛋不生成普通 episodeNumber，版本与彩蛋身份不合并。可选 `MediaWork.defaultSegmentId` 必须精确属于 segments，并不重排官方顺序。

零进度 `started` 必须已有该用户成功获得的、当前 scope 且 Host 资产归属有效的实时播放方案。失败播放不能产生观看记录；正进度加有效 detail 允许已经在本地离线播放的记录同步。detail proof 24 小时有效，最多每库 4000 / 全局 16000 条；播放开始凭证 24 小时，最多每用户 4096 / 全局 16384 条。它们只证明身份或成功开始，不代表未来播放权益。用户历史比临时 proof 长寿；升级、重新登录或 proof 淘汰不会删除用户完成状态与续播位置。已有本人 canonical 记录的 tombstone 在当前在线读取 scope 下不依赖 provider 可用或新的 detail proof；凭空删除其他身份被拒绝。

Player `/player/history?source_kind=server` 与 overview/ContinueWatching 返回当前用户可见的精确在线身份；每次显示重新授权启用 scope，海报保留无签名公开描述并重新注册受保护 Host 引用，整个列表投影共享 5 秒预算且不在 SQLite 事务内联网。物理浏览器 `/media-libraries/history` 保持原有数值物理库 DTO，不插入虚假的在线物理条目。旧 tombstone、时间冲突规则、用户隔离与 Server A/B 配置归属沿用原历史协议。

插件错误的可选 `reason` 只接受以下固定枚举：`entitlement-required`、`region-restricted`、`drm-unsupported`、`quality-unavailable`、`incomplete-stream`、`asset-domain-denied`、`network-access-denied`、`download-unavailable`。Server 用固定提示分类，不转发 provider message。日志只保留闭集 code/reason、operation 与内部 scope 标识，拒绝未知原始文本、Cookie、URL query 和响应正文。Server 1.1.74 的 detail 与错误 envelope 非 strict 解码容忍新增字段；Mango 在严格 feed DTO 中不输出 `defaultSegmentId`，插件合同 floor 仍为 0.1.1。
