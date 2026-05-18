# Layout Persistence

> Layout state lives in `localStorage` per-client. Restored on launch.

## Decision

- Single key in `localStorage` stores the entire layout JSON.
- Per-client by virtue of per-webview-profile: the Wails webview has its own `localStorage`; each `--connect` browser instance has its own.
- **No backend storage, no new SQLite table, no transport RPCs** for layout. Layout is pure UI state and never crosses the Go ↔ frontend boundary — preserving the "transport boundary stays clean" invariant from `CLAUDE.md`.

## Storage shape

```json
{
  "version": 1,
  "panes": [
    { "paneId": "uuid", "threadId": "thread-id", "ratio": 1.0 }
  ],
  "focusedPaneId": "uuid"
}
```

`focusedPaneId` may be `null` when the empty state is active.

Suggested localStorage key: `agentOverflowPaneLayout`.

## Save triggers

- **Immediate write** on: add pane, close pane, reorder, swap thread in pane, focus change.
- **Debounced write** (~200ms trailing edge) on: divider drag / pane resize, to avoid hammering localStorage at 60Hz during a drag gesture.

## Restore on launch

1. Read the blob from `localStorage`. If missing, malformed, or `version` doesn't match, treat as empty (skip to step 5).
2. For each saved pane, validate the thread still exists via a backend call (`ListThreads` or a minimal `ThreadExists(id)` binding). Batch this so it's a single round-trip, not N calls.
3. Drop panes whose threads are gone (deleted, archived, or otherwise unavailable). Adjust ratios to stay consistent — the remaining ratios are kept as-is; no renormalization needed since ratios are relative.
4. If `focusedPaneId` still points to a surviving pane → focus it. If the focused pane's thread is gone → focus the leftmost surviving pane.
5. If zero panes survive (or no saved layout exists), mount the empty-state surface (0 panes is a valid state — see [pane-lifecycle.md](./pane-lifecycle.md)).

## Default state when no layout is saved

When no `agentOverflowPaneLayout` exists in `localStorage`, the default is the **empty state** (0 panes). This applies to:

- First launch of the multi-pane build for an existing user.
- Fresh install.
- Any session where the user has closed all panes and reopens the app.

One consistent rule: no saved layout → empty state. The user opens a thread from the sidebar (or creates a new one) to populate the first pane.

## Accepted limitation

`localStorage` is wiped if the user clears browser data (or wipes the Wails webview profile via OS-level "delete app data"). The layout is lost in that case. Acceptable v1 trade-off: layout is preference, not data. See [out-of-scope.md](./out-of-scope.md) "Forfeited durability".

## Implementation notes

- New module `frontend/src/lib/stores/paneLayoutPersistence.ts` (or inlined in `paneLayout.svelte.ts`) with `loadFromStorage()` and `persistToStorage()`.
- Debounce: reuse any existing trailing-edge debounce in `frontend/src/lib/utils/` or write a 10-line helper.
- Validation step in restore: a minimal `ThreadsExist(ids: string[]) -> Set<string>` Wails binding or filter inside `ListThreads` to avoid N round-trips. If a binding already exists for similar purposes, reuse it.
- Restore must run before `PaneHost` mounts, so the layout is populated by the time `getPaneLayoutItems()` is called for the first render.
