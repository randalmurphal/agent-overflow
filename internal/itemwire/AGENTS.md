# internal/itemwire

The wire projection for timeline items. A stored row is complete; the
copy a client receives is bounded. Governing spec:
`docs/specs/remote-access.md` §14.

## What it does

Three things, all on the way OUT and none of them to SQLite:

- **`meta.input` elision.** Over `MetaMaxBytes`, drop the largest leaves
  until under budget. JSON-aware: keys, nesting and array indices
  survive, so a consumer reading a sub-field finds the same value or an
  absent key. Never a truncated JSON string.
- **Inline diff previews.** With previews off, or over
  `InlinePreviewMaxBytes`, drop each file's `previewPatch` and keep its
  chrome — path, rename source, change kind, insertion and deletion
  counts, truncation flag — so the collapsed row is unchanged.
- **A typed marker for both.** `wireElision` on `items.meta` names the
  dropped `input` paths; `previewElided` on the file entry names a
  dropped patch.

## Rules

- **A projection is not a storage shape.** `internal/itemmeta` owns
  shaping on the persist path. Nothing here writes, and nothing here
  runs before a write.
- **Every marker ships with its route out.** A value removed here is
  returned by `App.GetThreadItemProjectionSource`, which is deliberately
  NOT projected — it is the way out of the projection. A new marker
  without a route is not a smaller payload, it is a missing value.
- **Identity is retained, content is elided.** `retainedIdentityKeys` is
  the inventory of `meta.input` keys some surface renders in FULL —
  paths, skill, query, recipient, questions, command. Every other reader
  caps its output far below `LeafFloorBytes`. Both halves of that claim
  are pinned by tests: `TestProjectMeta_IdentityKeysSurviveWhateverTheir
  Size` here, and the frontend's `metaInputLeafRenderCaps.test.ts`, which
  fails when a capped reader loses its cap or an unnamed uncapped reader
  appears. A new uncapped reader means a new key AND a new case there.
- **Budgets skip, they do not break.** A candidate that cannot be
  dropped never stops the walk over later candidates, so one giant value
  cannot leave a row's other giant values on the wire. Same rule as
  `buildPersistedCodeSpansMaxBytes` in `internal/highlightapp`.
- **The preference rides the request, never the settings.** One backend
  serves several clients that can disagree about inline previews, so
  `inlinePreviews` is a parameter on every item-window RPC. The server
  never reads `collapseDiffPreviews`.
- **Under budget is byte-identical.** The fast paths are a length check
  and a `strings.Contains`; the ordinary row is not parsed, not
  re-encoded, and not allocated for. That is what lets `Project` sit on
  the live item-upsert path.

## Where it is applied

Every path that hands items to a client, or a window ends up with mixed
rows in it:

- the slice and cursor pagers, `ListItems`, `GetThreadItem`, and the
  subagent/plan/background-task readers (`internal/app/app_paging.go`,
  `app_item_projection.go`)
- `SyncThreadWindow`, including the replica write-back it feeds
- `GetThreadLiveState`'s deferred items
- item upsert and patch events (`internal/triage/item_events.go`), at
  the constructors rather than the emit sites, so a new emitter cannot
  forget. The event bus encodes once for every subscriber, so a push
  frame carries the preference-independent half — the byte budgets —
  with previews left on.

Streaming deltas are deliberately outside: they carry text, not JSON
blobs, and are the hot path.

## The byte backstop

`internal/app/app_item_projection.go` admits rows until the row count or
`itemWindowMaxBytes`, whichever comes first, and always admits one
oversized row on an otherwise-empty page. It is sized above the measured
heavy window on purpose: trimming rows trades visible history for bytes,
so the field-level projection is what does the work and this only stops
a pathological window.

`internal/app/app_wire_budget_test.go` is the gate — it serializes the
cold window into a `transport.ServerFrame`, deflates it the way the
socket does, and fails on ceilings recorded with their measurement.
