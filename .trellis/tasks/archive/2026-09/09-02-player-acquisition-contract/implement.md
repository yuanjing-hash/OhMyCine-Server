# Implementation Plan

1. 扩展 acquisition DTO 与 service 列表分页投影。
2. 增加 Player handler、路由与 OpenAPI 合同。
3. 增加提交前 expected identity 复验与 claim 绑定。
4. 增加 service/handler/router 测试，覆盖账号隔离、越权字段过滤、匹配与拒绝。
5. 执行格式化、相关测试、全量 Go 测试、vet 和构建。
