# 云端离线限时、历史确认与下载器并发

云端离线通过 `OfflineLifecycleClient.OfflineOperation(source)` 声明本次操作；通用策略不按下载器名称推断。分享接收、已有云端条目接管、BT/PT、本地下载和后续整理不使用云端计时。原生适配器在实际 native dispatch 前调用持久 recorder，Node 使用原任务的版本化云端 ledger。开始事实可为 dispatch、accepted 或有来源证明的 provider_added；它不是精确 observed-running 时间。首次时钟确定后绝对期限为 30 分钟，进度、重连、worker 租约、重启和 grant 刷新均不更新它。

## 完成和取消

到期维护独立于下载执行容量：对原操作先做一次有界、只读 `OfflineObserver`，在数据库事务外获取证据，再通过原任务身份/账号/选集/租约或独立维护身份 CAS 落定完成或到期。已证实所需内容完成优先结束云端计时；状态不可读或原提交仍未知时，到期本地取消并保留原控制意图。实际上游 stop ACK/缺失核对未确认前，真实下载占用继续保留。所有 stop 都使用 keep-output，不能凭本地取消删除已完成媒体。

显式文件选择保留每一项原始意图。已验证全包 metainfo 可携带独立 required media goal，只包含真实视频；require 字幕已由 Follow 的精确选择冻结。该 goal 不改变物理提交方式。未知原始磁力/HTTP 未提供可靠文件表时保留 aggregate/unknown；文件存在、最终大小、哈希字段、标题或 `Manifest.Complete` 本身不能证明字节已下载。required-files 完成必须由适配器提供明确 finalized-object 断言及本次完整 required digest/身份覆盖。它下游只传递已证明的精确范围，通用清理保留未见附带文件和整包目录。

明确 no-task rejection、原生 terminal failure、原任务真实完成/停止各有持久 settlement；保留原时钟及错误但停止超时维护。认证/配额/Node 传输错误码本身不构成这些证据。已确认运行的原生下载仍按原任务结算；没有原生提交的准备失败与分享转存失败，不作为正在下载的任务长期占用 admission。

只有持久 `not_accepted` 证明原生任务未创建时，用户主动“重试”会在一个事务中生成独立的新 Task/Job，并保留旧时钟和回执；重复点击返回同一新任务。当前权限、账号、目录、路线和历史仍须通过校验，Follow 集数与 acquisition/洗版关联同时迁移；历史需要确认时引导现有资源选择入口。未知写入、已受理后终态失败及超时不会借此重置时钟。

## 准备失败、取消与记录移除

建目录、上传种子是准备阶段，只有实际原生提交才进入离线下载。准备错误使用 `downloader_preparation_failed` 和普通有限失败预算；旧种子准备回执为 unknown 也不能无限套用“核对离线任务”。失败预算耗尽时，同一事务写入 Job 与 DownloadTask 的失败状态，界面不再承诺自动重试。

取消未提交的准备任务后，控制 Worker 在原执行退出后结束准备占用并释放下载名额，不访问 115 离线任务列表，不发起新提交。适配器保留原操作回执，只处理同账号、原计划、原任务目录的准备资源。已取消任务的后台收尾不计入界面逻辑活跃下载数。

`DELETE /api/v1/downloads/{id}` 默认 `delete_data=false`，移除记录、停止后续入库并保留源文件；下载器暂时不可访问时保留私有控制任务继续收尾，不因此阻止移除记录。显式 `delete_data=true` 才请求清理源／临时文件；未提交任务可按原准备目录执行清理，不能扩大到已建立的原生任务、其他任务或媒体库输出。清理请求受理不等于文件已经删除，界面不得静默把用户选择改成 `false`。

分享转存的失败日志保留实际阶段及可用的 HTTP 状态、提供方数字错误码；不保留响应正文、分享链接或凭据。连接测试成功只证明它检查的读取接口，不代表后续写入一定成功。

新增分享任务在创建 DownloadTask、Job、下载占位或接收目录之前，使用已授权接收器读取当前分享根状态。手动单条／批量、搜索结果及选文件入库共用这项检查；搜索本次成功读取的账号／配置／来源绑定证据可供同次选文件拆分复用，不用显示缓存代替检查。明确过期、失效或被取消的分享返回 `pan115_share_expired`，要求更换有效分享；暂时不可读、超时或未知响应仍返回原错误与 `unavailable`，不声称分享已失效。

单条请求使用现有错误 envelope；手动批量请求保持 `results[].error_code`、`results[].message` 和成功／失败计数，失败项没有 Task。客户端应显示原安全失败原因并保留失败输入，不能仅因批量 HTTP 响应成功就提示所有任务已入队。整包接收与可选文件选择保持原含义，BT、磁力和普通离线下载不做分享读取检查。检查通过后分享才过期时，仍由执行阶段终态错误处理。

