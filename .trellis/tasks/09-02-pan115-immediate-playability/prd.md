# 115 媒体即时可播放与 STRM 立即调度

## Goal

让已进入 115 媒体库且已被 Server 建立目录条目的视频立即可被 Player 播放，并让用户主动请求 STRM 时立即进入生成链路，而不依赖下一次扫描或等待同媒体库的目录修复完成。

## Requirements

- 115 媒体库条目本身是 Player 播放的持久事实；是否启用或是否已经生成本地 STRM 投影不能决定条目是否可播放。
- Player 的受保护播放接口必须继续执行用户权限、媒体库权限、Storage/Connection 可用性、稳定 provider item 身份和媒体库根边界校验，再签发临时 115 直链。
- Player 目录详情应在条目具备可用 115 provider 身份时立即返回可播放版本和稳定的 Server entry identity，不查询或依赖 `media_artifacts`。
- 用户点击 STRM 增量/全量刷新后，应立即为当前已提交的媒体库 generation 请求媒体产物生成，同时保留后台扫描以收敛刚发生但尚未入索引的变化。
- STRM 扫描与媒体产物生成不得因同媒体库的长时间目录修复 Job 共用资源并发键而被无关阻塞；各自仍需维持本域内的每库串行/合并语义。
- 自动媒体库扫描新增、更新或删除内容后仍必须调度对应 generation 的媒体产物，测试覆盖 115 新条目生成链路。
- 不修改现有运行数据库或删除用户任务；代码升级后由正常队列收敛。

## Acceptance Criteria

- [x] 关闭 STRM 投影的 115 媒体库中，已索引且有 provider item ID 的条目在 Player 详情中仍为 `playable=true`。
- [x] 上述版本通过受保护的 `/api/v1/player/media-entries/:id/stream` 返回临时 115 直链，不需要 `media_artifacts` 行。
- [x] 无权限、禁用媒体库/Storage、缺少 Connection/provider item、条目已离开媒体库根边界时仍拒绝播放。
- [x] 点击 STRM 刷新会先调度当前 catalog generation，再排队执行增量/全量扫描；目录修复运行时 STRM/产物 Job 使用独立资源键，可被调度器领取。
- [x] 扫描提交新增 115 条目后存在对应 artifact run，回归测试不只测试纯 helper。
- [x] 聚焦测试、`go test ./...`、`go vet ./...` 与 `git diff --check` 通过。

## Notes

- STRM 是供 Emby/Jellyfin/文件系统消费的可选本地投影；Player 使用已认证 Server entry 播放端点，不把本地投影当作播放授权记录。
