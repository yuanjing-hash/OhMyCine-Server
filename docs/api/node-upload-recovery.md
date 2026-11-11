# Node 上传执行与进度合同

本合同扩展现有 v1 操作及存储响应，不改变冻结上传计划、操作编号、文件选择或授权请求摘要。

## 执行归属

HTTP 请求完成与 Node 上传结束分别记录。Server 在父任务租约仍有效时继续核对原批次，单次请求超时不会将 Transfer 或 Job 判为完成。完成需要已核实的原文件结果及后续目录发布。未知写入不能因目标暂时未出现而再次提交。

Server 的持久物理写入记录保留原操作的归属。Server 重启只说明原 Server 进程退出，远端提供方写入仍需取得原 Node 的退出事实。原 Worker 的迟到回执必须同时通过队列 claim 与操作 epoch 栅栏，不能覆盖接手后的状态。

Node 从持久回执恢复文件检查点。最终回执保存失败时，重入仍核实已有目标与摘要。凭据续期保持同一操作、账号、目标范围和原计划摘要。授权编号不参与存储请求的稳定摘要。

## 停止与接管

新增可选能力 `operation_stop_cas_v1`，对应 `POST /node/v1/operations/{operation_key}/stop`。请求携带原计划摘要、精确 epoch 和取消意图。

```json
{"plan_digest":"<原计划摘要>","lease_epoch":1,"cancel":false}
```

Node 原子匹配已配对 Server、原操作、摘要及精确 epoch。暂停仅缩短该 epoch 的租期，取消在同一匹配条件下保存取消。停止请求不采用后来出现的更大 epoch。租约已更换时返回 `node_lease_conflict`。

续租先持久预留唯一递增 epoch，回执未知时保留已发高水位。既有 Node 返回较旧 epoch 时，先核对原计划，再续租本次预留的 epoch。较新的远端 epoch 不会被接管。

停止 ACK 之后仍需原存储结果的 `execution_active:false` 或可信完成结果，才可确认原异步执行已退出。Server 控制动作携带原物理写入凭据。持久 pause/cancel 被新的队列 claim 接手时，只有当前有效 claim 可核对并停止其前任。

若预留的续租尚未到达 Node，控制动作先确认实时停止能力，再凭原物理写入凭据预留更高控制 epoch 并发布已到期租约。封住旧续租后，才执行精确停止并等待原执行退出。首次停止 ACK 不释放写入保护。

旧 Node 不声明此能力时，新 Server 保留原恢复与写入保护，并明确提示升级节点，不会回退到可能取消新 owner 的无条件控制。旧 Server 可忽略新的能力值与响应字段。旧请求与原控制端点的 JSON 形状保持兼容。

## 可选响应与计量

StorageActionResponse 增加三个可选字段 `execution_active`、`phase` 和 `upload_progress`。其中 `upload_progress` 使用带版本的计量对象。

```json
{"version":1,"transferred_bytes":1048576,"total_bytes":2097152,"upload_speed_bps":524288,"sampled_at":"2026-11-11T00:00:00Z"}
```

此对象是 `upload_progress` 的示例，不包含账号、路径、令牌或临时地址。未知版本、缺失字段及无效观测降级为未知计量，不改变有效文件结果的验证要求。

`transferred_bytes` 是当前文件批次已交付内容的逻辑字节，分片重发不会累计为更高完成度。Server 汇总冻结 Transfer 的各批次。已核实秒传或复用可计入交付结果，但不产生上传速度。速度仅取本次上传尝试的实际请求正文采样，源摘要计算与目标读回不计入上传。通用 Job.speed 没有采样时钟，本流程保持其为空。带采样的速度由 Transfer DTO 提供，字节进度仍沿用原任务心跳与通知。

阶段区分 `waiting_node`、`hashing_source`、`uploading_target`、`verifying_target`、`waiting_credentials` 和 `completed`。界面只在上传阶段且采样新鲜时显示当前速度，旧 Node 或不支持回调的提供方显示未知。状态、阶段和安全错误码在变化时记录，日志不得包含私密计划、SDK 原始响应或凭据。

## 兼容恢复

私密 Transfer 计量使用 migration128 的 `upload_progress_json`，不修改旧 Node 表结构。自动恢复旧假完成 Job 仅限原归属、冻结路由、原批次、当前权限和 CAS 校验全部成立的记录。读界面独立反映实际未结算状态。恢复证据不足不能伪装为已完成，也不授予写权限。

原 Node 已完成时优先读取原结果，不依赖来源继续保留、导出续期或新上传授权。尚未完成的执行才检查并续期原导出，始终保留原 key、digest 与文件选择。
