# AGENTS.md

This repository contains the independent OhMyCine Server automation engine and the future `omc` CLI.

## Repository responsibility

- The Go Server module lives at the repository root.
- `webui/` is the Vue 3 administration console and a nested Go module used for SPA embedding.
- `cli/` is reserved for the Server operations CLI.
- Player development belongs in `yuanjing-hash/OhMyCine`.
- Official plugin, Plugin SDK and Hub development belongs in `yuanjing-hash/OhMyCine-Plugins`.
- Player and Server integrate only through versioned HTTP/WebSocket contracts.

Core pipeline:

```text
Discover → Download → Transfer → Import → Notify
Connections → Storage Destinations → Category Rules
```

## Read before writing

- `.trellis/workflow.md`
- `.trellis/spec/backend/index.md`
- `.trellis/spec/frontend/index.md` for `webui/` changes
- `docs/architecture/07-security-design.md` for credentials, proxy, file operations, plugins or permissions

## Development commands

Use Windows PowerShell locally. Docker is not a local prerequisite.

```powershell
go mod download
go test ./...
go vet ./...
go build ./cmd/server
go build -tags webui ./cmd/server

cd webui
npm ci
npm run permissions:check
npm run test
npm run typecheck
npm run lint
npm run build
go mod verify
go test .
```

The full isolated Windows gate is `./test.ps1`. Do not delete or reset existing runtime data to make tests pass.

## Implementation conventions

- Use Go 1.23+ and `context.Context` for external or long-running work.
- Keep HTTP handlers thin; business behavior belongs in `internal/services`.
- Keep provider-specific behavior behind interfaces under `pkg/`.
- SQLite is the default database and related writes use GORM transactions.
- REST routes use `/api/v1/` and the standard `{code,message,data}` envelope.
- Credentials are encrypted at rest and never logged or returned.
- Constrain file operations to configured roots and defend against traversal and symlink escape.
- `/proxy/*` requires signed, authenticated or explicitly trusted-LAN access.
- Preserve Player independence; do not add a source dependency on the Player repository.
- OpenAPI and architecture docs change with their contracts.

## Git and release rules

- `develop` is the integration and Server Beta source branch.
- `main` is the stable source branch.
- Server tags use `server-vX.Y.Z`; never use Player `vX.Y.Z` tags here.
- Do not push or publish unless the user explicitly requests it.
- Commit format is Conventional Commits with a Chinese subject/body, for example `feat(server): 添加自动更新`.
- Server Beta artifacts are built only by `.github/workflows/server-beta-release.yml` from the latest remote `develop` commit.

<!-- TRELLIS:START -->
# Trellis Instructions

This project is managed by Trellis. Working knowledge lives under `.trellis/`:

- `.trellis/workflow.md` — development phases and task lifecycle
- `.trellis/spec/` — backend and Server Web UI guidelines
- `.trellis/workspace/` — per-developer journals
- `.trellis/tasks/` — active and archived task artifacts

Codex integration lives under `.codex/` and reusable project skills under `.agents/skills/`.

<!-- TRELLIS:END -->
