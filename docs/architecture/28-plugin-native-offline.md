# 在线插件媒体的原生本地下载

Server 0.1.1 的 `online_offline_plan_v1` 是 Player bootstrap 的可选能力。在线媒体库摘要同时返回 `offlineDownloadSupported`；旧 Server 缺少字段时应视为不支持。在线媒体库由已有插件连接关联产生，不创建可上传的物理媒体库。

`POST /api/v1/player/online-libraries/:libraryId/items/:workId/offline-plan` 要求当前 Player 设备具有 `media_libraries.read` 和 `downloads.create`。请求体严格限制为显式的 `{segmentId,versionId,variantId}`，不允许自动换清晰度或分集。Server 校验启用的库、连接、安装、当前 package 的 `download.plan` 授权和插件能力，再调用 Runtime v1 operation 20。旧 `media.download_plan` 只能适配明确 MP4 或明确 DASH 双轨，不能从播放能力推断下载能力。

标准响应信封的 `data` 示例：

```json
{"version":1,"libraryId":"online-library-uuid","workId":"work","segmentId":"episode","versionId":"original","variantId":"720p","suggestedFileName":"节目.mp4","format":"hls","expiresAt":1791619200,"tracks":[{"id":"video","kind":"video","units":[{"id":"video:segment:42:8ff260efb6780204","url":"https://cdn.example.test/42.ts?short-lived-signature=opaque","byteRange":{"offset":0,"length":1024},"expectedBytes":1024}]}],"sidecars":[]}
```

`unit.encryption` 可选 `{method:"aes-128",keyBase64,ivHex}`；`headers` 只在无敏感信息时导出。字幕/弹幕 sidecar 有稳定 `id`、`kind`、`url`，可选 `format`、`language` 和安全 `headers`。目前格式从 MIME 推导 vtt/srt/ass/xml/json，插件契约没有单独 language 字段，缺失时不猜测。

Runtime 为 detail/playback/auth.poll/offline-plan 控制链提供最多 45 秒预算，简单 site 操作 16 秒；每次 Host HTTP 仍最多 15 秒。原生在线库 JSON/私有计划调用最多 60 秒，普通请求和下载单元维持各自原有限制，Server WriteTimeout 60 秒留有返回安全错误的余量。

整个离线计划解析也限制 45 秒。derive/export 元数据校验在单次计划上下文内共享最多 64 个 hostname 的 5 秒公网 DNS 观察，每个子 URL 仍校验 scheme/domain、当前 grant 和 owner。实际 HTTP 连接、控制读取及 redirect 不使用该缓存，仍重新校验公网地址；原生下载的公网校验也独立执行。

只有该私有、`Cache-Control: no-store` 的原生响应允许短时无凭据 HTTPS CDN URL。需要 Cookie、Authorization 或含 query 的 Referer 等私密 transport 时返回 `/api/v1/player/online-libraries/:libraryId/offline-assets/:assetRef`。每次 GET/HEAD 都重新检查设备 read + download、库/连接关联、资产拥有者、安装 package/generation、当前 network/download grant、有效期、域名及公网 IP。离线引用标记 `OfflineOnly`，普通 `/online-assets/:assetRef` 无法重放读取；Server Bearer 只由原生层发送到配置的 Server origin。响应正文、URL query、凭据和密钥禁止日志与持久任务状态；错误仅返回稳定安全代码。

`ReadOfflineControl` 最多读取 2 MiB 的 HLS 控制清单或 16 字节 AES key。计划总单元最多 8192，最多 48 次控制读取、32 把 key、master 两层。清单最多 50000 行，每行 8192 字节。支持 VOD 的相对 URI、独立音轨、初始化 MAP、byte range 和 AES-128；要求 ENDLIST，拒绝直播、DRM/SAMPLE-AES、未知加密格式、discontinuity、GAP/PART/SKIP 和视频 representation 歧义。没有完整视频暂存。路径/range 摘要参与稳定单元身份，刷新不会把新的地址当作完整性证明；Player 必须校验内容实体/字节和轨道拓扑。

普通 HLS 播放也通过 `rewriteHLSAsset` 重写 master/media 清单中的 segment/map/key/audio URI，子资产绑定相同 owner 与有效期；HEAD 必要时用内部 GET 读取控制正文，再返回 HEAD 元信息。该路径最大图深度 8、控制清单 2 MiB、重写正文 4 MiB；非标准扩展/comment 被移除，避免提供方私有参数经普通响应泄漏。所有媒体主体仍流式读取。派生引用全局最多 32768；过期引用回收、失败清理已产生引用，离线计划完成后释放控制和 direct 单元引用，网关引用保留至到期。

只读账号登录声明 `site.auth` 并沿用 op10/11，兼容老 `site.interaction`。Host op8 commit 返回 `credentialVersion`，新只读插件 confirmed poll 必須回传该版本。Server 通过 CAS 保存安全账号/member 摘要与观察时间，防止旧轮询覆盖新凭据的显示摘要；这不改变提供方登录事务的并发语义。凭据变更清空摘要，过期/24 小时未确认/非 healthy 状态显示 unknown，逐视频鉴权仍由提供方实时判断。数据库 migration 116 增加两列，不修改旧密文和版本。
