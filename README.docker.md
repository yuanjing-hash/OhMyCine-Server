# OhMyCine Server Docker 部署

本文提供 Docker Compose 和 `docker run` 两种部署方式，二选一即可。镜像包含 Server 与 Web 管理端，不需要在宿主机安装 Go 或 Node.js。

- 镜像：`ghcr.io/yuanjing-hash/ohmycine-server`
- 发布架构：`linux/amd64`、`linux/arm64`
- `beta`：Beta 滚动标签；`stable` / `latest`：正式版发布后更新的标签。
- 固定版本使用 `vX.Y.Z` 镜像标签，具体版本以 [Server Releases](https://github.com/yuanjing-hash/OhMyCine-Server/releases) 为准；Release 标签本身是 `server-vX.Y.Z`。
- 本文部署的是 Server。分布式 Node 使用独立镜像和 [compose.node.yml](deploy/compose.node.yml)，单机部署无需额外启动 Node。

下面的终端命令使用 Linux/NAS 的 Bash。Windows Docker Desktop 请使用 Linux 容器；PowerShell 可将多行命令合成一行执行。

## Docker Compose

新建一个部署目录，在其中保存以下 `compose.yaml`：

```yaml
services:
  server:
    image: ghcr.io/yuanjing-hash/ohmycine-server:beta
    restart: unless-stopped
    init: true
    shm_size: "256mb"
    ports:
      - "127.0.0.1:3000:3000"
    environment:
      OMC_PUBLIC_ORIGIN: http://127.0.0.1:3000
    volumes:
      - server-state:/var/lib/ohmycine
    stop_grace_period: 60s

volumes:
  server-state:
```

启动并查看日志：

```bash
docker compose pull
docker compose up -d
docker compose logs -f --tail=100 server
```

在宿主机浏览器打开 `http://127.0.0.1:3000`，按引导创建首个管理员。也可以直接使用仓库自带的配置，在仓库根目录运行：

```bash
docker compose -f deploy/compose.server.yml up -d
```

自带配置支持 `OMC_IMAGE_TAG`、`OMC_BIND_ADDRESS` 和 `OMC_PUBLIC_ORIGIN` 插值。例如在 NAS 的局域网地址 `192.168.1.10` 上运行：

```bash
OMC_BIND_ADDRESS=0.0.0.0 \
OMC_PUBLIC_ORIGIN=http://192.168.1.10:3000 \
docker compose -f deploy/compose.server.yml up -d
```

后续维护必须沿用同一份 Compose 文件、项目名和环境配置；也可以把这些非敏感变量保存到 Compose 使用的 `.env` 文件。更换项目名可能创建另一个空数据卷。

### 局域网访问与端口

上面的最小配置只允许宿主机访问。供其他设备上的 Player 或 Emby 使用时，将 `ports` 改为 `"3000:3000"`，并把 `OMC_PUBLIC_ORIGIN` 改为 Server 实际可达地址，例如 `http://192.168.1.10:3000`。

使用宿主机端口 3300 时，映射写成 `"3300:3000"`，对外来源写成 `http://192.168.1.10:3300`；容器内部仍监听 3000。

`OMC_PUBLIC_ORIGIN` 同时用于浏览器来源校验、STRM 和 Emby 网关地址生成，应与实际访问协议、主机和端口一致，不带路径。`0.0.0.0` 是监听地址，不能用作对外来源。反向代理部署时填写实际 HTTPS 域名，并转发 WebSocket；公网访问应通过 HTTPS 反向代理。

## docker run

以下命令与最小 Compose 示例等价，不要与占用同一端口的 Compose 实例同时运行：

```bash
docker volume create ohmycine-server-state

docker run -d \
  --name ohmycine-server \
  --restart unless-stopped \
  --init \
  --shm-size=256m \
  --stop-timeout=60 \
  -p 127.0.0.1:3000:3000 \
  -e OMC_PUBLIC_ORIGIN=http://127.0.0.1:3000 \
  -v ohmycine-server-state:/var/lib/ohmycine \
  ghcr.io/yuanjing-hash/ohmycine-server:beta

docker logs -f --tail=100 ohmycine-server
```

局域网访问同样需要改成 `-p 3000:3000`，并设置实际的 `OMC_PUBLIC_ORIGIN`。

## 持久化数据在哪里

镜像将应用状态统一放在 `/var/lib/ohmycine`。上面的命名卷会保留这些数据，容器重建后仍可使用：

| 容器内路径 | 内容 |
|---|---|
| `/var/lib/ohmycine/data/ohmycine.db` | SQLite 数据库，包括配置、媒体库、识别元数据等 |
| `/var/lib/ohmycine/data/credentials.key` | 默认生成的凭据加密密钥 |
| `/var/lib/ohmycine/data/cache/artwork/categories/` | 系统生成的媒体库分类封面 |
| `/var/lib/ohmycine/logs/` | 运行日志 |
| `/var/lib/ohmycine/plugins/` | 插件文件 |
| `/var/lib/ohmycine/browser/` | 浏览器 companion 状态 |

命名卷由 Docker 管理，并不是部署目录下的 `./data`。可以用 `docker volume inspect <卷名>` 查看卷信息；Compose 的实际卷名通常带项目名前缀。

如果需要直接管理宿主机文件，可以把状态卷改成 `/srv/ohmycine/state:/var/lib/ohmycine`。镜像以 **UID/GID 65532:65532** 运行，需要提前创建专用目录并授予该用户写权限。例如仅对新建的应用状态目录执行：

```bash
sudo install -d -m 0750 -o 65532 -g 65532 /srv/ohmycine/state
```

已有 NAS 目录请使用 NAS 权限界面或 ACL 授权；不要递归修改整个媒体盘的所有权。

## 本地媒体和 STRM 目录挂载

应用状态卷不会自动挂载宿主机媒体目录。在 Compose 的 `volumes` 下按需要增加：

```yaml
      - /srv/media:/media
      - /srv/strm:/strm
```

`docker run` 对应增加 `-v /srv/media:/media -v /srv/strm:/strm`。宿主机目录需提前创建；只挂载实际需要的目录。

在 Server Web UI 中填写的是 **容器内路径**：

- 本地存储根目录：`/media`，而不是宿主机的 `/srv/media`。
- 网盘媒体库的 STRM 本地输出根目录：`/strm` 或其中的子目录。
- 仅读取本地媒体时可以挂载 `/srv/media:/media:ro`；启用本地 NFO/图片生成、整理或删除时，需要对应目录的写权限。
- STRM 输出目录需要 UID/GID 65532:65532 的写权限。
- Emby/Jellyfin 需要另外挂载并扫描生成的 STRM 目录；它们也必须能访问 `OMC_PUBLIC_ORIGIN`。

网盘扫描和 Player 读取媒体库不要求开启 STRM。识别元数据保存在数据库中，Player 通过 Server API 获取；当前影片图片通过 Server 图片代理按需获取，不会因为扫描就全部下载到状态卷。

开启元数据产物后，本地媒体库的 NFO/图片写在媒体旁；网盘媒体库的产物跟随 STRM 本地输出目录。全量扫描会校验已有受管产物，内容未变化且文件完整时复用，不会每次全量重写。

容器中的 `127.0.0.1` 指向容器本身。配置 Emby、OpenList/Alist、下载器等连接时，应使用容器可访问的宿主机地址，或同一 Docker 网络中的服务名。

## 升级、停止与备份

Docker 部署通过替换镜像升级，Web UI 中的 Server 更新由部署方式管理。

Compose 部署在原部署目录、沿用原配置执行：

```bash
docker compose pull
docker compose up -d
docker compose logs --tail=100 server
```

使用仓库配置时，为这些命令补上 `-f deploy/compose.server.yml`。固定版本升级则先修改镜像标签，再执行拉取和重建。

`docker run` 部署先备份，再拉取镜像、停止并删除旧容器，然后重新执行完整的启动命令，保持原来的数据卷、挂载和环境变量：

```bash
docker pull ghcr.io/yuanjing-hash/ohmycine-server:beta
docker stop ohmycine-server
docker rm ohmycine-server
# 重新执行前文 docker run，并保留你实际使用的所有参数
```

`docker rm` 不删除上述命名卷。Compose 日常停止使用 `docker compose stop`，移除容器使用 `docker compose down`；**不要添加 `-v`，它会删除命名卷中的应用数据。**

备份时先停止 Server，再备份完整状态卷或绑定目录，完成后启动。数据库与 `credentials.key` 必须一起保留；如果使用外部 `OMC_CREDENTIAL_MASTER_KEY`，另行安全保管该密钥。媒体、STRM 目录需单独备份。跨版本恢复时应配套使用对应版本的数据备份，不能假定旧镜像兼容已升级的数据库。

## 自行构建镜像

在 Server 仓库根目录执行：

```bash
docker build --target local -t ohmycine-server:local .
```

将部署配置中的镜像替换为 `ohmycine-server:local`。此构建显示 `dev`，不自动包含官方 Release 注入的 TMDB 应用凭据；需要时在 Web 设置中配置 TMDB，或设置运行时 `OMC_TMDB_READ_ACCESS_TOKEN` / `OMC_TMDB_API_KEY`（二选一）。

更多环境变量和非容器启动方式见 [主 README](README.md)。
