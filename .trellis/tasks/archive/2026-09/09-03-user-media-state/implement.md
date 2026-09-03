# Implementation plan

## Phase 1 — History delivery and visibility

- [x] Add stable, user-scoped Server history pagination and route tests.
- [x] Trigger Player history sync after successful local mutations and expose bounded sync diagnostics.
- [x] Route Server-level history UI to persisted Server history while preserving plugin history filters.

## Phase 2 — TMDB projection and schema

- [x] Parse and persist bounded `belongs_to_collection` metadata for TMDB movies.
- [x] Add SQLite migration for favorites, collections and provenance-aware members.
- [x] Add migration/idempotency/user-isolation tests.

## Phase 3 — Collection reconciliation

- [x] Implement transaction-scoped post-scan auto-collection reconciliation.
- [x] Deduplicate media versions and enforce the two-distinct-movie visibility threshold.
- [x] Prove complete-scan removal and partial-scan preservation behavior.
- [x] Prove manual collections/members are untouched by TMDB reconciliation.

## Phase 4 — Player APIs and integration

- [x] Add thin handlers and device-Bearer routes for favorites and collections.
- [x] Implement ServerDataSource favorite and provider-collection methods.
- [x] Include Server in FavoritesView and use existing collection-selection UI.
- [x] Add safe parsers and contract tests.

## Validation

- [x] `go test ./...`
- [x] `go vet ./...`
- [x] `go build ./cmd/server`
- [x] Player `npm run typecheck`
- [x] Player `npm run lint`
- [x] Player `npm run build`
- [x] Player `cargo check --manifest-path src-tauri/Cargo.toml`

## Implementation evidence

- v65 adds user favorites, system/manual collections, and provenance-aware members without modifying v59 history rows.
- `TestFirstSuccessfulScanCreatesTMDBCollection` exercises the real scan pipeline and proves first-scan creation.
- Lifecycle tests prove multi-version deduplication, partial preservation, complete-scan pruning, two-film visibility, and manual-state isolation.
- Full Server Go tests, vet, build, Player typecheck/lint, Server data-source/media-action contracts, DirectML tests, and frame-interpolation contracts pass on 2026-09-03.

## Rollback points

- History read/diagnostics can ship independently from collection tables.
- Collection tables are additive and can remain unused if Player integration is rolled back.
- Auto-reconciliation can be disabled without deleting manual state.
