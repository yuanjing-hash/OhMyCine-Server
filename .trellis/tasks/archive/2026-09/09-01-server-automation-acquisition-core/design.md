# Design

## 1. Authorization model

保留现有 `roles` / `role_permissions`，新增用户级规则和资源范围规则。规则以稳定 permission code 为键，effect 仅为 allow/deny；scope 为空表示全局，否则使用受限的 resource_type + resource_id。授权解析器一次生成全局能力和资源规则，deny 在相同或更宽范围内优先。所有资源服务使用同一 `Authorizer.Require(actor, permission, resource)`，路由中间件只负责早拒绝。

内置角色定义改成模板目录，应用模板只复制当时的 permission 集合，不让后续模板更新覆盖管理员自定义角色。系统保护角色/owner 约束继续保留。权限变更在事务内校验操作者不能授予自己不具备的能力，并递增授权 revision，使现有会话下一次请求读取新权限。

Player 登录和 Bootstrap 使用同一授权解析器。返回的 capabilities 由 permission-to-capability 映射生成，至少区分连接、目录浏览、发现搜索、创建入库、订阅和管理操作。

## 2. Acquisition aggregate

增加以 actor + media_type + tmdb_id 为稳定身份的 acquisition aggregate，以及追加式阶段事件/任务关联。聚合保存当前阶段、下载/transfer/job/follow 引用、目标媒体库、覆盖快照、最后错误码和 revision；不保存站点凭据、下载 URL 或上游正文。

创建下载或订阅时在一个事务内写入冻结配置和阶段事件。下载、transfer、artifact、scan/follow worker 通过小型状态投影服务推进 aggregate，推进操作具备幂等键，允许进程恢复和重复事件。Player DTO 仅返回安全阶段、进度、时间、覆盖和可执行动作。

## 3. Shared search coordinator

`SiteSearchCoordinator` 统一普通标题和 MediaIdentity 两种搜索。当前每个请求本地创建 `chan(4)` 的实现改为 Server 生命周期内共享的 weighted semaphore；请求内另外保留公平排队和选站上限。默认并发为 4，配置值有安全上下界。

每个站点任务流程：queued -> running -> success/error/timeout/cancelled。站点调用使用独立 timeout child context，仍经过现有 `waitLimit`。单站的多语言别名有界串行执行，以免绕过站点限流；每个别名复用现有匹配、claim 绑定和去重规则。

协调器只向一个串行事件汇聚器发送内部事件。汇聚器更新计数、累计结果数并输出：

```text
media     已验证媒体身份和查询别名（仅身份搜索）
progress  全量计数快照与最近变化站点
site      一个站点的最终安全结果组
done      最终计数、失败站点和稳定排序摘要
error     仅用于整个请求无法开始的错误
```

并发完成的 `site` 可立即展示；JSON 返回和 `done` 摘要按 priority/id 稳定排序。单站重试仍传明确 site_ids，只替换该站点结果。

## 4. Directory structure state machine

路由语义：

- `GET .../structure`：最近诊断投影。
- `POST .../structure/diagnose`：创建诊断运行。
- `POST .../structure/preview`：基于诊断 revision 生成不可变计划和确认 token。
- `POST .../structure/repair`：验证 token/revision 后执行。
- `GET .../structure/runs`：历史。

计划 manifest 明确列出每个受管源、目标、尺寸/身份、元数据再生成和待清理项。执行阶段依次为 preflight、move、path reconciliation、artifact regenerate、obsolete managed artifact cleanup、empty-directory cleanup、final verify。任何阶段未完成都保持 running/partial/failed，不能写 healthy。

本地 mover 优先同卷 `os.Rename`，跨卷采用可校验 copy-to-temp + fsync/size verification + rename + source delete，从产品语义上仍是 move。115 执行器使用 provider item ID、批量 Move 和 reconciliation。空目录清理只从已验证源作品目录向媒体库根回溯，并在每层重新确认空且仍位于 root 内。

旧 artifact 清理不再以 `STRMEnabled` 作为所有本地受管元数据的门槛；按 target kind 和 managed ownership 判断。未托管残留只报告。Windows 错误分类器识别 sharing violation/lock violation 和 access denied，前者退避重试并确保关闭句柄，后者进入能力诊断。

## 5. Unified scheduler

新增 scheduler definition 和 run 表。definition 保存 owner/type/target、五段 cron、timezone、enabled、misfire policy、overlap policy、retry policy、max runtime、revision。解析器拒绝 6/7 段及不受支持语法，提供未来若干次运行预览。

调度器只负责按 due time 原子领取 definition 并向现有 persistent queue 投递领域 job；业务 worker 不内嵌 Cron。唯一运行/跳过/排队策略通过 definition + active run 事务决定。旧设置迁移为等价 definition，保留一次兼容读取窗口，迁移成功后不再双重触发。

## 6. Web UI

增加统一“计划任务”页、Cron 可视化编辑器、用户直接授权/拒绝和资源范围编辑。媒体库标题栏始终展示“检查目录结构”，操作按诊断 -> 预览 -> 确认修复分步。Server 搜索界面消费同一 SSE 进度协议，展示总进度、running/pending、已发现数、失败站点与重试。

## 7. Safety and compatibility

- 保留非流式搜索 API，内部改用协调器聚合。
- SSE 只输出安全站点名称、状态、计数、opaque claim；禁止 Cookie/passkey、torrent ID、上游正文和本地路径。
- 所有 mutation 使用 CSRF、permission、service authorization、revision/confirmation token 和审计。
- 数据迁移可重复执行；旧角色和旧计划配置保持等价权限/调度结果。
