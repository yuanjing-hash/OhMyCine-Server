# Server 安全自动更新设计

## Architecture

新增四个边界：

1. `internal/buildinfo`：只读当前版本和构建提交，Release workflow 通过 `-ldflags -X` 注入。
2. `internal/updater`：固定官方 GitHub Release 客户端、版本选择、下载/checksum、归档提取、plan/state 文件和 helper 执行器；不依赖 Gin/GORM。
3. `internal/services/UpdateService`：管理员授权、单飞状态机、通道设置、审计/日志、启动 helper 与请求优雅停服。
4. `internal/handlers` + WebUI 设置面板：薄 API 和状态展示。

```text
Admin UI
  -> GET/check/install/settings
  -> UpdateService (admin + singleflight)
  -> updater GitHub client (fixed repository and hosts)
  -> .runtime/updates/staging/<operation-id>/candidate
  -> staged candidate --ohmycine-update-helper <plan>
  -> old Server graceful shutdown
  -> backup current executable -> replace -> restart -> health
  -> success OR kill candidate -> restore backup -> restart old
```

## Source and release selection

- API endpoint is the fixed public GitHub REST release list for `yuanjing-hash/OhMyCine-Server`.
- Strict release identity requires `tag_name=server-vX.Y.Z`; asset names derive only from parsed version and `runtime.GOOS/GOARCH`.
- Supported self-update targets are `windows/amd64` and `linux/amd64`, matching the current release workflow.
- Beta selects the greatest version among prerelease and stable releases; Stable filters prerelease first. Drafts and duplicate/malformed assets are rejected.
- `dev`/unknown builds may check and display latest but installation is disabled unless an explicit comparable official current version is present. The first updater-capable Beta therefore becomes the baseline for later self-updates.

## Network and archive safety

- A dedicated `http.Client` enforces HTTPS, exact host allowlist, five redirects, timeouts and response limits. Redirect targets are revalidated every hop.
- GitHub JSON and checksum text use small independent limits; archive download uses a larger fixed cap and streams through SHA-256 into a newly created staging file.
- The checksum line must name the exact platform asset once and contain exactly 64 hex characters.
- Extraction canonicalizes slash-separated paths and rejects absolute/drive/UNC paths, `..`, links, devices and unsupported entries. Only one exact `ohmycine-server(.exe)` under the expected top-level release directory is accepted. The candidate is copied with bounded size into staging and fsynced before use.

## Persistent update state

No business database migration is introduced. Non-secret files live under `<runtime>/updates`:

- `settings.json`: channel and revision.
- `state.json`: public-safe lifecycle facts (phase, versions, timestamps, stable error code).
- `plans/<operation-id>.json`: private helper plan with executable paths, parent PID, health URL and original args.
- `staging/<operation-id>/`: downloaded archive and extracted candidate.
- `backups/`: bounded previous executable backup.

Files use create-new/temporary-write + sync + atomic rename where supported. API DTOs project only safe state and never serialize plan paths or release URLs.

## Process handoff and rollback

- `cmd/server` checks for the private helper flag before config/database initialization.
- Install prepares and verifies the candidate while the current Server remains online, writes the plan, starts the staged candidate helper, then sends an internal graceful-shutdown signal after the HTTP response can flush.
- Helper waits for the exact parent PID to exit with a deadline. It renames the installed binary to a unique backup, renames the candidate into place, starts the installed binary with the original non-helper arguments and inherited runtime environment, and probes loopback health.
- Failure before candidate start restores immediately. Failure after candidate start terminates/waits for it, moves the failed candidate aside, restores backup, restarts the old binary and records `rolled_back`.
- The helper never deletes `.runtime`. Cleanup is limited to known operation staging directories and bounded old binary backups; cleanup failures are non-fatal and safe.

## Deployment-managed detection

- Containers (`/.dockerenv` or explicit `OMC_UPDATE_MODE=managed`) and unsupported platforms report `deployment_managed`.
- A read-only/unreplaceable executable is detected during install preflight and returns `deployment_managed`; check/status remain available.
- There is no attempt to call Docker, systemd, package managers or repository Git commands.

## API contract

```text
GET   /api/v1/system/update
POST  /api/v1/system/update/check
PATCH /api/v1/system/update/settings  {channel, revision}
POST  /api/v1/system/update/install   {target_version}
```

All routes are authenticated, no-store and require `system.admin`. Install is asynchronous and returns accepted safe state. The browser polls status, tolerates disconnect during restart, then reloads auth/bootstrap after health returns.

## Compatibility and rollout

- Existing `.runtime` layout and `start.ps1/start.sh` continue unchanged.
- Official artifacts retain the existing archive/checksum names; only linker version metadata is added.
- Current pre-updater binaries cannot self-update. The next Beta is installed normally once; all later official versions can use the new flow.
- Stable channel becomes useful when a non-prerelease Server Release exists and requires no code change.

## Risks and mitigations

- Repository/Release compromise can replace both archive and checksum. Fixed repository/hosts and exact assets reduce substitution risk, but checksum is not an independent signing root; signed manifests can be added later without weakening this contract.
- A new version may introduce a non-backward-compatible database migration. This task adds no migration and documents that release migrations must remain rollback-compatible; schema snapshot/restore is intentionally not attempted while live.
- Power loss between binary rename operations can leave backup plus missing target. The helper state and uniquely named backup make manual recovery possible; tests cover each operation boundary.

