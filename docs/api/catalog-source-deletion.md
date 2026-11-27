# Catalog source deletion

The web media-library work menu and detail page open the same deletion dialog on the current page. Preparation reads the exact file list and shows its progress. The user still confirms the prepared list by entering the work title. Deletion completion updates the current catalog view and leaves the result dialog visible.

The existing endpoints remain compatible with JSON clients:

- `POST /api/v1/media-libraries/:id/catalog/:work/deletion-preview`
- `POST /api/v1/media-libraries/:id/catalog/:work/deletion-confirm`, body `{ "token": "<confirmation token>" }`

Both require the existing session, CSRF protection, and `media_libraries.media_delete` permission for the library. A preview now includes `deletion_id`, the opaque ID of its existing durable deletion record.

## Streaming progress

Send `Accept: text/event-stream` to receive the same operation as an SSE response. This remains one request, not an enqueued task. The stream sends:

- `event: progress`: `{ deletion_id?, phase, title, processed_files, total_files, removed_files, missing_files }`. Phases are `preparing`, `deleting`, `committing`, `completed`, or `interrupted`. Preparation counts files read, not files deleted.
- `event: result`: the normal preview or confirmation result. Only confirmation's `deleted: true` reports catalog deletion completion.
- `event: error`: `{ code, message }` with the service error code and safe user-facing message.
- Keep-alive comments while a provider call is pending.

The stream renews the HTTP write deadline; its whole operation has no 45-second batch cutoff. Each provider stat/recycle call retains the existing 45-second bound. 115 reads use the existing interactive class. Preparation reuses directory reads only within that preparation request. Confirmation reads every file and ancestor again before recycling it. Existing frozen identity, physical-write ownership, per-file checkpoints and catalog publication remain in effect.

## Reading a saved result

`GET /api/v1/media-libraries/:id/catalog/:work/deletion-status/:deletion` returns the progress shape, with phases `ready`, `deleting`, `interrupted`, or `completed`. It reads only the original actor's record scoped to the same library and work and uses the same deletion permission. It does not create a preview or replay a delete. The response excludes confirmation tokens, provider IDs and physical paths.

Once confirmation starts, the browser URL keeps only `deletion`, `deletionLibrary`, and `deletionWork` so refreshing can read the saved result even after the work itself is gone. Closing the dialog does not repeat or abort a confirmation already in flight. Reloading or disconnecting may interrupt that request; reopening reports its actual saved checkpoint and offers to read a new list before any further explicit confirmation. There is no automatic mutation retry.

Ordinary JSON errors distinguish changed identity (`409`), provider availability (`503`), and expired confirmation (`410`). A timeout or provider read failure does not claim that media files changed or require a full library scan.
