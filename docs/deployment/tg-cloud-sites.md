# 网盘分享站

在 **站点 → 添加站点 → 网盘分享站** 中创建站点。下一步先选择站点分类，当前支持 **盘搜（PanSou）**，再填写服务地址和 TG 频道。一个站点可以聚合最多 100 个公开 TG 频道，也可以创建多个站点分别管理电影、剧集等来源。当前网盘类型支持 **115**。

## 配置

1. 选择 **盘搜（PanSou）**。新建时服务地址默认填写 `https://so.252035.xyz`，与参考的 [115 自动追更插件](https://github.com/mrtian2016/MoviePilot-Plugins/blob/5b03087b1121c43f9739472fb6dfbba67279dae8/plugins.v2/p115strgmsub/__init__.py) 一致；可改为 Server 能访问的其他 PanSou HTTPS 根地址，不要填写 `/api/search`。编辑已有站点时保留其保存的地址。Server 只预填地址，不会部署或托管 PanSou。
2. 选择 115，填写频道名称、`@channel_name` 或 `https://t.me/channel_name`，每行一个。重复频道会自动合并。不支持私密群邀请链接，也不需要向 Server 提供 Telegram 登录会话。
3. 如果 PanSou 开启账号认证，勾选认证并填写用户名、密码。凭据在 Server 中加密保存；编辑时密码留空保留原值，关闭认证会清除已存凭据。
4. 保存会实际测试一次搜索，失败则保留原配置。当前沿用站点 3–30 秒超时范围，TG 站点默认 30 秒；PanSou 本身需要具备访问配置频道的网络条件。
5. 在下载器中配置支持分享转存的 115 下载器，选定网盘账号和接收目录。站点不另设账号、下载目录或执行节点。

## 搜索与追更

保存后该站点会进入统一搜索的站点列表。已有搜索选择会保留，请在选择站点时勾选新增站点。结果展示频道来源和可用的 TG 原帖链接；点击 **转存入库** 后选择 115 下载器与最终媒体库。分享地址和提取码保存在 Server 的短期结果凭证中，不直接返回浏览器。

订阅编辑时选择 115 下载器，并勾选对应网盘分享站。订阅继续按已有计划搜索、筛选明确缺失的集数，再执行分享转存和入库。网盘结果没有做种人数和 PT 免费促销属性，这两类筛选不作用于分享结果；清晰度、关键词、发布时间等其他条件仍生效。设置大小限制时，缺少可信大小信息的结果仍不会自动通过。

同一分享被多次发布时优先采用时间较新的有效资源标题。订阅的提交去重包含本轮缺失集数，因此同一分享追加新集后可以再次转存；没有明确季集信息的资源不会猜测集数自动提交。

这是按需搜索与已有订阅定时搜索，不会监听 TG 新消息。搜索质量、公开频道可访问性和分享有效性取决于配置的 PanSou 服务及上游资源。

## API 契约（v1 新增字段）

沿用 `/api/v1/sites` 创建、`/api/v1/sites/{id}` 更新和统一 discovery 搜索 / downloads 接口。创建请求示例：

```json
{
  "kind": "pansou_tg",
  "name": "115 剧集频道",
  "base_url": "https://pansou.example.com",
  "enabled": true,
  "priority": 100,
  "timeout_seconds": 30,
  "rate_limit_per_minute": 12,
  "cloud_config": {
    "provider": "115",
    "channels": ["channel_name"],
    "auth_enabled": false
  }
}
```

启用认证时增加 `username` 和 `password`。更新必须携带当前 `revision`，未传 `cloud_config` 保留原配置，传入时整体替换；密码缺省或空字符串保留，`clear_password: true` 显式清除。启用认证时不能保存空凭据。

站点摘要新增 `site_type: cloud_share`、`cloud_config`、`login_username`、`password_configured`。安全搜索结果新增 `source_kind: 115_share`、`cloud_provider: 115`、`channel`、可选 `post_url`，继续使用 actor 绑定的短期 `token`。站点配置版本变化后需重新搜索，旧 token 不可提交。

手动和订阅路由预览采用 `source_kind: 115_share`。服务端在提交时根据已保存的站点、结果凭证、115 下载器能力再次验证，拒绝普通 URL、磁力或种子来源，不可通过更改浏览器参数改走 qBittorrent。

协议参考：MoviePilot-Plugins 的 `plugins.v2/p115strgmsub/clients/pansou.py` 和 [PanSou 官方协议](https://github.com/fish2018/pansou)。本实现使用原生 Server 适配器，没有引入插件执行或消息采集任务。


## 预览分享与选择入库

搜索结果的「检测」左侧增加「预览分享链接内部内容」。使用已配置的 115 下载器账号读取分享目录，显示文件、子目录、各项大小和合计；预览不会转存任何文件。可见视频会按实际文件名及父目录预先识别，同时最多两项检测请求。

支持勾选文件或整个目录，以及全选、清空、仅选视频。点击「仅将所选内容转存入库」后，沿用现有媒体库路线确认。只转存勾选文件，保留其原相对目录；每个视频创建独立任务，匹配字幕等附属文件归入该视频任务。这样合集中的影片不会受原电影下载包“只取最大视频”的规则影响，单个任务失败也不会挡住其余影片。非媒体附件可以转存，但自动整理仍只接纳可信媒体及匹配附属文件。

- 预览有效期最多 5 分钟，且不超过搜索结果有效期。账号、下载器、站点配置变化后必须重新预览。
- 完整预览最多 2,000 项、200 个目录、16 层目录；超限返回错误，不把部分列表伪装成完整大小。单次选择最多 500 个文件，编码后的清单另有 24 KiB 上限。
- 提交冻结具体文件和大小；分享后来增加文件不会扩大任务范围。重试先核对已有目标文件，再转存缺少部分。身份、路径、大小变化或目标目录出现意外内容会阻止继续。
- 一次选择中的多个视频使用独立、可去重的提交。部分入队失败时页面保持原选择，重试不会重复创建已入队任务；更换选择或目的媒体库需要重新预览。
- Node 使用 `pan115_share_selected_v1` 来源类型，选择清单只存在于加密任务来源和密封授权中。旧 Node 会拒绝新类型，不会退回全量转存；使用此功能时需配套升级 Node。

### 新增 API

`POST /api/v1/discovery/share-preview` 接收 `{result_token, downloader_id}`，返回 `{token, entries, total_size, file_count, expires_at}`。每个 entry 仅含 `{token, path, name, is_dir, size}`；目录大小为完整后代文件之和。响应不包含分享地址、提取码或网盘文件 ID。

`POST /api/v1/discovery/share-preview/recognize` 接收 `{preview_token, entry_token}`，返回现有 `SiteRecognitionSummary`。对单个文件的识别不会把整个合集绑定为同一影片。

`POST /api/v1/discovery/downloads` 增加可选 `{preview_token, selected_entry_tokens}`；两者必须配合使用，空选择不会转为全量。选择目录会在 Server 展开为当次预览中的固定叶子文件。选择模式返回的正常下载摘要增加 `selection_tasks`、`selection_pending`、`selection_error`；首个任务仍保留原 `id` 字段，所有子任务遵循既有下载 API。未提供选择参数仍沿用整份分享行为。

上述接口同时要求 discovery read、downloads create 及对应 Site、Downloader 资源权限，提交时另检查目标 MediaLibrary 权限。Player discovery 下提供同等接口。客户端关闭预览时取消读取与识别请求，不持久化预览 token。
