# Timeline window pages: run stubs and the loaded member span

A history page ships every prose row in its range, and for every activity
run in its range a stub (counts, digest, edges) plus only the members that
would mount. The rest of a run's members load on demand. A window never
splits a run: the page edges land on run boundaries or prose rows, so a
held window is describable as "these edges, these shipped rows, these
stubs" and the server can verify it from one range read.

Wire cost of any page is bounded by what a screen can show: prose rows plus
`runWindowRows` members per run, under a byte ceiling the caller sets. A run
with 5,000 members costs one stub and `runWindowRows` rows.

Owners: `internal/store/activity_runs.go` (classification, scan, stubs),
`internal/store/paging.go` (page composition), `internal/app/app_paging.go`,
`app_item_projection.go` (request shape, byte trim) and
`app_activity_runs.go` (the members RPC),
`frontend/src/lib/stores/threadActivityRuns.svelte.ts` (the client run
record), `frontend/src/lib/stores/threadWindowDigest.ts` (held window).
The run's rendering contract is [activity-runs.md](activity-runs.md); the
pane's window bounds are in [frontend-scroll.md](frontend-scroll.md); the
held-window sync is [thread-replica-sync.md](thread-replica-sync.md) §5.

## 1. Run membership, on the server

The server groups visible rows selected by `TimelineSelection` into
runs with the rule `frontend/src/lib/utils/timelineRail.ts` and
`activityRunGrouping.ts` apply on the client. Both sides run the shared
fixture `internal/store/testdata/activity_run_vectors.json` (byte-identical
copy at `frontend/src/test/fixtures/activityRunVectors.json`), so the rule
cannot drift.

A row is a **rail row** when `kind` is `tool_call`, `tool_completion`,
`terminal_interaction` or `thinking` and its payload kind is not
`proposed_plan`. A run is a maximal consecutive stretch of rows that are
rail rows, or `notification` rows with a run member immediately before
them (absorbed bells). Any other row ends the run. Payload kind is only
known after the payload join, so classification happens in Go over a
narrow scan, never in SQL alone.

Runs are identified by `firstItemId`. Runs grow only at their newer end
(new rows land at the write head; head-healed prompts are prose), so the
first member is stable for the life of the window.

### Transcript selection

Scoped reads require the owning backend's `timeline.scopes.v1` capability.
Paging keeps its four positional arguments: `TimelinePageOptions` carries the
existing flat shape fields plus `selection`. Main-thread navigation keeps
`GetThreadUserMessageTicks`; scoped navigation uses `GetTimelineUserMessageTicks`.
Older clients can read main history on newer hosts. New clients surface an
update requirement before issuing scoped reads to an older host.

Selection is independent of `PageShape`. An empty selection reads the main
thread's top-level rows. A scoped selection reads direct children of its
canonical transcript root; `Tools` restricts that scope to tool activity for
an expanded background tray. Page composition, has-more probes, run-member
reads and held-window verification all apply the same selection. Local and
imported history use indexed physical arms.

Scoped pages also carry root identity and the latest execution's lifecycle
context, even when no transcript rows exist or a held window verifies fresh.
The pane and each tray digest own separate windows, run state and row leases.
Their history resources share ordered item events and backend recovery with
the main thread. Main-thread pruning therefore cannot evict scoped content.

## 2. The page

```go
type PagedItems struct {
    Items        []Item             // shipped rows, (turn_index, item_index) order
    Runs         []ActivityRunStub  // one per run in [OldestCursor, NewestCursor]
    OldestCursor, NewestCursor TimelineCursor
    HasMoreOlder, HasMoreNewer bool
    // OldestTurnIndex, NewestTurnIndex, HasMore unchanged
}

type ActivityRunStub struct {
    FirstItemID, LastItemID string   // run edges, both physical rows
    FirstTurnIndex, FirstItemIndex int // coordinates of those edges: a run
    LastTurnIndex, LastItemIndex   int // is contiguous, so a top-level row
                                       // is a member exactly when it lies
                                       // between them (§6 jumps)
    MemberCount             int      // every physical member
    LoadedFirstItemID, LoadedLastItemID string // shipped span; "" when none
    UnshippedBefore, UnshippedAfter int // members outside the span, per side
    UnshippedDigest string           // §5, over every unshipped member
    UnshippedGroups []ActivityRunGroup
    UnshippedPairedLaunchIDs []string // unshipped tool_call rows whose
                                      // completion is shipped
    ShippedSupersededLaunchIDs []string // shipped tool_call rows whose
                                      // completion is unshipped
    UnshippedFailed bool             // an unshipped member the header must
                                      // report as failed (§4 pairing rule)
    RunningBefore, RunningAfter *ActivityRunGroupKey // newest running
                                      // member on each side, or nil
}

type ActivityRunGroupKey struct {
    Kind, ToolName string
    MCP string   // json_extract(items.meta, '$.mcp'), "" for native tools
}
type ActivityRunGroup struct {
    ActivityRunGroupKey
    Rows int   // sum of display rows (§4), not member count
}
```

