# 受管浏览器组件

默认使用 Server 管理的 CloakBrowser companion；FlareSolverr 仍是用户自行部署、填写地址的公开页面渲染服务，不接收插件登录凭据。

## 安装与许可

OhMyCine 分发自己的 companion 源码及固定版本依赖声明，不在 GitHub 安装包或公开 Docker 镜像中分发 CloakBrowser 浏览器二进制。第一次使用时，管理员需要在产品中阅读并明确接受上游许可，再获取对应平台的浏览器。打包、启动 Server、安装 npm 依赖都不代表接受许可，也不应下载浏览器。

组件在 **Server 系统设置 → 内置浏览器** 中统一安装和查看状态，不是每个插件各装一份。插件账号密码登录默认使用该组件；遇到站点安全验证时才显示人工验证区域，完成后继续本次登录。Cookie 粘贴仍作为备用入口。

“已安装”只代表浏览器文件已取得，不代表当前平台能成功启动。启动失败、站点无法打开、未安装应分别处理；不要反复修改账号密码来解决组件问题。

登录成功后，Server 加密保存限定连接和镜像的 Cookie，不保存账号密码、网页表单或完整浏览器配置。浏览器空闲回收和 Server 重启不应直接等同于账号退出；恢复 Cookie 后仍由站点真实响应判断登录是否有效。切换镜像或修改连接凭据需要重新验证。验证码始终由用户手动完成。

## Docker

主 Server 镜像包含 Node.js 22、companion 的锁定依赖、Linux 浏览器共享库及中日韩字体，支持 amd64/arm64。无需另设 companion 容器或填写服务地址。`compose.server.yml` 配置 init 进程和 256 MiB 共享内存，浏览器端口不对外公开。

浏览器组件缓存保存在持久挂载中的 `/var/lib/ohmycine/browser`。每次 Linux 浏览器会话另外创建独立的 0700 临时 HOME、XDG 配置和缓存目录，不读取宿主机或管理员的浏览器配置；关闭、启动失败、导航失败和取消后清理。临时目录需要可写空间，持久组件缓存和加密登录 Cookie 不会随该清理删除。

容器使用非 root 用户，保留 Chromium sandbox。完整 Server 下载归档内的 `deploy/compose.server.yml` 必须与同一发行版的 `deploy/seccomp/` 目录一起保存；Compose 引用邻接的 `seccomp/browser.json`。完整策略以固定 Moby 默认策略为基线，只为 amd64/arm64 的 Chromium 用户、PID 和网络命名空间 clone，以及 `unshare(CLONE_NEWUSER)` 增加参数受限权限。其他默认拒绝及条件规则保留，不增加 capability，不使用 privileged、SYS_ADMIN、seccomp=unconfined、host IPC 或关闭 sandbox。来源、基线哈希和差异见 [浏览器 seccomp 策略](./browser-seccomp.md)。

已有容器只拉取新版镜像不会更新它的 Compose 或 seccomp 设置。先备份当前 Compose，将同版完整 `deploy/seccomp/` 放到 Compose 邻接目录，加入 `security_opt: ["seccomp=./seccomp/browser.json"]`，核对原有端口、数据、媒体和下载挂载后，再由管理员明确执行服务重建。保留原有数据、加密密钥和浏览器组件缓存。Docker profile 是容器创建参数，无法在运行中的旧容器里通过“检查组件状态”改变。OhMyCine 不会自动修改主机 sysctl/AppArmor 或重建服务；宿主机仍禁止非特权用户命名空间时，需要管理员核实平台策略，页面显示“沙箱不可用”，不能声称登录失效。

公开镜像构建验证不下载安装浏览器。实际浏览器启动、人工验证和登录验收仍需要在接受许可后完成，不能以普通容器健康检查替代。

## Windows / Linux 下载归档

Server 归档附带 `browser-companion` 源码和 `package-lock.json`，**不含 Node.js 运行时或已安装的 npm 依赖**。当前归档部署需要在服务器安装 Node.js 20 或更新版本（推荐 Node.js 22），然后在该目录执行：

```sh
npm ci --omit=dev --ignore-scripts
```

Linux 还需要与 Dockerfile 一致的 Chromium 共享库和字体。此步骤仅安装锁定的 JavaScript 包，不下载浏览器，也不自动接受浏览器许可。保持 `browser-companion` 与 Server 可执行文件同级。

