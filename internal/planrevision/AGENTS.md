# `internal/planrevision`

Pure formatting helpers for proposed-plan revision comments. App code owns
comment CRUD, selected-text resolution, mode changes, and dispatch.

`BuildPrompt` preserves caller order, omits comments with neither selected text
nor body, and emits body-only or selection-only comments without inventing
content. `IDsOf` preserves input order.