## HTTP 请求与安全错误

现有单次 raw download、Site/Player discovery download 和手动版本升级请求可选传递 `offline_failure_override_token`。服务端返回 HTTP 409，仍使用 `{code,message,data}`：

```json
{"data":{"error_code":"offline_resource_confirmation_required","offline_failure_warning":{"version":1,"reason":"offline_deadline_exceeded","timeout_minutes":30,"confirmation_token":"<64 lowercase hexadecimal characters>"}}}
```

界面提供“继续下载”和“选择其他资源”。token 仅绑定当前 owner/授权版本、真实来源账号、规范资源身份、精确选集、下载器/原目录、目标/路线/Profile、优先级和历史版本。本次接受与 token 消耗在一个事务中提交。已接受 token 保存原任务身份；Site 在不重开已消费 claim 的前提下，按原 opaque 入口请求摘要及当前站点/来源/目标授权只读重放。洗版入口摘要另含 upgrade ID/revision。改变来源、选集、账号、目标或权限不能复用确认。未知物理写入仍由原 receipt/resource guard 控制；确认不授予重复 Submit 权限。

批量请求可选 `offline_failure_overrides: [{index,confirmation_token}]`，index 必须对应本次 sources 下标且唯一。每个结果可含同一安全 `offline_failure_warning`，原始来源不回显。重试可以只发送未接受的警告项并重排新批索引；已接受项不会被整体重发。无全批 override 开关。

历史单独存放在 `offline_failures`，显示 DownloadTask/Job 删除后仍保留。key 使用 owner、工厂确认的账号 namespace、规范 v1/v2 资源身份和 required scope；相同 v1 的 Hex/Base32/大小写、tracker 或 v2 辅助 topic 不产生不同资源，不能仅凭标题或 HTTP token 变化猜测等价。不同账号和非离线路线不受影响。Follow 在共同 Submit 边界跳过匹配历史的离线路线，继续其他合法下载器和候选，不计假 submitted 或 submit_failed。成功明确重试只解决本次观察到的历史版本和 required scope。

## 并发与观察投影

管理 create/PATCH/list 的 `max_concurrent_downloads` 为 1..64 的整数，新建或迁移默认 2；PATCH 省略保留原值，0/越界失败。降低设置保留所有已接受下载。安全 downloader-use options 仍不暴露管理配置。

`download_admissions` 与短期 Job lease 分离。Claim 在同一 writer transaction 读取配置、采用/创建 admission 并领取 Job；满载 A 不阻塞 B、已有原任务观察、完成恢复或控制。共享 BTSelectionGroup 只占一个物理单位，原 union/ref/native ownership 保留。lease 过期、观察 yield、进度断线和未知 stop 不释放占用。升级时保守采用旧 active/unknown 行，再用有界原 Get/只读组 probe/原 Node 回执收敛；不 Submit、不恢复用户暂停。

正常进度、metadata 和重连都是有界 nonfailure scheduled observations，保持原 provider ID/source/选集和失败预算；next observation 不超过持久 deadline。相同优先级继续既有公平规则，连接事件只唤醒原状态重读。QueuePolicy 仍限制短期 Server 工作，用户设置不被迁移覆盖。

DownloadTaskSummary 与 AcquisitionStatus 可选 `acquisition_state`: `waiting_capacity|active|observing|reconnecting|cloud_ready|cancelled_stop_pending|clock_unknown`，以及 `offline_started_at|offline_deadline_at|offline_ready_at|offline_start_provenance`。这些是服务端持久事实；辅助云端状态不覆盖整体 failed/paused/cancelled 或识别诊断。一个逻辑任务不等于一个物理 slot，多个 BT refs 可能共享一个占用。

## Node 和兼容边界

新实际离线路线在 preview 和最终 Submit 都要求 `cloud_offline_lifecycle_v1`，原 share/adoption 资格独立判断。plan1 冻结 namespace/required digest，required intent 只进入 sealed credential；status/keep-output stop 绑定原 operation、账号、plan、lease 和 grant，不能由旧 grant 刷新扩大权限。没有新增 remote NativeWanted，显式缺集/子集仍要求既有真实选择能力。

旧 accepted plan0 保留原计划和已授权 status/read/数据流自然结算，投影 clock_unknown，不能猜测开始时间或新开 30 分钟，不能偷偷增加 stop grant 或再次 Submit。Node 在断连/原 grant 或 lease 过期时只能持久记录本地到期和 stop_pending、阻止新物化，物理上游 stop 等原操作获得有效授权后收敛；不会延长凭据有效期来假装及时停止。

本轮为隔离源码、SQLite 与协议 fixture 验证；没有运行服务、生产 DB、账号凭据、实际媒体、部署或发布验收。
