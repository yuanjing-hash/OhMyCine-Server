# 实施计划

1. 为 Player 115 详情和流解析补充无 STRM artifact 的失败测试，包括权限与 provider root 边界。
2. 将 Player 115 可播放判定改为 entry/Storage/Connection 事实，并在受保护流端点复用 115 临时直链协调器。
3. 为主动 STRM 请求增加当前 generation 的即时 artifact 调度，再保留后台 reconcile。
4. 将 structure repair、STRM reconcile、media artifact 的资源键拆成域级每库键，补队列领取测试。
5. 增加真实 reconcile 提交新增条目会创建 artifact run 的服务测试，修复发现的调度缺口。
6. 运行聚焦测试、全量 Go 测试、vet、diff check；更新相关 Trellis backend spec。
