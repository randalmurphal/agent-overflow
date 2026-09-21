# Subagent scope windows plan

Every subagent transcript surface (agent pane, inline card digest, tray
digest) reads through the same windowed page, run stub, on-demand member
and sync infrastructure as the main timeline
([timeline-window-pages.md](timeline-window-pages.md),
[activity-runs.md](activity-runs.md),
[thread-replica-sync.md](thread-replica-sync.md)). This document is the
design and build order for that change. It also closes the other places
where the wire or a renderer drops rows or text without a recovery route.

Status: planned, not started.

## 1. Requirement

Two rules bind every timeline surface, main thread and subagent alike:

1. The visible viewport is never truncated. A row that renders shows its
   whole content, or a control that loads the rest.
2. History is never unrecoverable. Any row the store holds can be reached
   from the surface by scrolling or a boundary control. A read that cannot
   ship everything reports an edge (cursor, has-more flag or stub count)
   that a later read advances across.

Memory is the accepted cost. Retained rows are bounded by the surfaces that
are open, never by a fixed row or byte cap without an edge.

## 2. Where the current code breaks the rules

The main timeline complies. Pages are units (prose rows and whole runs),
the byte trim folds dropped members into stubs and moves cursors
(`PagedItems.TrimShipped`), `ListActivityRunMembers` always advances,
window cuts keep held rows, and every wire elision carries a marker with
`GetThreadItemProjectionSource` as its recovery route.

The subagent path is a separate mechanism and breaks both rules:

| Site | Defect |
|---|---|
| `internal/app/app_item_projection.go` `projectItemSlice` | Keeps the newest rows under `itemWindowMaxBytes` (512 KiB) and drops the rest with no cursor, count or flag. Serves `ListSubagentDescendants`, `ListLiveBackgroundTasks`, `ListThreadProposedPlans`, `ListItems`. A transcript over the ceiling opens without its prompt and cannot page back. |
| `internal/store/subagent_items.go` `maxSubagentDescendants` | 2000 newest rows win; the collapsed count still reports the total, so the surface shows a count it can never load. `subagent_items_test.go` asserts the loss. |
| `frontend/src/lib/stores/agentScopeView.svelte.ts` | `loadOlder`/`loadNewer` return `NO_PAGE`, `hasMoreHistory` is `false`, cursors are `null`: the view has no way to say "there is more". |
| `agentScopeNeedsHydration` + `hydrateChildren` | Refetches once when `loaded < expected`, then marks the root exhausted after a fetch that returned nothing new, so a trimmed answer is accepted as complete. |
| `threadSwitchLoad.svelte.ts` sync and refresh reconcile | `reconcileSnapshotPage` keeps only page rows plus live-touched rows, so every sync or reconnect drops hydrated children and reruns the truncating fetch. |
| `AgentPane.svelte` "No output yet.", `BackgroundTaskTrayDigest.svelte` one-shot `loadAttempted` | Report a refused or trimmed load as an empty agent, or never retry. |
| `ToolResultCard.svelte` detail text | `LazyContentBlock` with `payloadId={undefined}` truncates at `MAX_INLINE_BYTES` with `…` and no expander. |
| `threadItemUpserts.ts` older-edge refusal | Refuses a pushed row below the floor without arming `hasMoreHistory`, so a repositioned row can sit in unreachable history. |
| `internal/itemmeta/trim.go` `TrimToolResultEcho` | Bounds completion-echo fields at persist time without a marker. The payload row still holds the text, but nothing on the item says the echo was cut. |

The mechanism behind the first six rows is the same: subagent children are
hydrated as one whole slice and ride inside the host pane's `items`, so
every window rule (filters, folds, held rows, reconcile) has a special
case for them and none of the paging rules apply.

## 3. Target model: a scope is a window

A **scope** is the set of visible rows with one `parent_id`. The main
timeline is scope `""`. A subagent transcript is scope `<launch id>`
(resolved to the transcript root for a resume carrier, as
`ListSubagentDescendants` does today through `transcriptRootFromMeta`).
Nested launches are rows of their parent's scope and open their own
scope; a scope never contains grandchildren.

Every window mechanism takes the scope root as a parameter and behaves
identically at every root:

