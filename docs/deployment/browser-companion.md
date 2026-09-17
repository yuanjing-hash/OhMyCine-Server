# 受管浏览器组件

默认使用 Server 管理的 CloakBrowser companion；FlareSolverr 仍是用户自行部署、填写地址的公开页面渲染服务，不接收插件登录凭据。

## 安装与许可

OhMyCine 分发自己的 companion 源码及固定版本依赖声明，不在 GitHub 安装包或公开 Docker 镜像中分发 CloakBrowser 浏览器二进制。第一次使用时，管理员需要在产品中阅读并明确接受上游许可，再获取对应平台的浏览器。打包、启动 Server、安装 npm 依赖都不代表接受许可，也不应下载浏览器。

组件在 **Server 系统设置 → 内置浏览器** 中统一安装和查看状态，不是每个插件各装一份。插件账号密码登录默认使用该组件；遇到站点安全验证时才显示人工验证区域，完成后继续本次登录。Cookie 粘贴仍作为备用入口。

“已安装”只代表浏览器文件已取得，不代表当前平台能成功启动。启动失败、站点无法打开、未安装应分别处理；不要反复修改账号密码来解决组件问题。

登录成功后，Server 加密保存限定连接和镜像的 Cookie，不保存账号密码、网页表单或完整浏览器配置。浏览器空闲回收和 Server 重启不应直接等同于账号退出；恢复 Cookie 后仍由站点真实响应判断登录是否有效。切换镜像或修改连接凭据需要重新验证。验证码始终由用户手动完成。

## Docker

主 Server 镜像包含 Node.js 22、companion 的锁定依赖、Linux 浏览器共享库及中日韩字体，支持 amd64/arm64。无需另设 companion 容器或填写服务地址。`compose.server.yml` 配置 init 进程和 256 MiB 共享内存，浏览器端口不对外公开。

浏览器缓存保存在 `server-state` 卷中的 `/var/lib/ohmycine/browser`。容器使用非 root 用户，保留浏览器 sandbox；不要用 privileged 或关闭 sandbox 来处理启动失败。宿主机若限制非特权用户命名空间，需先核实平台安全配置，产品应显示组件启动失败而非声称登录失效。

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
| `OMC_CLOAK_TUN_FAKE_IP` | 部署管理员明确设置为 `true`，允许受管浏览器通过可信 TUN 的 IPv4 Fake-IP 联网；默认关闭 |

不要对外暴露 companion 端口或 CDP。其认证凭据由 Server 管理，不能填到第三方 FlareSolverr 设置中。

## TUN / Fake-IP 网络

某些 TUN 客户端将站点域名解析为 `198.18.0.0/15` 范围内的虚拟地址，再由 TUN 转发到实际站点。旧的浏览器检查把这类地址当作受限地址，导致组件已安装却打不开站点，不代表账号密码错误或镜像权限错误。

仅在 **Server 所在机器或容器的网络确实经过你信任的 TUN** 时启用此选项。不是仅在访问管理页面的电脑上打开代理就有效。系统设置中的内置浏览器会显示当前生效状态；插件或网页不能修改这一部署信任选项。

Windows 源码部署，在启动 Server 的同一个 PowerShell 中设置：

```powershell
$env:OMC_CLOAK_TUN_FAKE_IP = 'true'
.\start.ps1
```

使用发行目录时，同样先设置变量，再运行该目录的 Server 可执行文件。若 Server 已运行，需由管理员正常停止并重新启动才生效；本选项不会自动重启服务。

Docker 在 Server 服务的 `environment` 中添加：

```yaml
environment:
  OMC_CLOAK_TUN_FAKE_IP: "true"
```

然后使用原部署命令重建该服务容器（例如 `docker compose -f compose.server.yml up -d`）；仅 `docker restart` 不会更新容器环境变量。容器本身的 DNS 与路由也必须经过该 TUN，设置变量不会替你安装或配置代理。

只有精确字符串 `true` 开启兼容，其他值保持关闭。兼容只允许固定站点域名的 DNS 结果位于上述 IPv4 范围，不允许用 IP 直接作为站点，不允许内网、回环、链路本地、IPv6 映射地址，混有这些地址的 DNS 回答仍被拒绝。仍固定数字地址连接，并校验原站点的 HTTPS 证书与域名；不查询外部公共 DNS 绕过 TUN、不自动切换镜像、不放宽插件白名单。关闭 TUN 后也应关闭此选项；开启但没有可信 TUN 接管时可能连接失败，不能将此选项当成通用解除网络限制按钮。

## 页面资源与重新加载

登录页面的公开脚本、样式、字体、图片、验证框架和 Worker 可以按正常浏览器规则加载，不需要为每个 CDN 单独维护插件白名单。每个网络目标仍使用受控 DNS 与地址固定连接，拒绝内网、回环、元数据地址和不安全端口；不会关闭浏览器沙箱或 TLS 验证。

这不意味着允许跨站登录：主页面和插件请求仍限定当前镜像，Server 不向其他域名注入镜像 Cookie，也不导出第三方 Cookie。第三方资源遵循浏览器自己的 Cookie、CORS 和 TLS 规则；FlareSolverr 不会自动收到登录凭据。

人工验证区的两个按钮不同：

- **更新画面**：只重新截图，不请求重新加载网页。
- **重新加载网页**：在原浏览器会话中重新打开当前 GET 页面，保留 Cookie 和待继续的本次登录；不会重放账号密码提交。若页面来自 POST 表单提交，明确拒绝刷新该表单，用户可以更新画面或继续验证。

部分资源被拦截、连接失败、超时或超过安全限制时，画面下方显示安全原因与计数，不显示资源 URL、响应内容或 Cookie。这是页面网络状态，不等于账号密码错误或登录已过期。网络恢复后可重新加载网页；不会自动切换镜像或自动重复登录。

## 更新限制

当前程序内自更新只替换 Go 可执行文件，**不会同步 companion 源码及其 npm 依赖**。需要浏览器组件变更时，归档部署必须手动更新完整发行目录并重新执行上述 npm 命令；保留数据、配置、密钥和缓存。Docker 使用更新后的完整镜像。Windows 全自动运行时分发及多组件原子更新仍待补齐，不能把单文件自更新宣称为浏览器组件也已升级。
