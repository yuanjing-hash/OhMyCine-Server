# 设计：115 即时播放与 STRM 立即调度

## 边界拆分

`media_library_entries` 是扫描后已确认存在的媒体文件事实，也是 Player 的播放来源。`media_artifacts(kind=strm)` 是可选的文件系统投影，继续服务 STRM 文件、Emby/Jellyfin 网关及其清理生命周期，但不再作为 Player 详情的可播放门槛。

Player 的 Bearer 端点已经完成用户和媒体库授权。115 播放时根据 entry -> library -> storage -> connection 解析稳定 provider item，实时校验文件仍位于媒体库 provider root 下，然后复用现有 115 多设备播放协调器和临时直链生成逻辑。播放 identity 使用稳定、非敏感的 Server entry identity，禁止返回 provider ID。

## 调度

主动 STRM 请求分两步：先对当前 `baseline_generation` 调用媒体产物调度，使已经入索引的内容无需再等一次 115 扫描；再排队执行原有增量/全量扫描，扫描后的新 generation 继续通过正常逻辑合并到最新产物任务。

目录修复、STRM 扫描和媒体产物属于不同工作域。资源并发键分别使用结构修复、STRM reconcile、artifact 命名空间，避免队列把它们误当作同一种互斥资源；每个 Job 类型内部仍以 library ID 合并并限制并发。扫描/修复的 generation CAS 和 artifact policy 校验负责最终收敛。

## 兼容与安全

- 现有 STRM 签名 URL、artifact opaque ID 和 Emby 网关保持不变。
- 只有 Player 已认证 entry 流端点新增无 artifact 的直连解析。
- provider item 必须是文件、Connection 支持临时直链，且当前父目录仍在媒体库 provider root 内。
- 不新增包含 provider ID 的公开 DTO、日志或审计字段。
- 旧队列记录继续按旧资源键完成；新请求使用新资源键。

## 回滚

代码回滚后 Player 恢复依赖 STRM artifact；没有数据库迁移，因此不需要数据回滚。
