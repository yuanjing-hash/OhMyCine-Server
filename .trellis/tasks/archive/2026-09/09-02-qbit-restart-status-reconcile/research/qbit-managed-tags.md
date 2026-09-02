# qBittorrent managed tag cleanup evidence

- Submit currently attaches the private stable `DownloadTask.ProviderTag` as `tags` in `POST /api/v2/torrents/add`.
- The tag is used by `Get("tag:" + tag)` before and after Submit to adopt an upstream task when the add response is ambiguous or lost.
- Once `DownloadTask.ProviderTaskID` contains a real qBittorrent hash, all polling, pause, resume, cancel, manifest and seeding operations use that hash; the unique OhMyCine tag is no longer required for identity.
- The current adapter implements add/query but no tag cleanup endpoint.
- qBittorrent Web API provides `POST /api/v2/torrents/deleteTags` with an exact `tags` form value. Deleting the exact unique `omc-<task-id>` definition removes only OhMyCine's task tag; it must not be used with arbitrary enumeration or user labels.
- `DownloadTask.ProviderTag` is private and can double as the durable cleanup marker: non-empty means cleanup remains; empty means the exact upstream tag was deleted successfully.