The page range `[OldestCursor, NewestCursor]` contains only whole runs and
prose rows. Every physical row in the range is either in `Items` or counted
by exactly one stub. That invariant is what lets a client fold stubs into a
held-window description (§5) and what lets retention drop rows without
asking the server (§6).

Cursors may name unshipped rows: a page whose newest unit is a run ends at
that run's `LastItemID` even when the shipped span stops earlier. The
cursor pagers accept such a cursor because they resolve it by id. A stub
with an empty shipped span counts every member as `UnshippedBefore` and
resolves both running edges on that side.

### 2.1 Composition

Every pager (`ListThreadSliceAround`, `listTailSlice`,
`ListItemsBeforeCursor`, `ListItemsAfterCursor`, and `SyncThreadWindow`'s
page) composes the same way:

1. Resolve the anchor row (or the tail). If it is a run member, find the
   run's edges by scanning to the nearest non-member on each side.
2. Walk outward in **units**, a prose row or a whole run, alternating
   sides for the around-slice exactly as the half budgets do today, or one
   side for the cursor pagers. A run unit ships its `runWindowRows`
   newest members, except the anchor's run, which ships `runWindowRows`
   members centered on the newest member at or before the anchor's
   coordinate (the anchor itself, or the row a child anchor renders under).
   A side stops when its shipped-row count reaches its budget; the unit
   that crosses the budget is still admitted whole (bounded by
   `MaxActivityRunWindowRows`, 200).
3. The scan reads a narrow projection through `timelineArms` in chunks:
   `id, turn_index, item_index, kind, tool_name, status, completion_of,
   rev, payload kind, json_extract(meta,'$.mcp')`, and the file-row
   expressions of §4. No `summary`, no full `meta`, no payload body. The
   chunks stream, but a run's scan rows stay in memory for the length of
   one composition (the pairing rule needs the whole membership), about
   100 bytes per member. A run longer than `maxActivityRunScanRows`
   (100,000) is an error, not a truncated stub.
   A cursor pager whose first row continues a run that started at or
   before the cursor expands that run whole: the page range crosses the
   cursor, the stub counts every member, and only rows strictly past the
   cursor ship. The client reconciles such a stub per §6.
4. Hydrate the shipped ids with `queryHydratedTimelineItems`, decorate
   (plans, subagent anchors) as today.
5. Build stubs from the scan: per run, aggregate every member outside the
   shipped span.

`finalizePagedItems` derives `HasMoreOlder`/`HasMoreNewer` from the range
edges as today.

### 2.2 The byte trim

`projectPage` projects the shipped rows, then admits a contiguous range of
them around the anchor under the caller's byte ceiling (`admittedRange`).
A shipped row that falls outside the admitted range does not leave the
page's range; it becomes unshipped: its `(id, rev)` folds into its run's
`UnshippedDigest`, its group into `UnshippedGroups`, and the span and side
counts move. The admitted range is contiguous, so a run can lose its whole
shipped span only at a far end; it then leaves the range with its unit, the
cursor moves to the last surviving unit, and that side's has-more flag
gains what was dropped. The anchor row is always admitted.

The anchor is resolved among the SHIPPED rows. An anchor the page did not
ship — a subagent child, a run member outside the window — resolves to the
newest shipped row at or before its coordinate, which `internal/app` reads
with one point read on that path only.

### 2.3 Request shape

```go
type PageShape struct {
    InlinePreviews bool // per-client setting, rides the request
    RunWindowRows  int  // per-client `activityRunWindowRows`; clamped to
                        // [MinActivityRunWindowRows, MaxActivityRunWindowRows],
                        // so 0 takes the setting default (30)
    MaxBytes       int  // projected bytes the caller wants; clamped to
                        // itemWindowMaxBytes (512 KiB); 0 = the ceiling
}
```