- page composition in units (prose rows, whole runs) with run stubs;
- the byte trim that folds dropped members into stubs and moves cursors;
- `ListActivityRunMembers` for members outside the shipped span;
- `SyncThreadWindow` with a held-window digest;
- the client window record (`ThreadTimelineWindow`, `ThreadActivityRuns`),
  its retention cuts, its boundaries ("Load older", "N earlier") and its
  fill-the-screen loop.

The host pane's `items` holds scope `""` only. Child rows never enter it.
Each open subagent surface reads a **scope window** owned by a registry
keyed by `(threadId, scopeRoot)`; several surfaces on the same root share
one window through reference-counted holds. A window with no holders is
released with its rows. That is the memory bound: open surfaces, nothing
else.

### 3.1 The scope header

A subagent surface must open showing what the agent was asked to do
(agent-visibility.md success criterion). A tail-anchored first page of a
long transcript does not reach the prompt row, so the page alone cannot
satisfy that. Every scoped page and scoped sync response therefore carries
a header beside the window:

```go
type ScopeHeader struct {
    Launch     Item  // the scope root row, projected
    Prompt     *Item // first visible user_text of the scope (wire_only prompt), nil when none
    Completion *Item // the launch's `complete:<id>` sibling in the parent scope, nil when none
    RowCount   int   // visible rows in the scope, every round included
}
```

The header is not part of the window's range. The renderer paints the
prompt above the window; when the window's oldest cursor is past the
prompt row, an ordinary older boundary sits between them, and loading
older eventually reaches the prompt row itself, at which point the
in-window copy replaces the header copy (same id, deduped by the row
projection). `RowCount` is what the collapsed card, the tray row and the
"N earlier" boundary show; it replaces the read-time
`subagentDescendantCount` and `subagentTranscriptDescendantCount`
decoration for surfaces that hold a window. `decorateSubagentAnchors`
stays for collapsed cards inside a page (count and latest preview) because
those cards hold no window.

`Launch` and `Completion` are in the header because a scope opened from
the tray or restored across a restart may have its root outside the host
window; today that state renders "This agent's launch row isn't in the
loaded timeline window." and depends on `loadAgentScope` pulling rows into
a held island. With the header, the surface never needs the host window to
hold its root.

## 4. Server changes

### 4.1 Store: scoped filters and pagers

- Replace `topLevelItemsFilterFor(alias)` with
  `scopeFilterFor(alias, root string)` producing `parent_id = ?` with the
  root as an argument. `windowedTimelineFilter` becomes a function of the
  root. Sites: `paging.go` (pagers, `decoratePagedItems`, the has-more
  probes at 360 and 376), `activity_run_scan.go` `fill`, `window_digest.go`
  (261, 304), `history_sync.go` `SyncThreadWindow` and
  `verifyHeldWindowTx`, `items_range.go` (`includeChildren` becomes a
  root), `timeline_units.go` callers. Reads that genuinely mean "the main
  timeline only" (`items_read.go` user ticks, title regeneration,
  `thread_aggregates.go`) keep the root `""` explicitly.
- Add `ScopeRootID string` to the store pager signatures
  (`listThreadSliceAround`, `ListItemsBeforeCursor`,
  `ListItemsAfterCursor`, `ListActivityRunMembers`, `SyncThreadWindow`,
  `ListItemsInRange`). Resolve a resume carrier to its transcript root once
  at the top of each call, as `ListSubagentDescendants` does now.
- `ScopeHeader` read: one point read for the launch, one for the
  completion sibling (`complete:<id>` under the parent scope), one ordered
  read for the first visible `user_text` of the scope, one `COUNT`. Built
  by the pagers when `ScopeRootID != ""` and attached to `PagedItems` as
  `Header *ScopeHeader`.
- Index: `items(thread_id, parent_id) WHERE parent_id <> ''` exists
  (`migrate_sql_items.go:96`); the scoped pagers order by
  `(turn_index, item_index)` within a parent, so add
  `(thread_id, parent_id, turn_index, item_index)` for non-empty
  `parent_id`. Verify with `EXPLAIN QUERY PLAN` in `paging_test.go`, the
  same way the top-level index is checked.
