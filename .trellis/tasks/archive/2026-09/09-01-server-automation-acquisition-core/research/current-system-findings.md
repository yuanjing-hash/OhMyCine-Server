# Current system findings

## Search

- `internal/services/site.go::SearchEach` and `site_identity_search.go::SearchMediaIdentityEach` already launch goroutines with a request-local semaphore of 4.
- Because the semaphore is request-local, simultaneous users can multiply total concurrency; it is not a Server-wide bound.
- SSE currently emits only `media`, `site`, and `done`; there is no queued/running/completed/result_count contract.
- MediaIdentity keeps aliases serial inside a site, which preserves per-site limiting. The missing UX is explicit progress and immediate stream consumption, not unbounded alias concurrency.
- JSON aggregation sorts MediaIdentity groups by priority/ID; streaming completion order is nondeterministic and must not become final display order.

## Directory repair residue

- Recent planned source video/subtitle/artwork paths no longer exist, proving core assets were moved.
- Residue consists of inactive managed NFO artifacts in old title folders while regenerated NFO files are active in new folders.
- Automatic obsolete artifact cleanup in `internal/services/strm_management.go` is gated by `STRMEnabled`, so ordinary local metadata libraries skip deletion.
- Structure diagnosis does not include inactive managed artifacts, allowing cleanup residue to project `healthy`.
- Observed ACLs grant the current user modify rights; persistent ACL corruption was not proven. Windows sharing violations during artifact work need distinct handling.

## Player bridge implication

- Player's Tauri `server_request_json` buffers the entire response and has one fixed 20-second timeout.
- A real progress bar requires a native authenticated SSE bridge or equivalent bounded streaming command; changing Vue alone cannot expose Server progress.
