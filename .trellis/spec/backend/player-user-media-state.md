# Player User Media State

## Scope

This contract applies to durable Player history, favorites, manual collections,
and Server-derived TMDB movie collections exposed through device-Bearer routes.

## Ownership and authorization

- Playback history and favorites are keyed by the authenticated user. Reads and
  writes must never return another user's rows.
- Manual collections have exactly one user owner. Only that owner may list,
  mutate, or delete them.
- TMDB collections are system-owned. Their members are filtered through the
  requesting user's current media-library read authorization before returning.
- Client item labels and paths are not identities. Mutations resolve a bounded
  opaque work token to a currently readable Server catalog work.

## Scan lifecycle

- `belongs_to_collection` is stored as a bounded, credential-free TMDB snapshot
  containing only ID, name, poster file identity, and backdrop file identity.
- Automatic collection projection runs inside the successful media-library scan
  transaction after recognition and entry persistence and before commit.
- The first successful scan establishes collection state. Later scans are
  idempotent reconciliations, not detail-page side effects.
- Collapse physical versions by stable work key and TMDB movie ID. A system
  collection becomes visible only when at least two distinct available movies
  are present.
- Partial scans may add proven TMDB members but cannot delete unseen members.
  Complete scans may remove only `origin=tmdb` rows proven absent from that
  library and recalculate visibility.
- Scan reconciliation never changes manual collections or `origin=manual`
  membership.

## History delivery

- Local Player history remains the offline fallback. A successful local mutation
  schedules Server synchronization through the shared change event.
- Re-uploading the same non-terminal timestamp is idempotent. At an equal
  timestamp, terminal completion/deletion may win once.
- History pages are user-scoped, exclude tombstones, and order by client update
  time descending with sync key as the stable tie-breaker.
- Synchronization diagnostics are bounded and redacted; tokens, stream URLs,
  credentials, provider identities, and local paths must not be recorded.

## Required verification

- Migration tests cover fresh, upgrade, and repeated application.
- Tests prove user isolation for history, favorites, and manual collections.
- Scan tests prove first-scan creation, multi-version deduplication, two-film
  visibility, complete-scan removal, partial-scan preservation, and manual-state
  isolation.
- Run `go test ./...`, `go vet ./...`, `go build ./cmd/server`, Player typecheck,
  lint, build, and Server data-source/media-action contract checks.