- Run classification inside a scope: `activityScanRow` membership rules
  are unchanged; the walker only sees rows of the scope. A run's members
  therefore share a parent, and `ListActivityRunMembers` validates that
  `RunFirstItemID` is a row of the requested scope, refusing with
  `activity_run_stale` otherwise.
- Delete `ListSubagentDescendants` and `maxSubagentDescendants`.
  `subagent_items_test.go` cap test and the parity fixture in
  `timeline_arms_test.go` move to the scoped pager tests.
- `HeldWindow` gains `ScopeRootID`; edges must be visible rows of that
  scope. `MaxHeldWindowItems` applies per window.

### 4.2 App: one page path, no bare slices

- `PageShape` gains `ScopeRootID string`. Every history binding
  (`ListThreadSliceAround`, `ListItemsBeforeCursor`,
  `ListItemsAfterCursor`, `ListActivityRunMembers`,
  `SyncThreadWindowRequest`) carries it. `projectPage` projects the header
  rows with the same shape and never trims them: they are three rows.
- Delete `projectItemSlice`. Its four callers:
  - `ListSubagentDescendants`: deleted (4.1).
  - `ListThreadProposedPlans`: 0 or 1 rows, project only.
  - `ListLiveBackgroundTasks`: launch rows only (no payload bodies), so
    the per-row wire elision bounds it; project only, and add a Go test
    that a tray row's projection has no field over the elision threshold.
  - `ListItems` (app.go:714): no frontend caller outside test mocks;
    delete the binding and the test mocks. Internal Go callers use
    `a.store.ListItems` directly and are unaffected.
- `itemWindowMaxBytes` stays as the clamp on `PageShape.MaxBytes` only.
  No code path drops a row against it without `TrimShipped`.
- Enforcement: a Go test in `internal/app` walks every `//ao:scope
  threads:read` method with reflection and fails when its result type is
  `[]store.Item` unless the method is listed with its reason (proposed
  plans: 0 or 1; tray feed: launch rows). Adding a slice-returning item
  RPC becomes a deliberate act.
- `TrimToolResultEcho`: when it bounds a field, set
  `meta.echoTrimmed = true` beside it so the renderer can offer the
  payload route. The payload row is the source; nothing else changes.

### 4.3 Wire and bindings

Regenerate bindings (`internal/app/AGENTS.md`). `SyncThreadWindowRequest`
and `PageShape` are additive; the deleted RPCs (`ListSubagentDescendants`,
`ListItems`) are removed from `methods_gen.go`, `methodRoutes.ts`,
`bindings.ts` and the test mocks in one change.

## 5. Client changes

### 5.1 Scope window registry

New `stores/scopeWindows.svelte.ts`:

```ts
interface ScopeWindow {
  readonly threadId: string;
  readonly scopeRoot: string;
  readonly header: ScopeHeader | null;
  readonly items: readonly Item[];      // the window's rows, scope-local
  readonly window: ThreadTimelineWindow; // cursors, has-more, loads, cuts
  readonly runs: ThreadActivityRuns;     // stubs, members, boundaries
  readonly loading: boolean;
  readonly verified: boolean;            // SyncThreadWindow answered
}
function holdScopeWindow(threadId, scopeRoot, backend): { window: ScopeWindow; release(): void };
```

- One instance per `(threadId, scopeRoot)`; `holdScopeWindow` increments
  a hold count and creates on first hold. `release` decrements; zero holds
  disposes the window and its rows. `holdAgentScope` in
  `agentPane.svelte.ts` becomes a caller of this.
- The window is built with `createThreadTimelineWindow` and
  `createThreadActivityRuns` exactly as `thread.svelte.ts` builds the main
  window, with `pageShape` carrying `scopeRootId`, `threadId` set (so
  member fetches are live), and `getHeldRowIds` absent (nothing outside the
  scope holds its rows). Retention constants (`TARGET`, `MAX`,
  `HARD_CEILING`) apply per window.
- Cold open: `SyncThreadWindow` with `scopeRootId`, the L1 cache and the
  IndexedDB replica keyed by `(threadId, scopeRoot)`
  (`replica/envelope.ts`), same stamp pair, same `reconcileSnapshotPage`.
  The header rides the replica envelope so a restored companion paints the
  prompt before the sync returns.