`PageShape` replaces the bare `inlinePreviews bool` on
`ListThreadSliceAround`, `ListItemsBeforeCursor`, `ListItemsAfterCursor`
and `ListActivityRunMembers`; `SyncThreadWindowRequest` carries the three
fields flat, because it is a JSON request body rather than an argument
list. `internal/app` clamps all three in one helper before the read, and
the store clamps `RunWindowRows` again. The pane sends
`TIMELINE_PAGE_MAX_BYTES` (`frontend/src/lib/stores/threadPaneShared.ts`)
on every page; the row budget stays the SQL ceiling.

## 3. On-demand members

```go
// ListActivityRunMembers returns up to `limit` members of one run adjacent
// to the caller's loaded span, plus the stub for the span the caller holds
// after this call. limit 0 refreshes the stub only.
func (a *App) ListActivityRunMembers(threadID string, req ActivityRunMembersRequest) (ActivityRunMembers, error)

type ActivityRunMembersRequest struct {
    RunFirstItemID    string
    LoadedFirstItemID string // "" when the caller holds no members
    LoadedLastItemID  string
    Direction         string // "before" | "after" | "around"
    AroundItemID      string // Direction "around": center the span here
    Limit             int
    Shape             PageShape // InlinePreviews and MaxBytes apply
}
type ActivityRunMembers struct {
    Items []Item
    Stub  ActivityRunStub // describes the run for the caller's NEW span
}
```

`Items` holds only rows the caller does not hold: `before`/`after` return
the members past the span's edge, `around` returns the whole new span. With
no loaded span, `before` returns the run's newest `Limit` members and
`after` its oldest. `around` is the jump path when the target is an
unshipped member of a run the pane already holds: the response replaces the
loaded span rather than extending it (the previous span's rows become
unshipped and are described by the returned stub). `Shape` lives on the
app-level request only; the store ships full rows and the app projects and
trims them. A response may therefore carry fewer than `Limit` members: the
app trims by `Shape.MaxBytes` from the end adjacent to the caller's span
and re-asks the store at the limit that fits, rather than dropping rows
from an answer whose stub would then describe a span the caller does not
hold. One member always ships, so a boundary can always advance. The server validates that the run still starts at
`RunFirstItemID` and that the loaded span ids are members; otherwise it
returns an error the pane reports and follows with a window reload.

## 4. Header counts

The header (`activityRunSummary.ts`) sums three sources: loaded members
(classified live, as today), shed members (§6), and `UnshippedGroups`.

Group facts the server must produce identically to the client:

- **Display rows.** A `tool_call` whose tool is a file-change tool counts
  a positive numeric `inlineDiff.totalFiles`, else a non-empty
  `inlineDiff.files`, else the non-empty string paths of
  `meta.input.files`, else 1 (`utils/fileChangeRows.ts`). The three
  sources are read as separate SQL expressions and the precedence is
  applied in Go, because the client falls THROUGH a present-but-empty
  source and a single `COALESCE` would not.
- **Pairing.** A `tool_completion` whose `completion_of` is a member of the
  same run counts zero rows; the launch counts. A launch with a completion
  in the run contributes nothing to `UnshippedFailed`/running (the
  completion supersedes it). The two launch lists cover every pair the
  shipped span splits: `UnshippedPairedLaunchIDs` names unshipped
  launches whose completion is shipped, so a loaded completion pairs and
  counts zero; `ShippedSupersededLaunchIDs` names shipped launches whose
  completion is unshipped, so a loaded launch reads as superseded instead
  of live. The byte trim (§2.2) moves a launch between the lists as it
  folds either half, in whichever order the trim reaches them.
- **Failed** is `status IN ('errored','killed')`; **running** is
  `status IN ('running','streaming')`, newest wins per side.

Presentation (label, icon, provider aliasing, MCP family) stays in
TypeScript: the client builds the presentation from
`ActivityRunGroupKey` exactly as it does from a loaded `Item`.

The fixture of §1 also carries expected group output per case.

## 5. Held window digest

The digest is the XOR of per-row FNV-1a 64 hashes of
`id + 0x1f + decimal rev`, rendered as 16 lowercase hex characters
(`WindowDigest`, `internal/store/window_digest.go`; client
`threadWindowDigest.ts`; vectors `window_digest_vectors.json`). XOR is
order-free and composable, so a client folds what it holds:

