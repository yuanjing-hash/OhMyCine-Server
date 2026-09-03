# Emby/Jellyfin collection lifecycle research

## Sources inspected

- Jellyfin `CollectionPostScanTask` (the actively maintained Emby fork):
  https://github.com/jellyfin/jellyfin/blob/master/Emby.Server.Implementations/Library/Validators/CollectionPostScanTask.cs
- Jellyfin `CollectionManager`:
  https://github.com/jellyfin/jellyfin/blob/master/Emby.Server.Implementations/Collections/CollectionManager.cs
- Last public Emby `CollectionManager` implementation:
  https://github.com/MediaBrowser/Emby/blob/master/Emby.Server.Implementations/Collections/CollectionManager.cs

## Observed lifecycle

1. Automatic grouping is a library post-scan task, not a detail-page side effect.
2. It is enabled per library through `AutomaticallyAddToCollection`.
3. The task pages through physical movies, reads the movie metadata `CollectionName`, and ignores secondary versions (`PrimaryVersionId`).
4. Candidates are grouped by collection name across the scanned libraries.
5. A new Box Set is automatically created only when at least two distinct movies are present.
6. Existing Box Sets receive any newly discovered movie IDs through the same CollectionManager used by explicit user actions.
7. Manual create/add/remove operations are separate collection service operations.

## Adaptation for OhMyCine

- Run reconciliation immediately after a successful media-library persistence transaction, while the scan generation and complete/partial evidence are still known.
- Use TMDB collection ID as the stable key; localized name is display metadata, not identity.
- Store automatic and manual membership provenance separately. The upstream post-scan implementation shown above is append-oriented; OhMyCine should reconcile automatic members on complete scans so deleted media does not remain forever, while never deleting manual state.
- Deduplicate multiple versions by `{library_id, work_key}` and TMDB movie ID.
- Require two available distinct movies before exposing the auto collection, matching the established Box Set behavior.
- Partial scans may add proven members but cannot remove unseen members.