- Fill the screen and the auto-load zones run per window through the
  surface's own scroll controller, as the main pane does.

### 5.2 Host pane holds top-level rows only

- `thread.svelte.ts`: delete `subagentMemory`, `ensureSubagentChildren`,
  `loadAgentScope`, `sweepUnheldAgentScopes`, `agentPaneHeldRowIds`,
  `toggleSubagentGroupExpanded`'s eviction, `subagentLiveAggregate`'s row
  sourcing. Delete `threadSubagentMemory.ts`, `utils/subagentFold.ts`, the
  fold slots in the L1 snapshot and replica envelope, `getHeldRowIds` on
  the window, and `itemsWithinLoadedWindow` islands.
- `threadItemUpserts.ts`: a pushed row with a non-empty `parentId` is
  routed by the stream-apply layer before the merge: to the scope window
  holding that parent when one exists (it applies the row through its own
  `upsertItemsBatch`, floor, ceiling and run rules included), otherwise to
  the anchor's **live aggregate** only. Delete `rejectedParentedItems`,
  `recordAdmission`, `swallowedChildIds` and `orphanedLiveChildren`.
- Live aggregate (`subagentLiveAggregate`) becomes a counter and latest
  preview per launch fed by the routed deltas, with no rows behind it. The
  collapsed card reads `max(decorated count, live count)` as it does today.
- `reconcileSnapshotPage` no longer sees children; the special cases go.
- `groupItemsBySubagent`: the host pane's rows have no children, so the
  grouping walk builds a launch node from the launch, its completion
  sibling and the aggregate. The fast path becomes the only path for the
  host; the same function groups a scope window's rows (nested launches
  inside a transcript) when a scope is rendered.

### 5.3 Surfaces

- `agentScopeView.svelte.ts` keeps its role as the `ThreadPane` facade the
  renderer needs (composer shell, turn state, actions) but the overridden
  members (`items`, `loadOlder`, `loadNewer`, `loadUntilItem`,
  `hasMoreHistory`, `hasMoreNewer`, cursors, `activityRuns`, `loading`)
  forward to the scope window. The `NO_PAGE` constant and the null-cursor
  overrides are deleted. `windowVerified` reads the window.
- `AgentPane.svelte`: holds the scope window for its scope; the restore
  effect, `scopeLoadAttempted`, the hydration effect and
  `agentScopeNeedsHydration` are deleted. Body states: header present and
  `RowCount == 0` renders "No output yet."; window loading renders the
  spinner; a failed sync renders the pane error with retry, never an empty
  transcript. The "launch row isn't in the loaded timeline window" state is
  unreachable and removed.
- `SubagentGroup.svelte` (inline card): expanding holds the scope window
  and renders the digest from its rows through the existing allowlist
  filter, tail-anchored, with the header prompt pinned at the top and the
  window's older boundary ("N earlier", `RowCount` minus loaded minus
  unshipped after) between them. Collapsing releases the hold. The
  "capped, virtualized, faded" digest becomes "windowed, virtualized,
  faded": the fade is presentation, the cap is the window.
- `BackgroundTaskTrayDigest.svelte`: same as the card. `loadAttempted`
  and "Agent launch is not in the loaded history." are deleted; the header
  supplies the launch.
- `ToolResultCard.svelte`: detail text over `MAX_INLINE_BYTES` renders the
  preview with a "Show all" control that expands from the meta already
  held (the detail is in `meta`, the truncation is client-side only). When
  `meta.echoTrimmed` is set, the control loads the payload through the
  existing payload route instead. `LazyContentBlock` gains a `fullText`
  source for the no-payload case and refuses (type error) a use that
  supplies neither `payloadId` nor `fullText`, so truncate-only rendering
  is unrepresentable.
- `threadItemUpserts.ts` older-edge refusal: refusing a row below the
  floor sets a `droppedOlderItems` flag the window turns into
  `hasMoreHistory = true`, mirroring `droppedNewerItems`.

## 6. Tests and enforcement

Fixtures at every level exceed the old caps: a transcript of 2500 rows
whose projection is over 1 MiB, with the prompt row first and a nested
launch in the middle.

