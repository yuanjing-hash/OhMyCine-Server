# Server Web UI Development Guidelines

> Long-lived implementation rules for the Vue administration console under `webui/`.

---

## Overview

The Server Web UI is an information-dense administration surface for the automation engine. It is not the Player application and does not inherit Player-only Cinema OS or native playback rules.

## Pre-Development Checklist

1. Read [Server Admin Web UI](./server-admin-ui.md) before changing any view, dialog, navigation item or theme token.
2. Keep API and permission contracts synchronized with the Go backend and generated permission catalog.
3. Use Vue 3 Composition API and strict TypeScript; keep reusable logic outside large view components.
4. Never persist credentials, signed URLs, provider headers or local absolute paths in browser storage.
5. Update `docs/architecture/08-server-web-ui-design.md` when administration flows or boundaries change.

## Guidelines Index

| Guide | Description | Status |
|-------|-------------|--------|
| [Server Admin Web UI](./server-admin-ui.md) | Console layout, dialogs, accessibility and verification | Active |
| [Server Online Media](./server-online-media.md) | Online libraries, history, progress and stream boundaries | Active |

## Quality Check

Run from `webui/`:

```text
npm run permissions:check
npm run test
npm run typecheck
npm run lint
npm run build
go mod verify
go test .
```

**Language**: Trellis spec files are written in English. Product-facing architecture docs may remain Chinese.
