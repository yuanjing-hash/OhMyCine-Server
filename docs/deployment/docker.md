# Docker 部署

Server 与公网 Node 使用独立镜像，均支持 `linux/amd64` 和 `linux/arm64`，Docker 自动选择主机架构。Node 只安装子系统，不捆绑下载器。

## 发布规则

现有 Server Beta Release 上传归档成功后调用 `docker-publish.yml`，使用 `GITHUB_TOKEN` 的 `packages: write` 权限推送：

- `ghcr.io/yuanjing-hash/ohmycine-server:vX.Y.Z` 和 `:beta`
- `ghcr.io/yuanjing-hash/ohmycine-node:vX.Y.Z` 和 `:beta`
- `docker.io/<Docker Hub 用户名>/ohmycine-server:vX.Y.Z` 和 `:beta`
- `docker.io/<Docker Hub 用户名>/ohmycine-node:vX.Y.Z` 和 `:beta`

推送 `server-vX.Y.Z` 标签自动构建完整 GitHub 下载资产和 Docker 镜像。标签提交属于 `main` 时发布正式版并更新 `stable`、`latest`；仅属于 `develop` 时发布 Beta 并更新 `beta`。两分支都包含该提交时优先正式版；两者都不包含则拒绝。版本号在通道间不能重复。普通分支 push 不发布。镜像复用同次 Release 的二进制并核对 SHA-256 和标签提交，保留内置 WebUI、TMDB 配置及 Node 安装验证公钥。首次发布后在 GitHub Packages 将两个包设为 Public，即可免登录拉取；私有包需要在部署机器执行 `docker login ghcr.io`。

## Docker Hub 自动发布配置

流程同时保留 GHCR，并推送 `docker.io/<Docker Hub 用户名>/ohmycine-server` 和 `docker.io/<Docker Hub 用户名>/ohmycine-node`，标签同样是 `vX.Y.Z` 和 `beta`，两者都包含 AMD64/ARM64。

在 Server GitHub 仓库的 `Settings → Secrets and variables → Actions` 配置 `DOCKERHUB_USERNAME` 和 `DOCKERHUB_TOKEN`。Token 使用具有这两个仓库读写权限的 Docker Hub Access Token，不使用账号密码，不写入代码。两个 Secret 为发布所必需。Docker Hub 上创建上述两个公开仓库后，用户可免登录拉取；私有仓库需要 `docker login`。

自动发布只需推送上述版本标签，不必先手工创建 Release 或上传资产。也保留 GitHub `Actions → Server Beta Release → Run workflow`，选择最新 `develop` 并输入版本号，手动发布 Beta。完整归档和签名上传成功后直接调用镜像流程（不依赖 GitHub Token 创建 Release 的事件再次触发），再分别拉取 GHCR、Docker Hub 的 AMD64/ARM64 镜像启动测试。全部通过且该版本仍是当前通道最新版本，才移动该通道别名。镜像失败时可在 `Publish Docker images → Run workflow` 输入已发布的 `server-vX.Y.Z` 补跑，不重建下载归档。构建与测试不需要本地 Docker。

使用 Docker Hub 部署时，将 Compose 中的 `image` 分别改为 `docker.io/你的用户名/ohmycine-server:${OMC_IMAGE_TAG:-beta}` 或 `docker.io/你的用户名/ohmycine-node:${OMC_IMAGE_TAG:-beta}`，其余配置保持不变。

## 主 Server

在主服务器保存 `deploy/compose.server.yml`，执行：

```sh
docker compose -f compose.server.yml up -d
```

默认访问 `http://127.0.0.1:3000`。同目录 `.env` 中的 `OMC_PUBLIC_ORIGIN` 应与浏览器实际访问的 HTTP 或 HTTPS 地址完全一致，否则修改操作会被来源校验拒绝。局域网直连时同时设置 `OMC_BIND_ADDRESS` 为对应网卡地址。公网推荐 HTTPS 反向代理，保留 localhost 绑定。