源码 Windows 启动脚本 `start.ps1` 正常构建时检查 Node.js 版本、安装上述依赖，并设置源码 companion 路径；`-SkipBuild` 不补装依赖。普通 Server 功能不应要求浏览器已安装。

可选部署环境变量：

| 变量 | 用途 |
| --- | --- |
| `OMC_CLOAK_NODE` | Node 可执行文件路径，默认从 PATH 查找 |
| `OMC_CLOAK_COMPANION` | companion `src/main.mjs` 的绝对路径 |
| `OMC_CLOAK_DATA_DIR` | 私有可写的浏览器组件数据目录 |

不要对外暴露 companion 端口或 CDP。其认证凭据由 Server 管理，不能填到第三方 FlareSolverr 设置中。

## TUN / Fake-IP 网络

某些 TUN 客户端会把域名解析成虚拟 IPv4/IPv6 地址，再由 TUN 转发。受管浏览器现在接受 Server DNS 返回的有效数字地址，包括 Fake-IP、内网和回环地址，无需另设开关；原来的 `OMC_CLOAK_TUN_FAKE_IP` 已移除。只有 **Server 所在机器或容器** 的 DNS 与路由生效，管理页面所在电脑开启 TUN 不会改变 Server 的网络。

域名连接仍固定解析到的数字地址并校验原域名的 HTTPS 证书；主页面仍限定当前镜像，不能以 IP 字面量配置镜像，也不会切换公共 DNS 或自动切换镜像。页面第三方资源可按浏览器规则访问 Server 网络中的 HTTPS 服务，因此管理员应只配置可信的镜像和插件。

## 页面资源与重新加载

登录页面的脚本、样式、字体、图片、验证框架和 Worker 可以按正常浏览器规则加载，不需要为每个 CDN 单独维护插件白名单。每个网络目标仍使用受控 DNS 与地址固定连接，并拒绝不安全端口；不会关闭浏览器沙箱或 TLS 验证。

这不意味着允许跨站登录：主页面和插件请求仍限定当前镜像，Server 不向其他域名注入镜像 Cookie，也不导出第三方 Cookie。第三方资源遵循浏览器自己的 Cookie、CORS 和 TLS 规则；FlareSolverr 不会自动收到登录凭据。

人工验证区的两个按钮不同：

- **更新画面**：只重新截图，不请求重新加载网页。
- **重新加载网页**：在原浏览器会话中重新打开当前 GET 页面，保留 Cookie 和待继续的本次登录；不会重放账号密码提交。若页面来自 POST 表单提交，明确拒绝刷新该表单，用户可以更新画面或继续验证。

部分资源被拦截、连接失败、超时或超过安全限制时，画面下方显示安全原因与计数，不显示资源 URL、响应内容或 Cookie。这是页面网络状态，不等于账号密码错误或登录已过期。网络恢复后可重新加载网页；不会自动切换镜像或自动重复登录。

## 更新限制

当前程序内自更新只替换 Go 可执行文件，**不会同步 companion 源码及其 npm 依赖**。需要浏览器组件变更时，归档部署必须手动更新完整发行目录并重新执行上述 npm 命令；保留数据、配置、密钥和缓存。Docker 使用更新后的完整镜像。Windows 全自动运行时分发及多组件原子更新仍待补齐，不能把单文件自更新宣称为浏览器组件也已升级。

## 启动诊断

“检查组件状态”只读取安装元数据和最近的运行失败，不启动浏览器，不请求站点，也不会清空已知失败。修正部署后重新提交插件登录才会尝试启动。启动成功并打开页面后清除本次启动错误；组件已安装、浏览器能启动和账号登录成功是分别验证的状态。

| 安全原因 | 处理方向 |
| --- | --- |
| 沙箱不可用 | 核对配套 Docker seccomp 和宿主机用户命名空间支持 |
| 私有临时目录不可用 | 核对 Server 临时目录可写权限及可用空间 |
| 缺少运行依赖 | 更新完整目录或镜像，核对 Node、包装依赖和共享库 |
| 浏览器文件不可执行 | 核对已取得组件的文件及执行权限 |
| 启动超时 | 核对 Server 资源后明确重试登录 |
| 未识别的启动失败 | 检查完整安装与运行环境；不会返回原始浏览器日志 |

分类仅在内存中识别有限的启动特征。API、组件状态和 UI 不保存或展示原始异常、argv、文件路径、代理、页面 URL 或 Cookie；普通“Operation not permitted”不会被武断认定为沙箱问题。