```
digest = XOR(loaded rows) ^ XOR(shed rows) ^ XOR(stub.UnshippedDigest for every held run)
count  = loaded + shed + Σ (UnshippedBefore + UnshippedAfter)
```

The server still re-derives the range from the database and compares
`(count, digest, edges, has-more)`; nothing the client sends is trusted.
`MaxHeldWindowItems` counts physical rows and is 8,000: the verification
read is `(id, rev)` only, so a large range costs less than one page. A
client holding a row with `rev < 0` sends no held window (an unstamped row
cannot verify; sending one would only cost the same page).

## 6. The client run record

`ThreadActivityRuns` holds, per run in the window:

- `stub`: the newest server stub (from a page or a members response).
- loaded members: one contiguous span of `Item`s in the pane's items list.
- `shed`: narrow copies (`id, rev, kind, toolName, status, completionOf,
  mcp, fileRows`) of members the pane dropped from memory since the last
  server stub. Shed rows are always contiguous with the loaded span on its
  older side. A members response or stub refresh describes them again, so
  it clears `shed`.

Rules:

- **Window edges move only across rows the client holds.** Retention
  (`keepWindowNearReader`, the hard ceiling, `cutWindowByRootCursor`)
  drops prose rows and whole runs outside its cut, and sheds the loaded
  members of a run that straddles it; it never drops a run whose stub it
  would still need. Subagent panes and expanded tray digests own separate
  scoped windows; pruning the main timeline does not affect their rows.
  Each timeline renders only its own loaded window (`itemsWithinLoadedWindow`).
- **Upserts.** A pushed row whose coordinates fall inside a held run but
  outside its loaded span is not inserted; it marks the stub dirty, and
  the pane refreshes it (`ListActivityRunMembers` with `Limit` 0,
  debounced per run). A page stub for a run the pane already holds whose
  loaded span differs from the pane's (a cursor page that crossed into a
  held run) merges the shipped rows and marks the stub dirty the same way. Rows at or past the newest edge append as today and
  extend the live run's span. `threadItemUpserts.ts` owns this routing.
- **Boundaries.** "N earlier" is `UnshippedBefore + len(shed) +` unmounted
  loaded rows; "N later" is unmounted loaded rows `+ UnshippedAfter`.
  Mounting past the loaded span fetches `ACTIVITY_RUN_CHUNK_ROWS` members
  in that direction and mounts them when they land; the boundary shows a
  pending state meanwhile and reports a failed fetch.
- **Jumps.** `loadUntilItem` resolves the target's coordinates; when they
  fall between a held run's stub edges but outside its loaded span it
  fetches `around` the target and then reveals it, instead of reloading
  the window. A target outside the edges is not the run's, whatever the
  window holds beside the run: it takes the whole-window slice. The
  server refuses a members call whose run or span no longer matches the
  store with the public code `activity_run_stale`
  (`app.ActivityRunStaleCode`); the pane branches on the code, never on
  the message, and answers it with a window reload.
- **Signatures and priors.** The run row signature and size prior include
  `MemberCount`, the loaded span edges, and the mount window; the trace
  schema records stub counts beside loaded counts.

## 7. Filling the screen

A page is sized in bytes, so it may not fill the viewport when its rows are
short (collapsed runs, chips). After a window change settles, the pane
loads the side that has more history whenever the loaded content is
shorter than the viewport plus both auto-load zones
(`timelinePaging.ts`), independent of scroll position, until it fills or
the retention target is reached. This replaces the row-count asks:
`ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS` is a retention target only and is no
longer a fetch size.

## 8. Tests

- Go: classification and stub fixture; page composition (whole runs, anchor
  run centered, byte trim folding into stubs, cursors on unshipped rows);
  held-window verification with stubs; members RPC in each direction;
  the wire-budget test restated for the new shape.
- Vitest: shared fixtures; registry stub/shed accounting; upsert routing;
  digest composition; header sums; boundary fetch states.
- Playwright (`e2e/tests/message-nav-rail.spec.ts`,
  `activity-run*.spec.ts`, scroll windowing specs): jump to first/last
  through heavy runs; scrolling past a run larger than the page; reopen
  answered `fresh` with stubs held. Rebuild the harness before running.
