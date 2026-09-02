# Technical Design

## Boundaries

- `DownloadWorker` owns the distinction between an unsubmitted download and monitoring an established qBittorrent hash.
- `QueueService` owns only genuine Worker lease recovery and correction of legacy false terminals.
- `pkg/downloader` exposes an optional managed-tag cleanup interface; only qBittorrent implements it.
- qBittorrent adapter deletes one exact Server-owned tag through `POST /api/v2/torrents/deleteTags` and never enumerates or deletes arbitrary user tags.

## Established-task reconnect loop

When qBittorrent `Get()` returns a retryable provider error and DownloadTask has a real hash:

1. Persist a safe temporary connectivity diagnostic on DownloadTask without changing it to terminal failed.
2. Keep the current Worker/Job lease alive through the Scheduler's existing keepalive.
3. Wait on a context-aware bounded timer, then call `Get()` again with the same hash.
4. On success, persist telemetry and clear only the temporary provider connectivity diagnostic.
5. Never return `WorkerResult.RetryAt` from this established-hash branch.

The existing delayed queue retry remains for submission paths where no stable ProviderTaskID exists. A `tag:<omc-id>` placeholder remains part of ambiguous-submit adoption until a real hash is resolved.

## Process restart and legacy correction

- Genuine expired Worker leases remain restart-safe queue recovery.
- Expiry budgeting counts consecutive lease-expiry attempts rather than unrelated provider retry history.
- Legacy `failed/worker_lease_expired` rows are reopened only when their recorded consecutive lease-expiry streak is below policy and the normal download worker can continue from the stored ProviderTaskID.
- The first successful provider sample atomically aligns DownloadTask telemetry and the active Job diagnostic; it never changes a cancelled task.

## Managed tag lifecycle

Add an optional interface such as:

```go
type ManagedTagCleaner interface {
    DeleteManagedTag(context.Context, string) error
}
```

The qBittorrent implementation validates the exact persisted tag shape and calls `torrents/deleteTags` with only that tag. The worker invokes cleanup only after a real hash has been persisted. On success it clears the private DownloadTask `provider_tag`, which is the durable cleanup marker. Failure is logged safely and leaves `provider_tag` intact for a later bounded retry; it does not fail download or import.

A startup reconciliation pass selects a bounded page of qBittorrent DownloadTasks whose ProviderTaskID is a real hash and whose ProviderTag remains non-empty. It uses the task's configured downloader client, performs the same exact cleanup, and clears the marker only after upstream success. The pass is bounded and best-effort so Server startup and active downloads are not blocked indefinitely.

## Compatibility and safety

- No schema migration is needed because `provider_tag=''` can represent successful cleanup.
- Older qBittorrent versions that reject `deleteTags` keep the marker for a later attempt; downloads still complete.
- No torrent deletion, file deletion, category mutation, location mutation, or user-tag mutation is introduced.
- 115 and plugin downloaders do not implement the optional interface and are unaffected.

## Rollback

Removing the optional cleanup calls leaves tags intact but does not affect torrent identity. Reverting the reconnect loop returns to queue delayed retry behavior without changing stored ProviderTaskID or files.
