# Implementation Plan

- [x] Reproduce and fix DownloadsView route-preview reset; add pure parsing tests.
- [x] Add batch download contract, service, handler, route and integration tests.
- [x] Add v60 additive persistence and DTO fields for structure diagnostics/repair runs.
- [x] Implement provider-neutral structure planner with movie/TV/sidecar tests.
- [x] Implement local and 115 repair backends with confinement, conflict, identity and empty-directory tests.
- [x] Register repair worker, API routes and initial post-scan diagnosis; integrate work-scoped pre-transfer repair.
- [x] Add media-library structure UI prompt/status/list/retry controls.
- [x] Implement `MediaMetadataEditor`, metadata endpoints and transactional projection/artifact change handling.
- [x] Replace the simple metadata candidate dialog with tabs for identification, fields and image selection.
- [x] Update API/types/spec/architecture documentation; no permission catalog change was required.
- [x] Run focused tests, full WebUI gates, `go test ./...`, `go vet ./...`, builds and diff checks.
- [x] Commit, archive the Trellis task and merge the feature branch into local `develop` without pushing or publishing.
