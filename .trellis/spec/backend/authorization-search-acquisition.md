# Authorization, Search, and Acquisition

## Scope

Use this contract when changing user authorization rules, Player bootstrap capabilities, PT/BT search orchestration, download/subscription creation, or media acquisition status.

## Authorization contract

- Built-in roles are templates. Applying one copies its current permissions; later template changes never rewrite custom roles.
- Effective permission is the union of role grants plus user allow rules minus user deny rules. Deny wins.
- A service that operates on a site, downloader, or media library must call the shared authorizer with the exact resource type and stable resource ID. Route middleware is only an early rejection layer.
- List/default endpoints filter inaccessible resources instead of returning choices that will later fail. Mutations re-check the selected resource at execution time.
- `/api/v1/player/bootstrap` derives capabilities from the authenticated device user's current effective permissions. It must not return a static capability list.

## Progressive search contract

- Title and TMDB-identity searches use the same Server-lifetime coordinator and shared bounded concurrency semaphore.
- Each site has an independent timeout and cancellation context. One site failure must not cancel successful sites.
- Multilingual aliases are bounded and deduplicated within a site and continue to use that site's rate limiter.
- SSE emits only `media`, `progress`, `site`, `done`, and `error`. Progress is a complete monotonic count snapshot; site results may arrive incrementally, while final ordering remains deterministic by configured priority and site identity.
- JSON fallback and SSE use the same coordinator result projection. Claims are actor/site-bound, opaque, expiring, and revalidated before recognition or download.

## Acquisition contract

- The stable aggregate identity is owner + media type + TMDB ID.
- Download, follow, transfer, and library projections advance the aggregate idempotently and survive restart.
- Confirmation freezes downloader, target library, classification and follow options. Retries do not silently adopt new defaults.
- Player and Server Web consume the same safe DTO. Resource-scoped denial hides inaccessible resource IDs while preserving a non-sensitive stage summary.
- Never persist or return site credentials, upstream bodies, provider file identities, torrent URLs, local absolute paths, or temporary playback URLs in acquisition state.

## Required tests

- Role grants, user allow, deny precedence, global/scoped rules, and service-layer bypass attempts.
- Bootstrap capability changes after authorization changes.
- Shared concurrency ceiling, real overlap, per-site timeout isolation, cancellation, stable order, progress/result counts, and single-site retry.
- Acquisition projection for download, transfer completion/failure, follow state, and inaccessible target libraries.
