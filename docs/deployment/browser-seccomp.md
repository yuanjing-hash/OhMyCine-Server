# 浏览器 seccomp 策略

`deploy/seccomp/browser.json` 是完整、默认拒绝的容器策略；不能把两个额外规则独立当成 Docker profile。

基线固定自 [Moby profiles](https://github.com/moby/profiles/blob/2ceae35d351c156cb5a8efc0fdc4a08cf94569d8/seccomp/default.json)，commit `2ceae35d351c156cb5a8efc0fdc4a08cf94569d8`（2026-09-30）。原始文件 `moby-default.json` SHA-256 为 `6416b47770785a41ac59073cdc77d9fe98517df2799dc83ef207e622de3053f6`。同目录保留来源 `upstream.json` 和 Apache-2.0 `LICENSE.moby`。基线需要定期审查更新，不能盲目换成上游 main，也不能复制旧 Playwright profile 覆盖更新的 Docker 限制。

使用 `python scripts/build-browser-seccomp.py` 可从原始基线重建交付策略。全部 baseline 字段、architecture 映射和 syscall 条件规则保留，尾部仅增加：

- amd64/arm64 `clone` 参数0的 namespace mask `0x0e020000` 必须为0，因此只增加 USER/PID/NET namespace 类型；仍拒绝 NEWNS、NEWCGROUP、NEWUTS 和 NEWIPC，内核的用户命名空间/所有权检查仍生效。
- amd64/arm64 `unshare` 参数0必须精确为 `CLONE_NEWUSER` (`0x10000000`)。`setns` 和 `clone3` 没有新增权限，`clone3` 的既有 ENOSYS 回退规则保留。

依据 [Playwright Docker 部署](https://playwright.dev/docs/docker) 的非 root、用户命名空间 sandbox 要求，以及 Chromium 的 [NamespaceSandbox](https://chromium.googlesource.com/chromium/src/+/refs/heads/main/sandbox/linux/services/namespace_sandbox.cc) 和 [Credentials](https://chromium.googlesource.com/chromium/src/+/refs/heads/main/sandbox/linux/services/credentials.cc) 调用类型，选择参数限制以避免无条件放开 clone/setns/unshare。升级浏览器或架构时需以无账号、无网络的隔离启动和本地渲染再次复验，不能仅凭策略 JSON 校验认为支持。

默认 ERRNO、AF_ALG/AF_VSOCK socket 筛选、io_uring 默认拒绝、bpf/ptrace 既有条件规则及其它危险 syscall 规则均保持基线原貌。该策略不能解决宿主机禁止 user namespace 的 sysctl/LSM 限制，也不修改 TLS、页面网络、凭据或插件权限。

普通 Server/Node 健康检查不获取浏览器或接受许可。安装浏览器与实际 sandbox、渲染及站点账号验收是独立步骤。
