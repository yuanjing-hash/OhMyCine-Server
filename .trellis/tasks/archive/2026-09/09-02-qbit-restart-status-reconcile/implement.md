# Implementation Plan

1. Adjust `DownloadWorker` so retryable qBittorrent `Get()` errors for a real ProviderTaskID remain inside a context-aware monitoring loop and never return `RetryAt`.
2. Preserve delayed queue retry for unsubmitted/ambiguous submission paths and keep explicit terminal provider errors unchanged.
3. Retain and adapt consecutive lease-expiry recovery plus legacy false-terminal correction; ensure existing qBittorrent hashes are reclaimed without Submit.
4. Add the optional downloader managed-tag cleanup interface and implement exact qBittorrent `deleteTags` behavior with safe validation and adapter tests.
5. Invoke tag cleanup after real hash persistence, clear the private marker only after success, and make failure non-blocking.
6. Add a bounded startup reconciliation for existing qBittorrent tasks with a real hash and uncleared OhMyCine tag.
7. Add service tests for no-attempt reconnect, cancellation during reconnect, zero resubmission, genuine failures, exact tag deletion, cleanup failure/retry, and legacy cleanup.
8. Run targeted service/provider tests, then `go test ./...`, `go vet ./...`, Server Web UI test/typecheck/lint/build, embedded Go build, and `git diff --check`.

## Risk and rollback points

- The reconnect timer must always select on context cancellation.
- Tag cleanup must occur only after real hash persistence; deleting it earlier would break ambiguous-submit adoption.
- Clearing `provider_tag` before upstream success would make leaked labels unrecoverable.
- The startup cleanup query must be bounded and must not block Server lifecycle on one unavailable downloader.
