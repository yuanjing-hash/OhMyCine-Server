# Technical design

## 1. Boundaries

The Server owns durable multi-device state. The Player keeps its current SQLite history and local collections so offline and Server-less use remain intact. Integration uses `/api/v1/player/*` device-Bearer routes only.

## 2. History

Keep `player_playback_history` and its revision log as the merge authority. Add a stable cursor/page read method that filters `deleted = false`, orders by `client_updated_at DESC, sync_key ASC`, and returns safe history DTOs. The existing sync endpoint remains the bidirectional delta protocol.

Player dispatches `PLAYED_STATE_CHANGED_EVENT` after successful local history mutations. The sync scheduler coalesces events for two seconds. It records a bounded, redacted diagnostic on success/failure instead of swallowing every error. For `ServerDataSource.listPlaybackHistory`, a plugin library selection continues to use `/online-history`; a Server-level selection uses the new persisted history page.

## 3. User favorites and manual collections

Persistence:

- `player_media_favorites`: `(user_id, library_id, work_key)` primary identity, timestamps.
- `player_media_collections`: UUID, nullable owner (`NULL` for system TMDB collections), source (`tmdb|manual`), kind (`collection|playlist`), name, TMDB collection metadata, visible flag, revision/timestamps.
- `player_media_collection_items`: collection ID, library ID, work key, optional TMDB movie ID, origin (`tmdb|manual`), ordinal, timestamps.

System TMDB collections are readable by every authenticated user but their returned members are filtered by media-library authorization. Manual collections are private to their owner. Favorite and collection mutations first resolve a Server work ID to an accessible catalog work; arbitrary client labels or paths are never trusted.

## 4. TMDB automatic collection projection

Extend the credential-free `tmdb.Snapshot` with a bounded collection projection: ID, name, poster path and backdrop path. Only the movie detail response reads `belongs_to_collection`; TV metadata remains unchanged.

After a scan's entries and recognition snapshots have been saved:

1. Select matched movie works in the current library and parse their persisted collection projection.
2. Collapse versions by work key and TMDB movie ID.
3. Upsert the system collection by TMDB collection ID and refresh only its TMDB-owned display fields when it is not locked.
4. Upsert proven `tmdb` members.
5. On non-partial scans, remove `tmdb` members for that library that are no longer present; never touch `manual` members.
6. Set visibility from the count of distinct available movies (`>= 2`).

The collection reconciliation belongs in the same database transaction as the successful scan projection. A rollback therefore cannot expose a collection newer than its catalog.

## 5. API

- `GET /api/v1/player/history?page=&page_size=&source_kind=`
- `GET /api/v1/player/favorites`
- `GET /api/v1/player/favorites/:itemId`
- `PUT /api/v1/player/favorites/:itemId` with `{favorite}`
- `GET /api/v1/player/collections?kind=`
- `POST /api/v1/player/collections`
- `GET /api/v1/player/collections/:id/items`
- `POST /api/v1/player/collections/:id/items`
- `DELETE /api/v1/player/collections/:id/items/:itemId`
- `DELETE /api/v1/player/collections/:id`

Responses use the standard envelope and bounded page/list sizes. Mutations are idempotent where the desired final state is supplied.

## 6. Compatibility and rollback

New migrations only add tables and TMDB snapshot JSON fields. Older Players ignore new capabilities. New Players keep local fallbacks when endpoints are absent. Rollback leaves additive tables unused and does not alter playback.