`server-state` 卷保存数据库、加密密钥、插件及日志，备份迁移时整体保留。需要本地媒体库、STRM 或接收 Node 文件时，额外挂载对应目录，在页面使用容器内路径。容器使用 UID/GID `65532:65532`，宿主机绑定目录要授予对应读写权限。镜像包含 FFmpeg，路径 `/usr/bin/ffmpeg`。

## 公网 Node

主 Server 的受管浏览器组件、许可确认及缓存说明见 [浏览器组件部署](browser-companion.md)。Node 子系统镜像不捆绑该组件。

1. 在主 Server 的传输节点管理创建 Linux 节点，填写主 Server 能访问的公网 HTTP/HTTPS 地址。页面会显示一次性的一行安装命令和完整 Docker Compose；两种方式选一种即可。
2. 选择 Docker 时，直接复制页面显示的 Compose 保存为公网机器上的 `compose.yml`，限制该文件的读取权限，再运行 `docker compose up -d`。Node ID、十分钟有效的配对令牌、Docker Hub 镜像版本和 HTTP/HTTPS 模式已经写入示例，无需另建 `.env` 或手工填写 `OMC_IMAGE_TAG`。令牌过期后须在主 Server 重新生成并复制新配置。不要在日志、工单或公开仓库中粘贴含令牌的配置。
3. 确认主 Server 能访问所填地址和容器的 4433 端口；若公网地址未显式指定端口，则需让反向代理把该地址的 80/443 转到 Node 的 4433 端口。然后在主 Server 点击“完成配对”和“测试”。
4. 添加下载器时选择该子节点，填写 Node 容器能够访问的下载器地址。Docker Compose 只部署 OhMyCine Node，不安装下载器。

页面从保存的节点地址生成传输模式：`https://` 使用镜像默认 HTTPS，`http://` 自动写入 `OMC_NODE_TRANSPORT=http`。正式 HTTP 保留节点配对与业务授权，但不提供传输加密。使用仓库中的通用 `deploy/compose.node.yml` 而非页面生成示例时，仍需自行填写 ID、令牌，并在选择 HTTP 地址时设置对应传输模式。

首次启动自动生成身份证书和密钥；页面生成的 `node-state` 命名卷保存节点数据库、证书、封装密钥及受管文件，升级必须保留。命名卷避免新建 `./data` 绑定目录因宿主机 root 所有权导致 Node UID/GID `65532:65532` 无法写入身份文件。已有节点不能直接换卷而不迁移身份数据。默认无需手动申请域名证书。使用自有证书时，只读挂载证书目录并设置 `OMC_NODE_TLS_CERT`、`OMC_NODE_TLS_KEY` 为容器内路径；更换身份后需重新配对。

下载器与 Node 分属容器时，`localhost` 指 Node 自己。可以加入同一 Docker 网络并使用下载器服务名；不要公开下载器管理端口。将宿主机上的同一下载目录分别挂载进 qBittorrent 与 Node 容器，并在下载器配置中填写各容器看到的路径。两边可以使用相同路径，例如都挂载到 `/mnt/media/omc/downloads`，也可以使用不同路径；`OMC_NODE_MANAGED_ROOT` 仍保留给 Node 自己的受管文件，不限制 qBittorrent 的挂载位置。qBittorrent 负责写入下载文件，Node 需要对该目录及文件有读取权限以校验和导出；Docker 挂载与宿主机权限须实际允许访问。做种仍由原下载器执行。

## 更新与源码构建

```sh
docker compose -f compose.server.yml pull
docker compose -f compose.server.yml up -d
# 公网 Node 机器改用 compose.node.yml
```

在 `.env` 设置 `OMC_IMAGE_TAG=vX.Y.Z` 可固定版本。不要用 `down -v` 更新，否则会删除持久化卷。容器通过镜像更新，不使用程序内替换二进制；数据库升级后的降级需要兼容版本或配套备份。

源码本地构建（不发布）：

```sh
docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile --target local .
docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile.node --target local .
```

源码构建默认为开发版本，不含正式 TMDB 凭据和 Node 安装验证公钥。Server 可在部署时设置 `OMC_TMDB_READ_ACCESS_TOKEN` 或 `OMC_TMDB_API_KEY`。正式测试镜像应使用上述 Release 流程。
