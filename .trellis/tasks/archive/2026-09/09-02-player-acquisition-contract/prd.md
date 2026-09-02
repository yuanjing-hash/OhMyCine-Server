# Player 入库身份绑定与默认目标合同

## Goal

为 Player 分步入库 UI 提供账号隔离、可恢复的 acquisition 列表，并保证从 TMDB 海报发起的资源不会静默识别成其它作品后错误入库。

## Requirements

- 新增 owner-scoped Player acquisition 列表 API，支持有界分页并返回安全展示字段、阶段、进度、目标库和更新时间。
- 下载创建接口可携带期望 TMDB ID 与媒体类型；Server 必须验证身份并把其绑定到 actor-scoped opaque result claim。
- 资源标题与期望 TMDB 多语言身份明显不匹配时，在任何下载器副作用之前拒绝提交。
- 聚合搜索已经绑定的身份保持兼容；直接标题/TMDB 搜索可在提交时补绑定。
- 任务列表不得泄露其它账号任务或无权读取的媒体库 ID。
- Player 可用下载器和媒体库继续按服务端稳定顺序返回，第一项作为当前默认选择。

## Acceptance Criteria

- [x] 用户只能列出自己的 acquisition，分页参数有严格边界。
- [x] 列表状态能表达排队、下载、整理、入库、完成和失败，并包含下载百分比、字节、速度、ETA 与传输文件进度。
- [x] 提交海报资源时身份在 Server 复验后被冻结；明显不匹配不会调用下载器或消费 claim。
- [x] 旧客户端不传期望身份时行为兼容。
- [x] 路由权限、服务测试和路由测试同步更新。
- [x] `go test ./...`、`go vet ./...` 与普通/WebUI 构建通过。
