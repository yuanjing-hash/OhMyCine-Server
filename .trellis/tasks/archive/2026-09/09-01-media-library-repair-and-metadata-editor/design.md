# Technical Design

## 1. Download form stability and batch submission

- Split background task polling from route-preview state. A route preview is keyed by downloader ID and source kind; polling reuses it and preserves `mediaLibraryID` when the target remains enabled.
- Add pure frontend parsing for newline-delimited sources: trim, ignore blanks, de-duplicate while preserving order, max 50.
- Add `POST /api/v1/downloads/batch`. The handler accepts shared route fields plus URL/share sources. `DownloadService.SubmitBatch` invokes the existing `Submit` authority for each entry and returns indexed per-line outcomes. Successful rows are cleared; failed rows remain visible.

## 2. Media-library structure domain

Introduce `MediaLibraryStructureService`, composed with the existing `MediaLibraryService` rather than adding storage switches to it.

```go
type MediaLibraryStructureBackend interface {
    StorageType() string
    Apply(context.Context, StructureBoundary, []StructurePlanItem, StructureProgress) error
}
```

- `StructurePlanner` is provider-neutral. It consumes persisted `MediaLibraryEntry`, associated `MediaLibrarySourceAsset`, current recognition snapshot and current Profile naming templates.
- Desired paths are generated through the existing transfer template renderer and fixed media-type root normalization.
- Sidecars follow the same stem/directory association rules as normal transfer and managed reorganization.
- A plan records private stable identities and expected sizes, plus a safe public projection with relative old/new paths.
- Local backend canonicalizes the library root, rejects symlink/reparse escape, moves one exact file at a time, then removes only empty ancestors below the root.
- 115 backend resolves the configured library root, revalidates every provider item, creates target directories through cached stable IDs, moves/renames exact items, then recycles only proven-empty obsolete directories below the library root.

## 3. Durable repair state

Migration v60 adds:

- structure status/count/check timestamps to `media_libraries`;
- `media_library_structure_repairs` containing owner, library/work scope, rule/generation fingerprint, private plan/checkpoint, phase, counts and safe error code.

The queue job payload contains only the repair ID. Full-library repair is explicitly queued by the user. `TransferWorker` calls the same service inline for an already-indexed matching TMDB work before planning the incoming transfer; this operation is already protected by the durable transfer job and is recorded as an automatic work-scoped repair.

After a complete scan, structure diagnosis runs over the committed catalog and updates only diagnostic fields. It never mutates files. Repair completion increments dirty generation and invokes ordinary reconciliation so entries, artifacts and notifications converge normally.

## 4. Metadata editor

Introduce `MediaMetadataEditor`, a pure validator/normalizer over `tmdb.Snapshot`. It accepts bounded editable fields and only validated TMDB image identities (`/file.ext`). Identity fields (`TMDBID`, media type) remain controlled by existing manual-recognition endpoints.

New endpoints:

- `GET /api/v1/media-libraries/:id/catalog/:work/metadata`
- `PUT /api/v1/media-libraries/:id/catalog/:work/metadata`

The read DTO returns a complete safe editable snapshot and known poster/backdrop options. Saving sets the recognition to manual preservation, updates linked entry projections transactionally and reuses the existing metadata-change/artifact barrier.

## 5. Safety and rollback

- Diagnosis is read-only; apply uses immutable plan fingerprints and per-item reconciliation.
- Existing target conflicts fail closed; no overwrite is implicit.
- Checkpoints make retries converge after partial moves.
- Browser DTOs never expose provider IDs, absolute paths, plan JSON or credentials.
- Migration is additive; disabling/removing the feature leaves existing media catalog behavior intact.