- Go (`internal/store`, `internal/app`): scoped page composition (units,
  anchor run centered, byte trim folding into stubs, cursors on unshipped
  rows) at a non-empty root; header contents for a launch with and without
  completion, resume rounds under one root; `SyncThreadWindow` fresh and
  stale at a scope; members RPC per direction inside a scope and refused
  across scopes; the reflection test over item-returning RPCs; index plan
  check for the scoped pager; `echoTrimmed` marker.
- Vitest: scope window registry hold, release, re-hold (rows released at
  zero holds, not before); child upsert routing to a held window and to the
  aggregate; older-edge refusal arming has-more; `agentScopeView` forwards
  paging to the window; card, tray and pane render the header prompt above
  a tail page with the boundary count; `ToolResultCard` expander.
- Playwright (`e2e/tests/background-tray-digest.spec.ts`,
  `compact-tray-digest.spec.ts`, a new `agent-pane-windowing.spec.ts`):
  open the pane from the tray while the agent runs past 512 KiB of
  transcript, assert the prompt is visible at the top, scroll up through
  every boundary to the prompt row, reconnect mid-scroll and assert the
  loaded rows survive, restart with the pane restored and assert the prompt
  paints before sync. The harness is the gate for these
  (`bin/ao-harness-e2e`).
- Delete the tests that assert the loss: `subagent_items_test.go` cap test,
  `app_item_projection_test.go` newest-wins slice test,
  `threadSubagentMemory.test.ts`, `threadSubagentFold.test.ts`.

## 7. Documentation updates

- `timeline-window-pages.md`: pages are per scope; add §2.4 scope header;
  §3 members validate the scope; §5 held window carries the root.
- `activity-runs.md` accepted tradeoff 2 ("resolved toward memory"): remove;
  retention is per window and the rows are reloadable through the window.
- `agent-visibility.md`: digest wording (windowed, not capped); pane scope
  description reads the scope window; check the "opens with what the agent
  was asked to do" criterion once the header ships.
- `thread-replica-sync.md`: `MaxHeldWindowItems` is 8000 (lines 592, 893);
  replica keyed by `(threadId, scopeRoot)`.
- `internal/store/AGENTS.md`, `frontend/src/lib/stores/AGENTS.md`: replace
  the subagent hydration route with the scope window registry.
- Delete the `projectItemSlice` comment block and the
  `maxSubagentDescendants` comment with their code.
- `docs/decisions.md`: record the ruling that no item read drops rows
  without an edge, and that retained rows are bounded by open surfaces.

## 8. Build order

Each stage leaves the app working and gated by its own tests.

1. Server scoped pages: store filters, pager signatures, header, index,
   `SyncThreadWindow` and members per scope, Go tests. Existing clients
   pass root `""` and see no change.
2. Bindings and the client scope window registry, with `agentScopeView`
   forwarding to it. The pane is the first surface moved; card and tray
   digests follow in the same stage so no surface is left on the old path.
3. Child upsert routing and the host pane cut-over to top-level rows only;
   delete the hydration, fold, held-row and reconcile special cases.
4. Delete `ListSubagentDescendants`, `projectItemSlice`, `ListItems`, the
   caps and their tests; add the reflection test.
5. The remaining truncation fixes: `ToolResultCard` expander,
   `echoTrimmed`, older-edge refusal.
6. Playwright specs and documentation.

Stages 3 and 4 are one commit series: the old path must not coexist with
the registry for longer than the cut-over takes, or two owners hold child
rows.

## 9. Open decisions

Product-facing choices the plan assumes; each names the default it takes.

1. Header prompt pinned above the window, with the older boundary between
   the prompt and the tail. Alternative: first page always starts at the
   prompt (top-anchored), which loses the live tail on open.
2. Card and tray digests share the pane's scope window (one fetch, one
   set of rows per agent). Alternative: digests keep a separate smaller
   window; rejected because it doubles the rows for the same agent.
3. A scope window with no holders releases immediately. Alternative: keep
   the last N released windows warm. The replica already makes a re-open
   paint before sync, so the default keeps memory bounded by open surfaces.
4. `ListItems` RPC deleted. Alternative: page it. No production caller
   exists, so deletion is the default.
