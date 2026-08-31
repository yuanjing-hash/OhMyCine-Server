# Server 安全自动更新实施计划

## 1. Build identity and Release contract

- [x] Add `internal/buildinfo` strict version model and tests.
- [x] Inject version/commit into Windows and Linux official binaries.
- [x] Extend Release guard tests for linker identity and immutable asset names.

## 2. Updater core

- [x] Implement strict channel/release selection and semantic comparison.
- [x] Implement fixed-host bounded GitHub client, redirect policy and response limits.
- [x] Implement checksum parsing, streaming archive verification and safe ZIP/tar.gz candidate extraction.
- [x] Implement atomic settings/state/plan files under runtime `updates/`.
- [x] Implement helper wait/backup/replace/restart/health/rollback with injectable process/filesystem/HTTP seams.
- [x] Add adversarial network/archive and success/rollback tests, including `.runtime` sentinel preservation.

## 3. Server integration

- [x] Add `UpdateService` singleflight state machine, admin authorization, safe audit and structured logging.
- [x] Add graceful internal update shutdown channel to `cmd/server` and early helper mode.
- [x] Add no-store admin-only status/check/settings/install handlers and route-operation coverage.
- [x] Add service/handler permission, concurrency, DTO redaction and error mapping tests.

## 4. Web management UI

- [x] Add typed update API state and error labels.
- [x] Add Settings page panel for version/channel/check/install/reconnect flow, gated by `system.admin`.
- [x] Add tests for permissions, stale requests, restart polling, managed deployments and responsive states.

## 5. Documentation and validation

- [x] Update architecture 02/07/08, roadmap, README/operations notes and Trellis executable specs.
- [x] Run `gofmt`, targeted tests, `go test ./...`, `go vet ./...`, `go build ./cmd/server`, `go build -tags webui ./cmd/server`, and configured linter.
- [x] Run WebUI `permissions:check`, tests, typecheck, lint, build and nested Go module verification/tests.
- [x] Run Release guard/actionlint and Windows launcher tests.
- [x] Perform an isolated Windows runtime smoke with a fake local Release server/helper fixture; never mutate the owner's live `.runtime` during tests.

## Rollback points

- Product-code rollback is a normal Git revert; updater state files are additive and non-secret.
- Runtime helper never targets the repository or runtime data. A failed live replacement restores the uniquely named old binary before reporting completion.
- Do not publish a Beta until all gates pass and the new Server repository has the required official TMDB Secret.
