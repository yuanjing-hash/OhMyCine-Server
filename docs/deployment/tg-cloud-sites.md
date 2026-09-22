# TG 网盘资源站点

在 **站点 → 添加站点 → 网盘 / TG** 中创建站点。一个站点可以聚合最多 100 个公开 TG 频道，也可以创建多个站点分别管理电影、剧集等来源。当前网盘分类支持 **115**。

## 配置

1. 准备可由 Server 访问的 PanSou 搜索服务，填写其 HTTPS 根地址，例如 `https://pansou.example.com`，不要填写 `/api/search`。Server 不内置公共搜索服务，也不会自动部署 PanSou。
2. 选择 115，填写频道名称、`@channel_name` 或 `https://t.me/channel_name`，每行一个。重复频道会自动合并。不支持私密群邀请链接，也不需要向 Server 提供 Telegram 登录会话。
3. 如果 PanSou 开启账号认证，勾选认证并填写用户名、密码。凭据在 Server 中加密保存；编辑时密码留空保留原值，关闭认证会清除已存凭据。
4. 保存会实际测试一次搜索，失败则保留原配置。当前沿用站点 3–30 秒超时范围，TG 站点默认 30 秒；PanSou 本身需要具备访问配置频道的网络条件。
5. 在下载器中配置支持分享转存的 115 下载器，选定网盘账号和接收目录。站点不另设账号、下载目录或执行节点。

## 搜索与追更

保存后该站点会进入统一搜索的站点列表。已有搜索选择会保留，请在选择站点时勾选新增站点。结果展示频道来源和可用的 TG 原帖链接；点击 **转存入库** 后选择 115 下载器与最终媒体库。分享地址和提取码保存在 Server 的短期结果凭证中，不直接返回浏览器。

订阅编辑时选择 115 下载器，并勾选对应网盘 / TG 站点。订阅继续按已有计划搜索、筛选明确缺失的集数，再执行分享转存和入库。网盘结果没有做种人数和 PT 免费促销属性，这两类筛选不作用于分享结果；清晰度、关键词、发布时间等其他条件仍生效。设置大小限制时，缺少可信大小信息的结果仍不会自动通过。

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
