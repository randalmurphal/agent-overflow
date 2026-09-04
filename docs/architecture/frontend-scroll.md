# Frontend Scroll Architecture

This is the durable contract for chat and discussion scrolling. It keeps
the operational rules out of `AGENTS.md` while preserving the invariants
that matter when changing `ThreadPane`, `MessageTimeline`, `ChannelView`,
the timeline virtualizer, or the scroll controller (`utils/scroll/`).

## Owners

- `MessageTimeline.svelte` owns the outer chat scroll container.
- `components/virtual/TimelineVirtualizer.svelte` + `utils/virtual/` own
  virtual row geometry. The split inside:
  - `utils/virtual/` is the bespoke windowing engine, pure data + math
    with no DOM and no Svelte: `sizes.ts` (the size store: measured px
    or estimate per row, memoized offsets), `window.ts` (visible-range
    math), `engine.ts` (the reducer: scroll/resize/measurement/length
    inputs in → mount window + totalSize + at most one compensation
    observation out), `priors.ts` (per-thread, per-row measured-size
    persistence and the estimate resolver, DOM-free; see its header for
    the per-row signature model and the storage-adapter seam),
    `priorsStorage.ts` (the localStorage-backed adapter that makes
    priors survive an app restart, not just a same-session thread
    switch), `types.ts` (the shared shapes). Design doc:
    [`virtualizer-replacement-plan.md`](virtualizer-replacement-plan.md).
  - `TimelineVirtualizer.svelte` is the adapter binding the engine to the
    DOM: one lazy ResizeObserver for the scroller and every mounted row,
    scroll-event feed, spacer + absolute row positioning (`VirtualRow`),
    scrollend synthesis, and the imperative handle
    (`TimelineVirtualizerHandle` in `utils/virtual/types.ts`). It is also
    the controller's **content-geometry source** in chat: the spacer
    height it writes IS the content height, so it delivers
    `ContentGeometrySample`s (`onContentGeometry` →
    `stick.deliverContentGeometry`) post-flush instead of the controller
    re-observing the same element with a second ResizeObserver. Same
    observation shape, one frame earlier, no duplicate layout read on
    the streaming hot path. Each sample also carries the scroller's
    content-box viewport height. While that viewport is stable, the
    controller computes the bottom target directly as `height -
    viewportHeight` and caches that absolute target alongside the last
    observed `scrollTop`. Content deliveries and ordinary spring frames use
    those two facts without re-reading `scrollHeight` / `clientHeight` at
    display rate. Sentinel clamp detection and write-refusal retries force a
    real target read; viewport/width changes take one real resync. Composer
    clearance is scroller padding, so it cancels from `scrollHeight -
    clientHeight` and cannot become hidden target-offset state.
    Row ResizeObserver deliveries use their supplied `contentRect` directly.
    A 0×0 box is the hidden-`display:none` signal and is ignored. Do not add a
    synchronous visibility query such as `offsetParent`: it forces layout for
    every visible row while providing no information the delivery lacks.
  - **The engine never writes `scrollTop`.** Geometry changes that would
    move content above the viewport surface as `EngineCompensation`
    observations; imperative scrolls (`scrollToIndex`) compute their
    target in the engine and perform the write through the controller
    chokepoint (the `applyScrollTarget` prop). There is no second
    scrollTop writer to guard against by construction.
  - **The engine's offset never trails a controller write.** Every
    chokepoint write is reported back (`onScrollTopWritten` →
    `noteScrollTopWritten`) before the scroll event carrying it fires,
    because a compensation computed between the two would be based a
    frame behind the glide and land the viewport that far back.
- `utils/scroll/` owns user scroll intent and **every** programmatic
  `scrollTop` write. Inside the package:
  - `resolver.ts` is the pure per-delivery reducer. Every contentRO
    delivery and every engine compensation observation becomes an
    observation; **the resolver's decision is the only authority on
    what, if anything, gets written**. Adding a scroll behavior means
    adding a decision branch here, not a write site somewhere else.
  - `index.svelte.ts` is the controller: the reactive flags templates
    subscribe to, geometry reads, wiring for the machines below, and
    the public API. `types.ts` holds the consumer contract; consumers
    import from `utils/scroll/index.svelte`.
  - `chokepoint.ts` is the single `writeScrollTop` chokepoint every
    programmatic write routes through, plus its satellites: the
    provenance ledger, arrival-readback acceptance, and spring-tick
    trace sampling. It records the requested-to-readback quantization
    error but does not render a second position. Controller content must
    not carry authored `will-change`, `translate`, or `rotate` state.
    A permanent content layer caused stale WebView2 pixels while state,
    DOM, and input remained live (bug-report-20260823T224631Z). Earlier
    promote/demote leases also caused three raster-transition flickers.
    The spring instead authors whole grid pixels in its motion model
    (below). Real-Chromium coverage pins CSS-pixel quantization at DPR
    1, 1.25, 1.5, and 2 — the premise of the spring's grid witness. It
    also pins constant hairline raster energy while fractional DPR
    turns equal CSS-space steps into alternating device-pixel displacement.
    The soak rig's `make soak-contract` check verifies quantization and
    compositor ownership in WebView2.
  - `intent.ts` is the event-sourced intent machine: wheel/scroll/pointer/
    key/touch listeners, escape and re-stick, restore-snap consent, and
    programmatic-write tagging. Intent is never geometry-inferred. Its
    target-capture scroll listener reads the event-time `scrollTop` once and
    records that observation on the native event. The virtualizer's ordinary
    listener reuses it instead of taking a second getter. Do not substitute a
    chokepoint write readback for the event-time value. Native find, focus
    scrolling, browser clamps, and authored writes can coalesce into one event,
    so the write readback may no longer describe the surface when the event is
    dispatched.
  - `spring.ts` + `retarget.ts` own chase kinematics. They define HOW a spring
    advances scrollTop frame to frame once the controller decides one runs. Speed each
    step is capped by three ceilings recomputed from live geometry
    (below all three, the spring's own decay governs): the
    **acceleration slew** (a geometric onset ramp, ×1.10 per 60Hz frame
    over max(ramp base, current speed toward the target), the base a
    refresh-independent CSS-space 1.0 px/frame floored by the motion floor: a
    standstill quantum eases in instead of jumping to its peak, and
    glides stretch into the next quantum's arrival instead of
    stop-starting per line), the **deceleration envelope** (0.09 ×
    remaining, the ease-out), and the **hard velocity cap**
    (27 px/frame); the
    accelerate→decelerate crossover is wherever the falling envelope
    undercuts the rising ramp, so a fixed-target glide needs no mode
    state. A target that extends while the viewport is already braking
    or holding a speed uses an acceleration-preserving **retarget
    bridge**. Velocity stays forward, acceleration advances from braking
    through zero into driving under a per-frame jerk bound, and
    large-glide jerk scales from the endpoint accelerations so a
    cap-speed handoff does not nearly stop. Repeated streamed-line tests
    pin the bridge at 60, 120, and 165Hz.
    What the spring WRITES is whole pixels on the engine's grid (owner
    ruling 2026-09-04: no jitter; where constant motion cannot avoid
    it, stop instead). Each tick's displacement snaps to a ladder of
    even cadences — `n` grid pixels a tick, or one every `k` ticks, the
    nearest rung held through a small hysteresis — so a deceleration
    steps 3, 2, 1 once and a slow rate is a steady cadence, never the
    1,2,1,2 or 1,0,1,1,0 mix that rounding a fractional model paints;
    only at cruise (8+ pixels a tick) is the residue carried for an
    exact average rate. The **motion floor** is a rung of that ladder,
    derived per tick from the grid and the measured frame cadence
    (`quantizedFloorRung`: closest in ratio to 60px/s, never under 60
    changes a second — 1 CSS px per 60Hz frame at DPR 1 and 2, one per
    two frames at 120Hz DPR 1, one device pixel per frame on a 2.625×
    120Hz phone), and once a glide has run above it the floor holds
    through to the landing: there is no sub-pixel tail. The grid is
    witnessed from readback, device pixels until a write off the
    CSS-pixel grid reads back rounded onto it (desktop Chromium at every
    DPR), and the witness persists for the page's life. The 120Hz
    result is unit-traced only; the Android emulator runs at 60Hz.
    Carried
    momentum decays by the slew factor per real elapsed frame while
    parked, so a brief inter-quantum catch-up resumes at speed while a
    longer pause re-enters at the base ramp. Also owns the
    resume snap: after an observed rAF discontinuity (tick gap
    ≥1s, or document visibility resumed ≤2s ago) a chase more than one
    viewport behind snaps fully to the target (`spring.catchupSnap`
    write). The backlog accrued while frames weren't painting was
    never going to be watched, and the residual one-viewport glide the
    old clamp left is exactly the workspace-return animation the user
    ruled out (2026-08-22). Distance alone never snaps; growth arriving
    after the snap ramps up as a cold onset.
    Every pane spring schedules through `utils/animationFrameBatcher.ts`, the
    app-wide native-rAF coordinator also used by streaming reveal and nav-rail
    sync. Spring callbacks use `before-dom-update`; reveal and rail callbacks
    use `dom-update`. This order lets the spring write against the
    virtualizer's previous clean geometry sample, then lands DOM work before
    paint. A callback requested while a phase is dispatching belongs to the
    next frame, and cancellation remains per callback. Do not replace this
    with one native rAF loop per pane or reorder the phases without a traced
    active multi-pane A/B and the Chromium phase-order test.
  - `observers.ts` is the content-geometry delivery pipeline, the warm-up
    (quiescence) gate, and resize classification. Two sources feed the
    one pipeline: engine-sourced samples in chat
    (`externalContentGeometry`), a contentEl ResizeObserver everywhere
    else (ChannelView). Each delivery is gathered here, decided by the
    resolver, and applied through the controller's chokepoint, so "a
    content delivery" reads in one place.
- The MessageTimeline scroll-session modules (extracted siblings in
  `components/chat/`; MessageTimeline keeps the thin `$effect` bodies
  that call into them):
  - `timelineRestore.svelte.ts` owns thread-switch restore sessions:
    switch-edge bookkeeping, scroll-snapshot save/restore, and the
    scroll-to-item flow.
  - `timelineSizePriors.svelte.ts` owns per-thread, per-row size priors,
    including the `ROW_KIND_ESTIMATE_PX` floor-biased kind estimates,
    the priors capture/persist cadence, and the lazy-once
    width/expansion validity check. Installs the localStorage
    persistence adapter (`utils/virtual/priorsStorage.ts`) at module
    scope.
  - `timelinePaging.ts` owns load-older/load-newer gates and handlers.
  - `timelineWindowAnchor.svelte.ts` owns prune-shift anchoring when the
    live window drops rows off the top.
  - `timelineRowProjection.svelte.ts` owns the node-derivation pipeline:
    structural grouping (subagent/wait/read groups), the reveal gate
    (`revealedNodes`), rail classification, and response-pill duration.
  - `timelineDiagnostics.ts` owns render/state tracing and the dev-only
    memory-stats, pane-geometry, row-resize, margin-divergence, and
    reasoning-tail-jump checks.
  - `timelineQuietWork.ts` owns the quiet scheduler: one cadence
    (structural changes + scroll end, debounced, with a recheck timer
    bridging the sentinel outliving the last scrollend) for the
    timeline's deferred structural work. Its passes: the recent-window
    prune retry, the activity-run auto-collapse releases, and the
    row-UI prune. Geometry-mutating passes run only while no glide is
    running or armed, at most one per callback. Design rationale:
    [`scroll-arbitration-plan.md`](scroll-arbitration-plan.md).
    Its activity-run pass additionally uses
    `timelineVisibilityGeometry.ts`: a hidden document makes cached
    virtualizer geometry ineligible to prove an off-screen fold until the
    existing content-geometry subscription produces a new visible,
    post-flush sample. The barrier is pass-local so background row-UI and
    recent-window memory bounds keep their established cadence.
  - `timelineRowUiPrune.ts` bounds per-row expansion-handle retention
    to a buffer around the visible range plus the tail (a quiet-work
    pass on the 'always' rung: it mutates no visible geometry and must
    keep bounding memory mid-stream).
- `components/chat/ActivityRun.svelte` owns the one nested scroller that runs
  the same physics as the pane: a height-capped clip over a stretch of
  activity rows, with a second controller instance on the tail run only
  (the newest REVEALED run, `node.atTail`, and not `live`, which ends
  mid-stream when closing prose arrives behind the reveal gate).
  Geometry and window math live in `utils/activityRun{Clip,Window}.ts`,
  per-run state in `stores/threadActivityRuns.svelte.ts`. Full architecture:
  [`activity-runs.md`](activity-runs.md).
- `ThreadPane` owns the scroll-controller registration slot so shared
  surfaces can pause or notify scrolling without reaching into component
  internals. It is **single-occupancy and belongs to the surface that owns
  the pane's scroll container**: `MessageTimeline`, or `ChannelView` on a
  discussion pane, whichever is mounted. A controller *nested inside* one
  of those (the activity run's) never registers.

  The slot is `$state.raw`, and that is load-bearing rather than a
  micro-optimization: `detachScrollController` decides whether a teardown
  is stale by comparing the incoming controller against the registered one,
  and a plain `$state` proxies the object on assignment, so the comparison
  is false even for the same controller. It never cleared. A torn-down
  controller, and through it the detached scroll subtree, stayed reachable
  from the pane for the pane's whole life. A controller is a handle: no
  consumer reads data through it, they all re-read the slot.
- `threadScrollSnapshots.ts` owns semantic per-thread scroll snapshots:
  `{ kind: 'bottom' }` or `{ kind: 'anchor', itemId, offsetTop }`.

Do not add another owner for any of those responsibilities. In
particular, a new programmatic write path either goes through a resolver
decision and the `writeScrollTop` chokepoint, or it doesn't go in.

## Thread Switch

`pane.switchThread` is the entry point. It snapshots the outgoing pane,
restores a bounded cache snapshot when present, and otherwise fetches a
viewport-sized slice with `App.ListThreadSliceAround(threadID,
anchorItemID, SLICE_AROUND_ITEM_BUDGET)`.

The pipeline itself (`snapshotOutgoingPane`,
`installCacheOrFreshState`, `paintReplicaWindow`, `runItemWindowSync`,
`applySyncResponse`, `runParallelLoad` and `refreshFromBackend`) lives
in `frontend/src/lib/stores/threadSwitchLoad.svelte.ts`;
`thread.svelte.ts` keeps the pane state it writes through and exposes
the two methods as one-line delegations.

The switch edge is not the only cache writer: pane close is the other
(`pane.snapshotForClose` → `snapshotPaneForClose`, called by
`destroyPane` before `clear()` empties the items, and by
`startDraftPlaceholder` when "+ New" replaces a mounted thread). Both
edges share `cacheOutgoingWindow` (size priors, write-back timer
retirement, optimistic-row stripping, the L1 snapshot + durable
replica), so a reopened thread restores warm instead of cold-fetching
and paying the estimate→measure cascade as a visible spring
(bug-report-20260822T020840Z: every reopen in the trace was
`source: "fetch"`). The close path skips a thread the store no longer
lists, because deletion flows evict every cache tier via `removeThread`
before closing panes and the snapshot must not resurrect the window.

The switch runs `SwitchThread`, live-state hydration, recent-turn fetch,
and the initial slice under one `Promise.allSettled`.
There is no second wider-window load on switch. Older history pages in
lazily through `pane.loadOlder()` when the user scrolls near the top, with
the manual "Load older messages" button as the explicit fallback. The
bottom edge mirrors this: when the window has been pruned away from the
tail (`hasMoreNewer`), scrolling near the bottom pages forward through
`pane.loadNewer()`, with the "Load newer / Jump to latest" control as the
fallback.

Both auto-load triggers share one direction-agnostic gate
(`createAutoLoadGate` in `timelineScroll.ts`). It is gesture-armed:
`disarm()` after every load, re-armed only by a real wheel/touchmove/
keydown (the post-load `shift` compensation is a programmatic scroll and
must not re-arm) plus a 350ms cooldown fallback. Its progress guard
compares the **full** floor cursor
(`oldestLoadedCursor` / `newestLoadedCursor`), turnIndex **and** itemIndex.
Keying the guard on turnIndex alone latched auto-load off on long
single-turn threads, where paging advances the item floor but never the
turn floor (incident bug-report-20260616T143320Z): a 400-item turn 57
loaded once, then the guard read 57 === 57 and refused every later
auto-load until a manual click. Neither direction re-anchors after the
load: the prepend (older) and the head-prune (newer) hold the reading
position through the virtualizer's `shift` head-splice compensation (see
**Load Paging** below), so auto-load-newer never scrolls and
auto-load-older leaves the user exactly where they were reading even if
they keep scrolling as the page arrives.

`threadItemCache.ts` is a small LRU of visible-window snapshots, not a
full-history cache. It rejects oversized snapshots, evicts inactive
threads touched by persisted mutations, and force-evicts same-thread
reloads so revert/reload flows do not paint stale rows.

`mergeMissingItemsById` is the merge contract for initial load and older
paging. Existing in-memory rows keep their references; missing rows are
added and the result is sorted. This preserves row identity (and the
engine's index-keyed measurements) while remaining correct under
persist-then-emit ordering.

Sort-position changes are the one merge outcome that is neither a tail
nor a head change: an upsert that moves a row (e.g. a queued message
repositioned to the turn tail on interrupt) re-sorts `items` at the
same length. The virtualizer compares row keys every beat
(`utils/virtual/keys.ts`); a change that isn't a pure head/tail splice
remaps measured sizes by row identity (`engine.applyKeyedReorder`)
instead of leaving them position-keyed. The remap (not an
invalidation) matters because a moved row keeps its DOM size, so no
ResizeObserver delivery follows the move; a stale position-keyed entry
would never self-correct and rows below the move point would render at
wrong offsets (overlap) until an unrelated resize. The reorder's
compensation is anchor-based: the row under the viewport top (or the
nearest surviving row after it, when a mid-list splice removed it) is
held stationary. That is exact for length-changing keyed splices such as
the review pane's collapse/expand, not just same-length reorders.

## Load Paging (keyed mutation inference)

`loadOlder` grows the window at the head and never drops the tail; a
successful page also sets the pane's `userPinnedHistory` latch (below).
`loadNewer` grows the tail and may prune the head, but only while the
window is unpinned. When a paired prune does fire, both mutations commit
before one final Svelte flush. The virtualizer
compares the previous and next key sequences before exposing render data and
classifies the combined change as head, tail, unchanged, or a general keyed
mutation. Callers cannot label a mutation or leave a mode bit armed.

On a pure **head** change `applyLength` splices the size store at the head
(`spliceHead`) and reports a `head-splice` compensation whose target keeps
the viewport stationary. On a pure **tail** change it does neither. A
combined head-and-tail page replacement uses `applyKeyedReorder`, which
carries every surviving measurement by key and anchors the nearest surviving
visible row. Priors need no remap
step across the splice: they resolve per-row against a content
signature (`utils/virtual/priors.ts`), not a position, so there is no
index-keyed prior state left to shift. Duplicate keys fail at the virtualizer
boundary instead of corrupting the measurement map.

`loadNewer` applies its paired prune directly (the dropped end is
always opposite the reading viewport, so there is nothing to veto or restore).
The streaming / settle prune keeps an explicit anchor-survival guard
(`canPreserveTimelineWindow`, below) because it can fire under a
reading viewport where the prune may remove the row under the reader. The pane
owns and commits the retention mutation. The viewport only answers whether
the visible anchor survives. `<TimelineVirtualizer>` independently classifies
the filtered/grouped `revealedNodes`, so a Read group, notification filter,
subagent group, or reveal boundary cannot make pane-level direction metadata
corrupt the rendered size store.

## Live Window Bounds

The streaming append path caps the loaded window
(`ACTIVE_TIMELINE_WINDOW_MAX_ITEMS`, pruning back to
`ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS`). Two rules bound every cap and
cut in `threadTimelineWindow.svelte.ts`:

- **Caps count top-level rows only**, and prune cuts select by an item's
  top-level root, so children always travel with their anchor — the
  frontend half of the backend pagers' `topLevelItemsFilter` rule.
  Subagent children render inside their anchor's card (or the agent
  companion, whose held rows every cut also keeps), so counting them let
  a busy agent's invisible child mass force the prune into evicting the
  visible conversation (incident 2026-08-31: one launch card left, and a
  "Load older" whose pages the next prune cycle ate again).
- **User paging pins the window** (`userPinnedHistory`): after a
  successful `loadOlder` or a `loadUntilItem` recenter, no automatic
  prune runs at all — history the reader explicitly loaded is never
  taken back for a few MB of summary rows. The pin clears when the
  window is rebuilt at a bounded size: thread switch, cache restore,
  `loadRecentTail`.

A mounted timeline normally defers the
prune to visual quiet because reconciling hundreds of rows is still expensive
main-thread work. Correctness no longer depends on that timing. The
virtualizer keeps surviving rows on one stable mounted paint plane, preserves
their local coordinates across keyed structural changes, and lets the plane
origin absorb content-space relocation. The outer spacer changes document
height without becoming the raster surface. This prevents the previous
remove-all intermediate render. Surviving rows keep their raster even when
the hard ceiling forces a prune during activity. If that ceiling removes the
visible anchor, the resulting content replacement still has to paint and may
jump, but it does not pass through an empty intermediate frame.

Wire settle is not the end of the visible
stream: the reveal smoother keeps draining the tail for seconds after
the turn completes, so a settle-time prune landed its flush, the most
expensive in the app (78–186ms in the bug-report-20260801T214455Z
traces), inside the glide the reader was watching. `settleTurn`
therefore only *records* the prune as pending
(`settleRecentWindowPrune`) when a mounted timeline is behind the pane;
the quiet scheduler (`timelineQuietWork.ts`) runs
`retryDeferredRecentWindowPrune` once no glide is running or armed. A
pane with no timeline (discussion surface, headless) prunes at settle
directly.
`ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS` is the memory backstop and
the only force: back-to-back turns that never reach quiet keep deferring
until the ceiling prunes mid-stream.

The streaming / settle window prune goes through `MessageTimeline` when a
timeline is mounted. The pane owns the window decision and mutation. The
timeline contributes only the synchronous `canPreserveTimelineWindow` guard.
Bottom intent is always preservable because the controller owns the resulting
pin. Reading state is preservable when the first visible item survives. If a
normal recent-window prune would drop that visible anchor,
the pane defers it and retries when bottom intent returns instead of
re-asking on every append. The hard ceiling is the only exception; it
forces the prune even when anchor preservation vetoes the operation, and
it is independent of provider turn state.

Subagent child rows get a tighter bound than the window cap. Streaming
children must live in `pane.items` (the delta pipeline applies only to
loaded rows), but once a child settles and nothing can render it
(collapsed inline card, backgrounded launch, Codex spawn), the pane evicts
the row and folds its count/preview into a per-anchor registry
(`utils/subagentFold.ts`). Collapsing an expanded card evicts its settled
subtree the same way. Card expansion re-hydrates from SQLite via
`ListSubagentDescendants` and reclaims the folded ids. An id is folded
XOR loaded, never both. Folds ride the thread-switch snapshot cache with
the window they describe, and a folded id arriving again over the wire
(reconnect replay) is dropped, not re-inserted. Net effect: per-pane
subagent memory is O(active children), not O(transcript), and the window
cap counts only renderable rows.

## Run Height Changes

A deliberate change to a run's height goes through
`PaneScrollController.preserveViewportBottom` (`timelineWindowAnchor.svelte.ts`,
the same module as the prune transaction, reached via
`withViewportBottomHeld`). Today that is every activity-run collapse or
expand: the ones the reader asked for (a toggle, the header's collapse-all)
and the auto-collapse gate's batched off-screen releases. The hold is not a
call-site convention. It lives inside the registry's own mutators
(`ThreadActivityRuns.setCollapsed` / `setAllCollapsed` /
`releaseOpenedLive`), so a new caller cannot forget it and must not wrap a
hold of its own. The one hold-free write is `expandForReveal`, reachable
only through `revealActivityRunItem`, and the missing hold is load-bearing:
`scrollToItem` guards its post-reveal resume on a restore token, and a hold
issues one (`nextRestoreToken`). Held, the jump would abort at its own
guard before scrolling anywhere. A bottom restore racing the jump's
destination is the second reason. The verb can only expand, so the
hold-free path cannot be borrowed for a collapse. The releases are
not reader-requested (the gate fires precisely because the reader is
provably elsewhere), but they ride the same transaction: the runs they
change are out of sight by construction, and the anchor restore is what
makes that a guarantee for a mid-list reader rather than a property of
engine estimate compensation. One caveat follows from being unasked: the
gate runs as a 'quiet' pass of the quiet scheduler (`timelineQuietWork.ts`),
so it never fires while `autoScrollInFlight()` reports a glide running or
armed. The transaction's bottom-pinned restore is a direct write and would
snap an animation the reader is watching; explicit clicks keep their
instant behavior, because for them the snap IS the intent. The stand-down can only
see motion that exists when the pass runs, so the gate's transaction also
restores with takeover `'yield'` (reader-asked toggles keep the default
`'claim'`): a wire append landing between the release and its restore arms
the structural one-shot, and the yielded `requestBottom` hands the trip to
the armed spring instead of writing a bottom that already contains the new
row. The transaction shares the prune's shape (capture intent, pause the
spring, run the change, restore after the flush) and differs in which edge
it holds:

- **The prune holds the TOP.** Rows vanish from an edge nobody is looking at,
  so the first visible row keeps its offset and the change is absorbed below.
- **A toggle holds the BOTTOM** (`captureTimelineTailAnchor`, restored with
  `scrollToIndex(align: 'end')`). The change is absorbed ABOVE the viewport:
  an expanded run reveals itself over the rows the reader is already reading
  instead of shoving them down the page, and collapsing gives the height back
  the same way. Holding a row above the change is what makes a toggle "open
  downward", which is the wrong direction for something they clicked.

The `pauseAutoScroll` half is not incidental. Without it an expand while
bottom-pinned reaches the controller as content growth and springs across the
whole delta, which for a collapse-all is most of the conversation, animated. Under the
transaction the sticky branch instantly re-pins the new bottom, so there is
nothing left to animate.

Both restores rely on `scrollToIndex` converging: the target is recomputed as
measurements land, so one `tick()` is enough to schedule a restore whose rows
have not been measured yet. The pending navigation outlives the transaction by
settle windows of real time, so it carries a takeover guard: a pass only
continues while the viewport still sits where the navigation's own writes (and
compensations delivered on its behalf, and the browser's clamp when the
content under it shrinks) left it. Anything else moving the viewport (a
reader gesture, the spring's next glide) cancels it, because a stale
absolute target re-fired over new motion is a visible yank (the
release-then-glide "snaps mid-animation" bug; regression:
`timelineVirtualizer.browser.test.ts` takeover tests,
`activityRunAutoCollapse.browser.test.ts` glide-yank test). The takeover
guard cannot see growth the navigation itself keeps chasing, so align-end
convergence also excludes the DESTINATION row's own growth: once a pass has
written against a measured destination, size gained past that baseline is
subtracted from later targets. An align-end target decomposes as
offset + size − viewport, and only the offset half (a fold's RO landing
above, an estimate correcting) is the navigation's to hold. The size half
growing is live content; chasing it re-fired the navigation as instant
writes when streaming resumed inside the settle window of an auto-collapse
bottom restore (bug-report-20260731T211929Z; regression:
`appendAfterQuiet.browser.test.ts`, the settle-window test).

**Bounded by the scrollback that exists.** Opening upward spends `scrollTop`,
and a run near the top of a thread may not have enough. The write clamps at 0
and the remainder falls downward. Inherent (nothing scrolls above the first
row), not a fallback path.

## Warm-Up And Restore

A fresh timeline mount places unmeasured rows at estimated heights and
corrects them as the per-row ResizeObserver fires. On long threads that
correction can shift the viewport by hundreds of pixels, so chat hides
`contentEl` while `!stick.isWarm`.

Warm-up stays closed until a content ResizeObserver event has fired and
then gone quiet for `QUIET_MS`, or until `FAILSAFE_MS` elapses. The quiet
timer is gated on ResizeObserver evidence; do not replace it with a plain
wall-clock delay.

A consumer that knows its async typesetting (the markdown renderer's
katex/mermaid) has settled can pass `quietContextSignal` to shorten
the quiet window to `SETTLED_QUIET_MS` (~one frame). That shortcut is
itself gated on **geometry stability**: `quietContextSignal` is blind to
the engine's estimate→measure cascade, which grows `scrollHeight` over a
series of contentRO fires spaced wider than `SETTLED_QUIET_MS`. Revealing
on the short window mid-cascade shows the surface land right, flicker as
the cascade finishes, then land right again. That is the idle-thread
flicker. So
the controller only takes the short window when the latest contentRO
height delta is `≤ WARMUP_SETTLE_EPSILON_PX`; a larger delta (or the
first fire, which has no baseline) holds the conservative `QUIET_MS`
window, which each cascade fire resets so it closes only once the cascade
goes quiet. A height delta of exactly `0` (a width-only / padding-var
reflow) carries no new height information, so the gate keeps the prior
magnitude rather than reading it as "settled". Otherwise a reflow firing
in the gap between two cascade steps would trip the short window early
(the cold-boot residual, where steps are far apart and font/layout
reflows fire in the gaps). Reveal tracks `scrollHeight` stability, never a
guess at how long the cascade takes. This lengthens the hidden window for
genuinely cascading threads (the ones that would have flickered) by the
minimum needed; `FAILSAFE_MS` still caps the worst case.

The signal MessageTimeline supplies is composed:
`anyMarkdownSettledSinceArm || mountedMarkdownCount === 0`. The second
term is **settled-by-absence**: a mounted window containing zero
`ChatMarkdown` rows (all tool output / terminals / images) has no async
typesetting coming, so it must not sit behind the conservative window
until the failsafe. Presence is a live count registered through
`CHAT_MARKDOWN_PRESENCE_CONTEXT`, which makes the signal *withdrawable*:
a markdown row mounting after the quiet timer armed flips the signal
back to falsy, and `notifyQuietContextSignalChanged` then DISARMS the
armed timer. The settled-by-absence license is gone, and only an earned
settle (or the failsafe) may open the gate.

The geometry gate only ever masks the cascade. It cannot prevent it,
because the cascade settles in bursts spaced wider than any safe quiet
window (trace `bug-report-20260622T225817Z`: a final +200–500px burst
landed ~160ms after reveal, longer than even `QUIET_MS`). For a
**revisited** thread the cascade is instead *eliminated*: the engine's
row estimate resolves each unmeasured row from the previous visit's
persisted measurement (the priors replay, described in "Row And Payload
State" below), so the surface mounts at its final height. Chat's
engine-sourced geometry samples carry the per-row settle evidence that
proves it (every mounted row measured, all within
`WARMUP_SETTLE_EPSILON_PX` of their estimates), and the warm gate then
reveals **immediately**
(`markWarm('settled')`), skipping the quiet wait entirely, once the
markdown-settled signal also confirms no late typesetting wave is
coming. A cold mount's corrections are tens-to-hundreds of px, so it can
never fast-path; late async growth (mermaid, images) lands as a
correction and holds the gate exactly as before. Priors are per-row and
persist past a restart (`utils/virtual/priorsStorage.ts`), so the
"revisited" case above now also covers a thread reopened after a fresh
app boot, not just a same-session thread switch. The boot window only
needs to be a suffix of a previously-captured, possibly much larger,
window (see priors.ts's header for why the per-row key makes that
robust to window-composition changes). The geometry gate therefore
guards only a **genuine first-ever** visit to a thread, or rows whose
content changed since their last capture (a per-row signature miss).
Those are the cases where no valid prior exists and the estimate→measure
cascade is unavoidable, and the gate is best-effort there. Both defenses coexist: priors
remove the cascade when they can, the gate hides it when they can't.

On thread switch, `MessageTimeline` must call `stick.armWarmup()` from
`$effect.pre` so `isWarm=false` before the new DOM paints. The restore
effect then calls `stick.forceStick({ reason: 'restore' })` and schedules
one rAF `observe('content')` settle pass for late composer padding, row
measurement, or Streamdown layout changes. The rAF pass is escape-aware.

**That arm does not survive a fetch.** On the cache-miss path the pane
sits EMPTY for the whole `ListThreadSliceAround` round trip, and an empty
mount window is still a content-geometry delivery. The virtualizer
reports `totalSize` 0, which arms the quiet timer exactly as a real
cascade fire would, and the gate opens ~`QUIET_MS` in, against nothing.
(The gate's own "arm the quiet timer only on geometry evidence" rule
covers *no source yet*, not *no content yet*; under the RO-backed source
the two coincided, because an unmounted `contentEl` has no observer.) So
the rows that arrive 100–300ms later would mount through an open gate and
run their estimate cascade in front of the reader.

The **initial-slice re-arm** closes it again: `applyInitialSlice`'s call
site in `threadPaneScroll.svelte.ts` (`armInitialSliceWarmup`) calls
`PaneScrollController.armWarmup` synchronously with the item mutation
(strictly before the flush that mounts those rows, the same ordering
contract `markStructuralContentPending` carries), so the hide covers the
mount from its first paint to the quiet edge, and `FAILSAFE_MS` still
bounds it. Scope is exact, and each exclusion is a blank flash avoided
rather than a missing case:

- **Initial slice only.** Streaming appends, reveal-gate releases, and
  load-older/newer pages mount against content the reader is already
  looking at.
- **Rows only.** An empty result re-arms nothing: there is no cascade to
  hide, and holding the gate would sync-pin the first streamed tokens
  instead of gliding them and park an empty pane behind the 2.5s
  failsafe. Empty panes stay visible, as the placeholder→materialized
  path already treats them.
- **Chat only.** A discussion pane registers ChannelView's controller
  over an unrelated surface (the same stand-down `armStructuralSpring`
  makes).
- **First content mount only.** With the IndexedDB replica
  (`docs/architecture/thread-replica-sync.md` §6.1) a cold open can mount rows
  twice: the durable paint, then the `SyncThreadWindow` page that
  replaces it. The paint re-arms (it is an initial slice in every way
  that matters to the gate), and the page does NOT, because by then the
  reader may already be looking at those rows and re-closing the gate
  would blank content that is on screen. `runItemWindowSync`'s
  `paintSource` is the discriminator, and it rides the cold-load trace
  alongside the sync verdict.

One deliberate behavior change rides the sync-based cold open: a warm
re-entry to a thread the user had deep-paged (say 800 rows in the L1
snapshot) collapses back to the slice budget (~200 around the anchor)
whenever the sync answer is not `fresh`. The page replaces the painted
window, and rows outside it are re-fetched lazily on scroll like any
cold open. Keeping them would merge rows from an older attestation
under a newer stamp, the one composition the replica's understate rule
forbids (`docs/architecture/thread-replica-sync.md` §6.1 step 4), and it
matches what the transport-gap `refreshFromBackend` path already does.
A `fresh` answer, the common case for an unchanged thread, keeps the
full painted window.

The chat adapter maps it to `armWarmupWithReset`, not the bare controller
call: the incoming rows' markdown has not typeset yet, so the
settled-since-arm latch has to start the cycle false or the shortened
quiet window would open on a stale settle.

Same-thread reloads must watch `pane.switchGeneration`, not just
`pane.threadId`, because revert/reload can replace items without changing
the thread id.

The controller also exposes `warmReason` alongside `isWarm`: which of
`'quiet'` / `'failsafe'` / `'settled'` / `'skip'` last opened the gate, or
`null` before it has opened for the current cycle. It is reporting-only
(see "Cold-load trace" under Diagnostics); no consumer should branch
scroll behavior on it.

## Intent And Programmatic Writes

Bottom geometry and user intent are separate. The viewport can be near
the bottom while the user has escaped bottom-follow. Upward wheel, key,
touch, middle-button autoscroll, or scrollbar-gutter pointer intent
escapes synchronously when it targets the timeline or a descendant.

A recent down-intent window is the only consent path for re-sticking after
escape. Re-stick uses the distance from bottom captured at scroll-event
time so streaming growth between the event and the deferred check cannot
invalidate what the user actually saw.

A selection drag pauses the spring in place and escapes on the scroll
events it causes (`isSelectingInside`: the primary button is held AND the
selection's range shares ancestry with the scroller; a collapsed caret
counts, matching the upstream port). The button half is READ from
`event.buttons` on the pointer stream, never latched from a
mousedown/mouseup pair: a native drag-and-drop (pane title handle,
sidebar row, browser tab) fires mousedown and then no mouseup or click,
and the latched version stayed "held" until the next click, so every
spring in the app re-armed without writing while `isAtBottom` stayed true
and no jump button appeared (bug-report-20260903T221457Z). `dragstart`
and window `blur` clear it synchronously; `pointermove` resyncs. Touch is
excluded (a finger is a scroll or a tap). The real-browser proof that a
drag swallows the release lives in
`scroll/selectionTracking.browser.test.ts`.

Programmatic scrolls go through the controller:

- `forceStick({ reason: 'user' })` for explicit bottom-follow.
- `armRestoreSnap()` (sets the defensive escape, then arms consent)
  followed by `forceStick({ reason: 'restore' })` for thread/channel
  restore.
- `markAtBottom()` for empty-timeline restore without writing scrollTop.
- `requestBottom({ takeover })` for every out-of-band "put the reader at
  the bottom" placement: transaction restores
  (`timelineWindowAnchor.svelte.ts`), the pause-release re-pin, and the
  host-layout re-pin. The takeover is the arbitration rule that replaced
  those callers' pairwise stand-down guards: `'claim'` (the reader asked,
  as in a disclosure click) cancels any bottom-follow program and places
  instantly, because user intent always may retarget the viewport;
  `'yield'` (the system asked) hands the trip to an engaged program (a
  glide running or a structural-append arm holding one ready, exactly
  `autoScrollInFlight()`) and writes nothing, because a one-shot
  absolute write landing mid-glide collapses an animation the reader is
  watching into a snap. The escape rule lives inside the API, not in
  caller discipline: a `'yield'` while `escapedFromLock` writes nothing
  at all, and a `'claim'` ends the escape (user intent re-establishing
  bottom follow, with `markAtBottom`'s intent-state sweep.
  `forceStick` routes its own cancel-and-place through this same claim
  path). Virtualized surfaces pass a `write` callback
  (`scrollToIndex(last, {align:'end'})` + `markAtBottom()`) so
  placement converges through the engine; beyond escape,
  `requestBottom` presumes bottom intent, so callers gate on their own
  "was the reader holding the bottom" predicate first.
- `observe(kind)` for out-of-content geometry changes ('content',
  'live-content', 'composer-geometry', 'host-layout').
- `pauseAutoScroll()` for drag/resize leases.

The virtualizer's own imperative scrolls route through the same
chokepoint: `listRef.scrollToIndex(...)` computes the target in the
engine and performs the write via the controller's `applyScrollTarget`
(wired as a `<TimelineVirtualizer>` prop), so index scrolls are tagged
and attributed like every other controller write, with no wrapper call at
the call site. Engine compensation observations arrive through
`applyEngineCompensation` (see **Engine Compensation Routing**).

Because every programmatic write flows through the chokepoint and every
user gesture is classified by the intent machine, the controller keeps a
one-field **provenance ledger**: the last explained `scrollTop` (the
chokepoint's browser-rounded readback, or the position of the last
user-classified scroll event, with resize-correlated events deliberately
excluded, since a clamp's own scroll event is one). During a spring
sentinel the only mover the ledger cannot account for is the browser's
max-scroll clamp, so "live scrollTop differs from the ledger" is
witnessed clamp EVIDENCE. Both stranded-oscillation recoveries (the
resolver's `isSentinelOscillationStranded` and the spring tick's guard)
require that evidence in addition to the sentinel-entry baseline match.
That match is latched per sentinel session, so an authored write
ratifying the clamped position cannot launder a strand. An authored
displacement with the same
numeric shape (a head-splice compensation's anchor hold) updates the
ledger and therefore glides its hidden growth in instead of snapping
(bug-report-20260801T213259Z).

Never write `scrollTop` directly from feature code. The virtualizer's
`scrollToIndex` is instant-only by design, because native smooth scrolling
emits asynchronous scroll events that race the controller's tagging; do not add
a smooth option, and never call `scrollIntoView()` on a virtualized row.

`prefers-reduced-motion: reduce` forces sync-pin behavior regardless of
requested animation mode. The app's `lowPowerMode` setting rides the same
gate (`motionReduced()` in the controller): spring glides keep rAF,
geometry reads, scroll writes, and browser paint work active every frame,
so low power means instant placement everywhere, including landing an
in-flight chase on its next tick when the setting flips mid-glide. The
same setting also snaps the streaming reveal to per-wire-chunk mutations
(`PerItemSmoother`'s `revealImmediately` seam) and suppresses the
activity shimmer.

## Engine Compensation Routing

The engine compensates for geometry changes that move content above the
viewport: above-viewport row remeasures (`remeasure-above`) and load-page
head splices (`head-splice`). It does so by **reporting** an absolute
scroll target instead of writing `scrollTop` itself. `TimelineVirtualizer` forwards each
`EngineCompensation` to the controller's `applyEngineCompensation`, which
delegates the decision to the pure `resolveEngineCompensation`
(`utils/scroll/resolver.ts`) and applies at most one write through the
controller's chokepoint. The controller is the single `scrollTop` writer
during follow by construction (browser clamps are native and untagged).

Delivery timing is load-bearing: the virtualizer queues the compensation
and delivers it from a post-flush `$effect` (after the spacer height and
row offsets reflect the new geometry, still before paint), so the
resolver samples live `scrollHeight`, and a target beyond the old maximum
cannot clamp. The rationale lives in `TimelineVirtualizer.svelte`
("Post-flush write timing"); the regression test is the "write timing"
describe in `timelineVirtualizer.browser.test.ts`.

### The row that spans the viewport top

Rows entirely above the top compensate by their exact size delta; rows at
or below it need nothing. The at-most-one row **straddling** the top is
neither: growth in its off-screen-above part shifts everything visible
down, growth below the top is ordinary reflow, and whole-row
`[index, height]` cannot say which happened. It historically compensated
nothing, leaving up to one row's worth of uncompensated shift whenever a
tall row's off-screen head grew (late KaTeX/mermaid, a decoding image, a
width reflow re-wrapping the window).

`utils/virtual/readingAnchor.ts` supplies the missing split by hit-testing
the element painted at the viewport top and recording its offset **from
the top of its own row**. Row-relative is the whole trick: rows are
absolutely positioned at engine offsets, so a row's own position is a pure
function of the above-rows arithmetic that is already exact. Anything that
moves the anchor *within* its row is intra-row growth above the reading
position and nothing else, so the two corrections compose without any
chance of double-counting.

The engine stays DOM-free: `applyMeasurements` takes an optional measurer
and calls it only for the straddling row, only when that row's height
actually moved. `boundStraddleShift` then clamps the answer to the row's
own delta (same sign, no greater magnitude), a physical bound, since the
part above the reading position is a *part* of the whole. That is what
keeps the DOM measurement non-load-bearing: a stale anchor, a re-rendered
subtree, or a NaN can only pull the correction back toward zero (toward
the historical behavior), never past it into an over-correction.

Tracking is gated on `trackReadingAnchor`, which chat wires to "the
controller does *not* hold bottom-follow intent". While pinned, the
per-beat pin write already absorbs growth anywhere in the content, so the
correction is unnecessary and a hit-test per scroll event would be pure
cost. The gate is re-read when the measurer is *called*, not only when the
anchor is sampled: bottom-follow can be regained with no scroll event and
no measurement pass (`markAtBottom`, `forceStick`, the resolver's own
`setIsAtBottom`), and a stale armed anchor would otherwise land a sub-row
correction on top of the pin write. Coverage: the "straddling-row
attribution" describe in `timelineVirtualizer.browser.test.ts` (real
layout, including that transition), `readingAnchor.test.ts` for the
measurement, and `engine.test.ts` for the attribution and its bound.

The resolver's decision order (each tier's regression provenance is
documented at the function):

- **head-splice pass** applies head mutations (load-older prepend, paged
  head-drop prune, a tail-following run clip's window advance)
  verbatim: the engine's offset math is exact and the anchor must hold.
  A head splice displaces `scrollTop` while the content height, and so
  the bottom target, stays unchanged, which is byte-for-byte the shape of a
  browser clamp after a content dip-and-restore; the provenance ledger
  is what keeps the sentinel's oscillation guards from snapping the
  splice's stated growth in instead of gliding it
  (bug-report-20260801T213259Z, think → bash inside a run clip). The
  splice's write goes through the chokepoint, so the ledger explains
  the displaced position and the guards find no clamp evidence. No
  special-casing at this tier.
- **reading pass** (not warm, not at bottom, escaped, or paused): the
  compensation lands unchanged (mount cascades, mid-thread reading).
  Above-viewport visual stability is the whole point; suppressing these
  visibly shifts the reading anchor.
- **anchor-redirect** applies when the compensation requests a target
  meaningfully *above* the bottom target while intent is pinned and
  either (a) the DOM is already at true bottom (within
  `AUTO_FOLLOW_BOTTOM_EPSILON_PX`) or (b) no spring chase is in flight
  (`!springActive`, the idle displaced case): the write is rewritten to
  `targetScrollTop()`. The engine's `delta` only compensates
  above-viewport remeasures, not the at/below-fold growth that pushed
  the bottom down, so letting the requested value land paints short of
  bottom. That is the cold thread-switch flicker for (a); for (b), a post-warm
  remeasure burst that grows the total in the same delivery defeats the
  pinned-DOM check and strands the viewport viewports above the bottom,
  displacement the next growth's spring then pays off as a multi-second
  visible glide (bug-report-20260822T020840Z, seq 64892: an idle
  reopen's `remeasure-above` left an 8118px gap). Only an in-flight
  chase keeps verbatim relocation, because the spring re-reads
  `el.scrollTop` per tick, so relocating it is loss-free.
  A redirect that actually moved the viewport (landed more than the
  epsilon from where it was) also opens the observers' 250ms
  **pinned-remeasure settle window**: contentRO deltas inside it carry
  `pinnedRemeasureActive` and sync-pin instead of gliding, because they
  are the same correction wave still landing, not the bottom advancing.
  The window is fixed, deliberately not refreshed by the deltas it
  classifies, so a streaming turn that starts inside it cannot be
  converted into indefinite sync-pins.
  The **cold-load settle window** is the second announcer of the same
  fact, with a different lifecycle: every warm-up arm (attach, restore
  forceStick, the slice application's `armWarmup`) opens it, and while
  it is open post-warm growth sync-pins too. The warm gate opens on
  ~100ms of RO quiet, but the estimate→measure cascade and the window
  sync land bursts seconds later (2026-08-22 boot restart: 8.5kpx of
  measurement growth glided for ~2s, then an unrelated bottom-held
  transaction snapped it). It ends for good at the first delivery that
  observes live content or an armed structural append (from then on
  glides own the pane), with an 8s cap as the failsafe for a pane that
  never streams; `skipWarmup` (placeholder materialization) clears it.
  Both signals feed the one `pinnedRemeasureActive` resolver input; the
  trace records them separately (`coldLoadSettleActive`).
- **pass**: anything else applies verbatim, mid-chase included. The
  compensation is an exact coordinate shift: layout moved the content
  under the viewport by `delta`, and the write moves the viewport by the
  same `delta` before paint, holding the visual field stationary. The
  spring re-reads `el.scrollTop` every tick, so an applied write
  mid-chase just relocates the chase. The remaining gap is unchanged
  and the glide continues.

There is deliberately no **mid-chase decline** tier. The virtua-era gate
declined sub-viewport compensations while a spring chase was in flight,
but declining an exact compensation is what *caused* the visible jump: a
background completion patching its collapsed tool row above the viewport
shifted the content under a stationary viewport by the row's height
delta, then the spring re-chased the same distance (2026-07-21). Nor is
there an animation tier: the resolver's engaged tiers key on observed
geometry (`pinned` + `moves-away`), which makes spring-lifecycle timing
irrelevant to compensation handling. That is what retired the cross-file
invariant that the animation-mode hold outlast
`RETAIN_ANIMATION_DURATION_MS` (and, later, the mode latch itself).

Routed writes are controller writes: attributed (`engine.compensation` /
`engine.anchorRedirect` in the `scroll.write` trace) and self-tagged for
the scroll handler like every other programmatic write. Decision-level
coverage lives in `resolver.test.ts`; the controller wiring in
`scroll/index.svelte.test.ts` ("engine compensation applier"); the
adapter seam (delivery timing, windowing, measurement) in
`timelineVirtualizer.browser.test.ts`; and the user-visible outcomes in
`compensationOutcome.browser.test.ts`.

## Live Content Animation

Autonomous content growth while pinned at the bottom has **one**
behavior: the chase glides. There is no per-delivery animation mode and
no "was this real content?" classifier in the physics path. The resolver
asks only whether springing is *allowed* right now
(`springGateIsOpen({springStopRequested, paused, isAtBottom, escaped,
prefersReducedMotion})`), every input being an explicit signal about
scroll state or user preference, never a guess about what produced the
pixels.

This is deliberate. The controller cannot distinguish "content arrived"
from "layout got corrected": a shiki highlight resolving, KaTeX
typesetting, a mermaid diagram sizing, an image decoding, and a text
chunk landing all reach it as the same thing: the content box got
taller under a pinned viewport. The retired `animationMode` latch
guessed, keyed on a 500ms window after the last *stamped* content
advance, and every growth that landed outside a stamp window teleported
instead of gliding. Both of the 2026-07-25 jank reports were that one
bug: a background command completing while the agent was idle (nothing
stamps, the row jumps in), and post-turn markdown/highlight drain
(stamps lapse mid-settle, so the tail alternates glide and jump).
Misclassification is now impossible because there is no classification;
both routes end at the same scrollTop, so the worst case degrades from a
teleport to a slightly unnecessary glide.

`prefers-reduced-motion` (OS setting or the app's low-power toggle) is
the one input that turns growth back into an instant pin, which is the
correct semantic for it.

### Liveness is a separate question

`lastLiveContentAt` still exists, and `ThreadPane` still stamps it
whenever live timeline content advances: assistant prose, thinking,
compaction reasoning, direct text patches, visible-field updates to
already mounted rows (a running tool row growing its output preview per
flush window, or running→completed result chrome landing), and wire
appends / reveal-gate releases entering the loaded tail (via
`armLiveContentAppendSpring`, below). `MessageTimeline` and `ChannelView`
turn it into a boolean with `isLiveContentActive(now, lastLiveContentAt,
LIVE_CONTENT_ACTIVE_HOLD_MS)` and pass it as the controller's
`liveContentActive` option.

It answers a different question: **is more content expected imminently?**
Two consumers, neither of them the physics choice:

1. **The spring sentinel** (`spring.ts`). When a chase arrives and no
   target change has landed within `RETAIN_ANIMATION_DURATION_MS`, an
   active liveness reading keeps the sentinel alive across the gap
   instead of cancelling. That is what holds `springActive` true for
   the springActive-keyed resolver carve-outs (negative-delta,
   overshoot, idle deadband) through an inter-chunk pause. Without the
   distinction the sentinel would be immortal: a permanent 60Hz rAF per
   pane, since growth alone would always want a spring.
2. **The viewport path** (`notifyLiveContentMaybeGrew`). That entry
   point also carries *viewport* changes (the composer growing under a
   multi-line draft), where an instant pin is correct while the thread
   is idle. Liveness (or a pending structural append) is what
   distinguishes the two there, but only from rest. A chase already in
   flight absorbs the observation regardless of liveness
   (`absorbedByActiveSpring`, shared with the plain content path): both
   clocks are short (500ms hold / 250ms one-shot) and mid-chase
   retargets deliberately refresh neither, so a glide extended by async
   row settling (payload previews, highlight spans) outlives them while
   still animating. That is the tool-call boundary where a composer-rail
   resize used to land as an instant write over the running animation.
   Only a large overshoot (content collapsed out from under the
   viewport) still snaps through, matching the resolver's mid-spring
   threshold.

Getting liveness wrong is cheap by construction: the worst outcome is a
sentinel restart or one extra rAF, never a teleport. The 500ms hold is
pure tuning, and unlike the latch it replaced it is not load-bearing for
motion quality.

Structural transcript appends additionally have a one-shot:
`markStructuralContentPending()` marks the next near-term command/tool
row growth as append-driven even when nothing stamped liveness. When a
chase starts from the one-shot alone, it cancels on arrival instead of
entering the streaming sentinel, so later negative geometry corrections
sync-pin invisibly instead of deferring to a dead chase after the append
settles.

The pane data layer is the sole owner of the arm, with two arm shapes in
`threadPaneScroll.svelte.ts`: `armLiveContentAppendSpring` (arm + liveness stamp)
for `applyProviderItemUpserts` (a wire append to the loaded tail) and
`recomputeRevealPass` (the reveal gate releasing withheld rows, which are
already in `pane.items` and mount without any upsert in that flush), and bare
`armStructuralSpring` (arm only, no stamp) for the composer's optimistic
user-send. All arm sites run
synchronously with the data change, strictly before the Svelte flush in
which the virtualizer measures the new/released rows and delivers their
geometry sample. An effect-based arm (MessageTimeline's former
live-follow signature effect) loses that ordering race, so the append's
own growth is seen a frame late by the sentinel and the viewport path;
a turn-keyed effect is additionally blind to appends landing after turn
end (interrupt echo, force-closed tool rows), which under the retired
mode latch sync-pinned as whole-viewport teleports
(bug-report-20260702T193212Z). The arm is a TTL refresh
(250ms), so double-arming (an upsert whose recompute also releases rows)
is harmless. Each arm also schedules a one-frame-after-flush
`observe('live-content')` nudge, so growth that never fires a
content-geometry delta (a thinking row tail-pins its clipped body
internally while the next top-level row mounts) still gets a bottom
re-check; a monotonic token cancels superseded nudges and a
`switchGeneration` capture cancels stale ones across switch/reload/clear.

Restore safety is layered: `armStructuralSpring` itself gates on
`pane.loading` (the whole switch+load settle is a restore, not an
in-turn append, per bug-report-20260622T041049Z), the reveal arm
additionally requires that the boundary change actually releases rows
that still exist (`boundaryChangeReleasesRows`: a gate dropping because
the lone streaming row drained, or because a revert removed the tail,
mounts nothing) and never fires across a switch because
`disposeAllSmoothers` nulls the boundary first, and the controller's
warm gate independently pins the post-restore settle. Discussion-surface
panes are skipped entirely (`threadUsesDiscussionSurface`, shared with
ChatView's surface swap): their registered controller belongs to
ChannelView, so arming would open a spring window on unrelated
channel-message growth.

`spring` is an eligibility signal, not an unconditional animation. If
`contentRO` observes a content-width change, the controller opens a short
width-reflow settle window and sync-pins paired height corrections. This
keeps pane, sidebar, and window reflows (including Mermaid `useMaxWidth`
height changes in the rendered window) from producing a visible
half-viewport spring chase just because live content advanced recently.
During that window, the compensation resolver passes the engine's
anchor-preserving writes for the same reason.

Negative content deltas usually sync-pin when the user intends to stick,
but a small negative correction during an active spring is absorbed by
the spring so estimate/correct row measurement pairs do not snap the
viewport. Width-driven shrink corrections and large overshoots still
snap immediately.

At idle, with the spring settled (`springToken === 0`) and pinned at the
bottom, the content box height can flip ±1-2px every ResizeObserver delivery when
the fractional sub-pixel total lands on an X.5 boundary under a fractional
device-pixel-ratio (WSLg / HiDPI). The bottom target
(`scrollHeight - clientHeight`) flips with it, and the `contentRO`
positive/negative-delta sync-pins re-pin `scrollTop` to that moving target
on every wobble frame, a self-sustaining ±2px limit cycle (the whole idle
viewport visibly vibrates). The **idle re-pin deadband** breaks it: when no
spring is in flight and `scrollTop` is already within
`IDLE_REPIN_DEADBAND_PX` of the target, the re-pin is skipped
(`idlePinWithinDeadband`, folded into both pin predicates, and since the
Stage-2 extraction this decision lives in `utils/scroll/resolver.ts`,
the controller's pure decision core). It keys on distance-from-target,
not delta magnitude,
so genuine growth moves the target ≥ a line height (gap ≫ deadband) and
pins normally; the `springToken === 0` gate makes it idle-scoped by
construction. During streaming the spring holds its token across
inter-chunk gaps, so an active chase is never touched. Full mechanism + the
capture it was root-caused from:
[`settle-flicker-analysis.md`](settle-flicker-analysis.md). Coverage: the
net-zero `±2px` oscillation test (≤2 `scrollTop` writes with the gate, 24
without).

## The Print Doctrine

The transcript renders like print: rows are static ink, and the only
things that move are the page under the reader and the text still being
written. Concretely, exactly two motion owners exist inside the timeline
scroller:

1. **The scroll controller's glide** (`utils/scroll/`) animates the
   scroll offset only. Rows never move relative to the content; the
   viewport moves over them.
2. **The streaming line-slide** (`TailClampedText`'s inverted translateY,
   drained by a rAF tracker — `tailSlide.ts`) is the one in-content
   animation, on the one region that is genuinely in motion: the line
   being streamed.

Everything else is still ink. No CSS transitions (the app.css timeline
kill rule zeroes them, per `components/chat/AGENTS.md` §Row Contract),
no Svelte `transition:`/`in:`/`out:`/`animate:` directives
(`timelineAnimationDirectives.test.ts`), and no keyframe animation on
row content that moves anything but light
(`timelineKeyframeAnimations.test.ts`). The guards make the wrong thing
inert or loud rather than relying on review memory.

"Moves anything but light" is the keyframe rule, and it was briefly
wider. `1633dcea` banned keyframe animations from the scroller
outright, on the theory that any Animation object licenses the
compositor to present before raster lands, and disarmed `animate-pulse`
app-wide to satisfy it. Measurement (below) kept the hazard and killed
the scoping, so that ban bought no quiet frame and charged ~28
whole-document repaints/sec of inline style writes for it. What survives
is the doctrine's own rule: a row may fade, never move. `opacity` adds no
second motion owner and no geometry for the controller's compensation to
fight, so it is the one property allowed, and the guard reads the
`@keyframes` bodies in `app.css` to enforce it. A transform, a size, an
inset: each is a third motion owner and fails the build.

The ambient indicators are all CSS animations on compositable
properties, phase-locked to wall clock by `utils/ambientPhase.ts`:
`ambient-pulse`, `ambient-led`, `ambient-spin`, `working-sprite-run`.
All four are stepped, which is a separate rule with its own incident
(2026-07-04: one smoothly pulsing 6px dot was a standing 165
presents/sec client that stuttered *other applications*) and its own
check in the same file, run over `app.css` rather than over this
directory, because that hazard is document-wide.

One in-scroller-adjacent animation is allowlisted rather than removed:
`MessageTimeline`'s explicit-jump landing flash is an overlay on the
NON-SCROLLING wrapper, a sibling after the scroller in source order,
placed there deliberately so no row gains an animation. A jump is an
instant teleport, not a compensated move.

The guards do not yet cover everything the doctrine claims, because both
walks cover `components/chat/` and the hazard is the scroller's DOM
subtree. Known in-scroller animations outside them, kept deliberately
pending a product call: the `animate-spin` loading ring on
`primitives/Button` (load-older/newer, the message editor's working
state, plan-card expansion: a *transform* animation, so a straight
violation of the fade-never-move rule, and live during exactly the
head-splice compensation commits load paging performs). Removing or
converting it (e.g. to the ticker-driven `SteppedSpinner`) changes
visible behavior, so it is a decision, not a cleanup. `lib/markdown/render/`
is the other subtree the walks miss; it carries no animation or transition
today, and the popovers that used to (`transition:scale|global` on
click-gated dialogs) were deleted with the rest of the library chrome.
(A third exception, the
user-message jump-target glow, turned out to be dead code. Its only
producer died with `DiffPanelDrawer` in the review-pane redesign, and
the whole flash mechanism was removed rather than left dormant.)

This is not an aesthetic preference. Two motion owners on the same
pixels fight, and the compositor makes the fight visible: the browser
has no reason to draw until raster lands, *unless an animation is
active*, which drives a begin-frame every vsync, so it draws on the
frame deadline whether or not the tiles are ready.

That wording was corrected on 2026-08-24 against measurement
(`scripts/perfprobe/present-policy-arms.mjs`; ledger entry in the
perf-investigation skill). This document previously said an animation
flips the scheduler to *smoothness-priority*. It does not:
`tree_priority` never leaves `SAME_PRIORITY_FOR_BOTH_TREES` while
animations run, because that mode is driven by pinch and active
compositor scroll, not by CSS animations. The conclusion held, the named
mechanism did not.

Two properties of the real mechanism do not match how a
"no animation objects in the scroller" rule would be scoped:

- **It is binary, not proportional.** One animation and thirty measure
  the same (20 / 15 / 17 / 17 draws landing during outstanding raster at
  1 / 3 / 14 / 30 animating elements, against 3 with none). The only
  meaningful state is zero, so removing *some* animations from a window
  buys nothing.
- **It is document-wide, not scroller-scoped.** An animation outside the
  scroller scores the same as one inside (18 vs 23). "It mounts outside
  the scroller" is therefore not, on its own, a safety argument, though
  it remains a correct statement about where a component renders.

Zero is unreachable here and that settles it. The working sprite, the
LED chase and the stepped spinner run through every working turn, and
`TailClampedText`'s line-slide runs continuously through streaming. All
four are wanted, and the last is one of the two permitted owners. So the
counting rule was chasing a state the app never enters, which is why the
guard now enforces the motion rule instead. What still narrows the
window is keeping *fresh raster* out of the compensating commit, which
is what the transition kill rule does: a decorative transition on a row
does not merely exist during the toggle, it repaints during it.

Caveat before quoting the numbers: headless, `SoftwareRenderer`, one
raster thread. The arm-to-arm comparison is the result; the absolute
counts are not. The
timeline's core moves are compensated viewport-space moves (bottom-held
toggles, prune shifts, head splices): rows that stay screen-stationary
while their tiles all invalidate at once. Under the default policy that
is invisible; with any animation active in the same commit it is a
checkerboard where text used to be (the 2026-08-17 expand flicker: a
chevron rotate, a fade, and browser-re-dispatched hover transitions were
enough). The two permitted owners are bounded by construction: a scroll
glide changes only `scrollTop` and does not also mutate row geometry, and
the line-slide animates a small clip whose tiles the same commit just
painted.

The doctrine also explains this document's recurring shape: every
mechanism above (bottom-held transactions, engine compensation routing,
the reveal queue's paced drain, the idle re-pin deadband) exists to make
discrete content changes land as either *zero visible motion* or *one
controller-owned glide*. When adding polish to a row, the question is
never "does this animation look nice" but "which of the two owners does
this motion belong to". If the answer is neither, it renders as
print: instant, settled, already there.

## Layout Rules

`ChatView.svelte` positions the composer and live-turn UI as an absolute
overlay. A `--composer-height` CSS variable drives `scrollEl`
`padding-bottom`, keeping composer growth from changing the scroll
surface's `clientHeight`.

The composer ResizeObserver writes `--composer-height` directly before
observing `'composer-geometry'` on the scroll controller. Waiting for
Svelte's microtask flush would pin against stale layout. Idle composer
geometry resolves to the same same-paint pin as a `'content'`
observation; when live content is inside the spring hold window, or a
chase is already in flight however stale the hold
(`absorbedByActiveSpring`), the live-capable path keeps following the
moving bottom instead of sync-pinning mid-chase.

`overflow-anchor: none` belongs on the outer scroll container (and is
also set on the virtualizer's spacer). Browser scroll anchoring otherwise
fights the engine's compensation targets and the controller's sync-pin.

The pane scroll surfaces (chat timeline, discussion channel) draw no
native bar: `.pane-scroll-surface` (app.css) suppresses it and applies
`will-change: scroll-position`, and an `<OverlayScrollbar>` sibling is
the surface's scrollbar with zero layout width in every state, so an
overflow transition can never re-wrap the transcript, and gestures on
the strip state intent through `onUserScrollStart` / `onUserScrollEnd`
(the intent machine's geometric gutter hit test reads
`offsetWidth − clientWidth`, always 0 here by design). The
composited-scrolling hint is why the follow spring's per-frame
`scrollTop` write is cheap: without a composited scrolling layer, every
offset change ran a full main-frame Layerize (~1.1ms × 155/s during
two-pane streaming, 2026-08-25). This retired the
`scrollbar-gutter: stable both-edges` reservation the chat scroller
carried while it had a classic space-consuming bar (the reservation
held the centered column aligned with the composer overlay across the
bar's appearance; with no bar it would inset the column for nothing).

A gutter never transferred to a scroller *inside* the centered column
either: it would inset that box's content relative to the prose above
and below. The activity-run clip suppresses its native bar the same way
and renders the same overlay thumb. See
[Nested scrollers](#the-activity-run-a-nested-scroller-with-the-panes-physics).

Status banners are absolute overlays, not reserved layout slots. They
must not change the scroll surface height on mount/unmount.

## Nested Scrollers And Gesture Attribution

Rows may own scrollable bodies (command output, subagent children,
wait-group children, tool-result patches, payload bodies). Wheel and
touch events bubble, so without help the outer intent machine cannot
tell a gesture aimed at the pane from one a nested box just absorbed,
and treating the latter as "the user left the bottom" broke follow while
the outer pane had not moved at all.

Every such box opts in with the `nestedScroll` action
(`utils/scroll/wheelAttribution.ts`). On each gesture the intent machine
walks `event.target` up to its own scroll element, and the first
registered scroller that can still move in that direction owns the
event; the machine then ignores it. Nothing registered can consume it →
the gesture belongs to the boundary, and native scroll chaining takes it
there. That chaining is deliberate, because browsing up out of a nested
box has to reach the pane, which is why nested boxes keep the default
`overscroll-behavior` rather than `contain`.

A registry, not a computed-style measurement: wheel handling runs while layout
is dirty mid-stream, so `getComputedStyle` over every ancestor of every
wheel event would force reflows at gesture rate. Geometry reads stay
confined to explicitly marked elements, usually zero or one per gesture.

The same helper serves nested controllers: an inner controller passes its
own clip as the boundary, so a command-output box inside an activity run
attributes correctly against both levels.

Adding a new scrollable row body means adding `use:nestedScroll` to it.
Contract: [`scroll-contracts.md`](scroll-contracts.md) C7.

### The activity run: a nested scroller with the pane's physics

Most nested bodies are inert boxes. An activity run's clip is not: the run
holding the live tail gets its own `createUseStickToBottomController`, same
spring as the pane, so streaming activity chases inside
the cap while the prose above it stays put. Rules that matter from the outer
side ([`activity-runs.md`](activity-runs.md) has the rest):

- **Only the tail run**, the newest REVEALED run (`node.atTail`), not the
  `live` one: `live` ends the moment closing prose arrives behind the reveal
  gate, and keying the controller there cancelled a glide the reader was
  watching (the 2026-08-19 in-run jump). Historical runs are plain
  `overflow-y: auto` with a restored `scrollTop`; a controller per run in the
  buffer would be a spring, an observer set, and intent listeners each for
  physics one of them can use.
- **The clip's outer height changes only on explicit events** (growth toward
  the cap, item expansion, a collapse toggle), never from inner streaming.
  That is what keeps the outer engine quiet, and it keeps `rowDelta === 0` for
  the straddling row during inner scrolling, so the reading-anchor measurer
  never sees inner movement.
- **The nested controller leaves `externalContentGeometry` unset** (no
  virtualizer inside a run, so its own contentEl ResizeObserver is the right
  source, following the `ChannelView` precedent) and never touches
  `pane.attachScrollController`.
- **Inner scroll position lives in the per-pane registry**, keyed by the
  registry-assigned `runId`, so a run the reader scrolled inside does not snap
  back to its tail every time the virtualizer evicts its row.
- **The clip's native scrollbar is suppressed to zero width** and the
  affordance is a `components/shared/OverlayScrollbar.svelte` in the column's
  padding. A gutter cannot be used inside the centered column, because it would
  inset the run's rows off the rail the run draws, and a bar that takes width would
  re-wrap the run's text every time it appeared. The consequence for this
  package: `offsetWidth - clientWidth === 0`, so `intent.ts`'s geometric
  scrollbar-drag test can never fire for the clip, and the overlay thumb
  states its intent instead (`pointerdown` → escape, release at the bottom →
  `markAtBottom()`). Event-sourced, per C6.

## Row And Payload State

Virtualized rows can remount at any time. User-visible row state that must
survive remount lives on `ThreadPane` registries keyed by item id,
payload id, or subagent group key. Loaded payload bytes live in the
byte-bounded module cache in `payloadDataCache.ts`.

Measured row heights ARE replayed across thread switches, and across an
app restart, under a per-row validity model (`utils/virtual/priors.ts`).
The `{#key pane.threadId}` block remounts the `<TimelineVirtualizer>` on
every switch, so without a replay it re-runs the full estimate→measure
cascade. That is the thread-switch flicker, identical for cached and
uncached threads because the item cache avoids the *fetch*, not the
*remeasure*.
The priors replay makes the mount start at the already-measured total:
the engine's `RowEstimate` resolves each unmeasured row from the previous
visit's persisted measurement (falling back per-row to the kind-height
table, then the flat default), and the per-row ResizeObserver's first
delivery matches the estimated size, so `applyMeasurements` no-ops it.
Zero re-render, zero scroll jump.

Priors are keyed by a **per-row content signature** (`nodeSignature` in
`utils/timelineStructureSignature.ts`: id, status, `summary.length`,
`updatedAt` for a leaf; key + member count for a group), not by
position. Each thread's `SizePriorsEntry` is `{ width, expansionSig,
rows: Map<signature, measuredPx> }`: **width** (the wrap point) and
**expansionSig** (`pane.expansionSignature()`, non-default subagent/diff/
payload expansion) gate the whole entry. A mismatch on either refuses
every row in it, degrading the mount to the kind/flat estimate chain,
same as a cold first visit. The per-row signature gates each row
independently within a valid entry: a row whose content changed simply
misses on its own map key, without invalidating its still-valid
siblings. This replaced an earlier design that keyed ONE positional
`sizes: number[]` snapshot against a **whole-window** structure
signature (the newline-join of every loaded row's signature), a key
that a fresh app boot's small initial window essentially never matched
against a full session window of hundreds of streamed/paged rows, so
restart replay was effectively dead. The per-row map fixes that: a boot
window's rows are a *suffix* of a larger captured window, and each one
resolves independently. A handful of global display settings (`fontSize`,
the sans/mono fonts, `collapseDiffPreviews`) also change row height but
are deliberately **not** keyed, a documented, benign residual: toggling
one mid-session then revisiting a thread replays stale heights, which the
warm-up gate masks as a cold first visit (the estimate→measure cascade
re-runs and corrects them), never a crash or stuck viewport. Keying them
would make the residual airtight but buys no visible change (same masked
cascade either way) at the cost of a drift-prone signature; the choice is
recorded in `priors.ts`. Row-UI state is reset to default on every switch
(`rowUiState.clear()`), so at restore time the expansion signature is the
default one, which is exactly why a thread that was idle-at-default
replays cleanly and a thread that had something expanded (taller rows) is
correctly refused.

The width/expansion validity check is deliberately **lazy-once**, not
eager: `resolveRowEstimateOnThreadEdge` (`timelineSizePriors.svelte.ts`)
runs in `$effect.pre` before the virtualizer remounts, and on a fresh app
boot the scroll surface has not been laid out yet. An eager check would
read width 0 and spuriously refuse every restart replay. The check
instead runs on the first `RowEstimate.at()` call and is memoized for
the rest of that mount. Even at first use the width can still
legitimately read 0: the width signal is RO-only (async by the
one-source rule in `scrollSurfaceWidth.ts`), while the engine's first
`at()` calls run synchronously when the virtualizer mounts with data.
On boot, whichever lands first is a machine-speed race. Width 0
therefore means "layout hasn't reported yet", not a real wrap point,
and the check **trusts the entry's captured width** in that case
(latched, so every row in the mount resolves consistently): window
geometry restores across restarts, so the real width almost always
matches, and a genuine mismatch degrades to the documented
self-correcting display-settings residual instead of a guaranteed full
cascade.

`setThreadSizePriors` REPLACES a thread's entry wholesale on every
capture rather than merging row-by-row: streaming rows carry
`updatedAt`/`summary.length` in their signature, so a row's key changes
on every append, and merging would accumulate dead signatures forever.
A wholesale replace self-cleans that for free.

Captures must store the **settled** sizes or the replay restores a
mid-cascade height. They ride two triggers, both routed through one
size-gated persist (gated on `getScrollSize()` so a 60Hz spring does not
re-slice the array): the scroll-position snapshot (`saveScrollSnapshot`),
and the rising edge of `stick.isWarm`, the controller's
"measurement-cascade-settled" signal, which guarantees a final-height
capture for a thread the user views but never scrolls.

The in-memory store is a bounded LRU (memory: ~one float per loaded row
per recent thread), a WORKING SET over a persistent backing store, not
the store itself. `utils/virtual/priorsStorage.ts` is a
localStorage-backed adapter (installed at module scope of
`timelineSizePriors.svelte.ts`, so it is active before any pane mounts)
that makes priors survive an app restart: writes are debounced (~1s,
coalesced per thread) and flushed early on `pagehide`/hidden
`visibilitychange`; reads lazily hydrate a memory miss. A 50-thread
storage cap and 50-thread in-memory cap both evict LRU-oldest. A memory
eviction never touches the persistent store (the thread's priors are
still on disk and rehydrate on its next visit); only an explicit
`clearThreadSizePriors` (thread deletion, item mutation, same-thread
reswitch, via `threadSwitchLoad.svelte.ts`'s `dropCachedWindow` and
`threads.svelte.ts removeThread`) deletes
from storage too. Storage failures (quota, corrupt JSON from a stale
schema version or a hand-edited profile) warn once and degrade to
"priors don't persist this session" rather than throwing. None of this
violates the visible-thread memory budget or touches SQLite. It is
still a session-scoped cache in memory, just one that can rehydrate
itself from disk. Kind estimates for priors-miss rows are **floors, not
averages** (`ROW_KIND_ESTIMATE_PX` in
`components/chat/timelineSizePriors.svelte.ts`): an estimate above a
row's real height shrinks `totalSize` on first measurement (a scrollbar
dip plus a synchronous browser `scrollTop` clamp at exact bottom), while
an undershoot only grows `totalSize`, absorbed invisibly by
remeasure-above compensation at the cost of a few extra transiently
mounted rows on a cold switch.

There is deliberately NO per-row min-height floor system. An earlier
per-pane row-geometry reservation (row-key + signature + width height cache
applied as a temporary `min-height` on remount) was deleted post-`f42dc6e6`:
a capture experiment on the scroll-away/return remount path (run in the
virtua era, with its manual-scroll marking patch and fractional height
caching in place) showed floors-OFF outcome-identical to floors-ON (zero
scrollHeight dips, zero scrollTop reversals, identical unmount batches,
clean bottom landings). See
[`scroll-rearchitecture-plan.md`](scroll-rearchitecture-plan.md) §3.
Async-short remount content is bridged at the content layer (streamdown
mermaid/math rendered-height caches, the attachment blob cache), and
`remountReturn.browser.test.ts` pins the outcomes. Two pieces survive the
deletion:

- The scroll surface width signal: the **content-box** width, observed
  asynchronously through `observeScrollSurfaceContentWidth`
  (`scrollSurfaceWidth.ts`), feeds the priors validity key, and is
  never a synchronous or border-box read (`getBoundingClientRect`,
  `clientWidth`). A second, disagreeing width source turns the width signal
  into a self-sustaining oscillation that re-renders every row at idle
  (CPU/heap-churn incident 2026-06-26, commit `a5a5d032`).
- Margin containment: the `[data-row-geometry-content]` row wrapper and the
  app.css `display: flow-root` rule (commit `4b3759a1`), independent of the
  floors; see [`settle-flicker-analysis.md`](settle-flicker-analysis.md).
  The virtualizer's per-row ResizeObserver measures content-box height, so
  the contract is what keeps measured height and visual extent in
  agreement.

Rows inside the transcript should keep their shell stable after first
render. Add details inside reserved slots; do not append completion-only
history rows or late adornments that change row geometry.

## Search

Full-thread search goes through the `MessageSearch` palette and the
`SearchThreadMessages` binding. Browser find only sees mounted virtual
rows and is not a complete search surface.

A search hit calls `pane.requestScrollToItem(itemId)`. `MessageTimeline`
loads older rows until the item is present, then scrolls through
`listRef.scrollToIndex(index, { align: 'center' })`, an escaped,
controller-routed write (the virtualizer performs it through
`applyScrollTarget`).

A hit inside a subagent transcript never appears in a history window
(windows hold top-level rows only). `loadUntilItem` walks the parent
chain to the launch root, slices the window around that root, hydrates
the subtree via `ListSubagentDescendants`, and the scroll resolves to
the containing `SubagentGroup` card.

## Discussion Mode

`ChannelView.svelte` shares the scroll controller (`utils/scroll/`)
without a virtualizer. Its
content element wraps the channel-message list so the same ResizeObserver
path handles message growth and async Streamdown layout. Composer-section
resize must also notify the controller because discussion's composer is a
flex sibling that changes `scrollEl.clientHeight`.

The channel is push-driven, not polled: `discussion:message` and
`discussion:state` events land on `threadChannelState.svelte.ts` (the
pane's discussion data layer, surfaced through `ThreadPane`'s
`channel*` getters), and `ChannelView` renders whatever that state holds.
Initial load and transport-gap recovery share one resync helper
(`eventsDiscussion.ts`'s `refreshDiscussionChannel`) that fetches with
`afterSeq = -1`, the exclusive "everything" cursor, since message
sequences are zero-based and a `0` cursor would silently drop the
channel's first message.

The current speaker's in-flight text streams like chat: a discussion
child thread (one participant's own session) has no mounted pane, so
`eventsItemStream.ts` feeds its `assistant_text` upserts/deltas through a
side-channel registry (`discussionLiveTail.ts`) into the channel state's
`liveTail`, keyed by the roster `discussion:state` last published.
`ChannelView` renders that tail as a streaming card and lets it fall
away once the matching agent message lands.

Growth animates exactly as it does in chat: unconditionally, subject
only to the signal-based spring gate (see Live Content Animation above).
The channel keeps its own liveness stamp for the sentinel and viewport
paths: the channel state stamps `lastLiveContentAt` on every
genuinely-new message and on live-tail growth, and `ChannelView` calls
`isLiveContentActive(now, pane.channelLastLiveContentAt,
LIVE_CONTENT_ACTIVE_HOLD_MS)` for the controller's `liveContentActive`
option. This is the channel's own stamp, independent of the chat
timeline's `lastLiveContentAt`. The two
surfaces never mount at once, but each pane tracks both so switching a
pane between chat and discussion mode never reads a stale latch.

## Diagnostics

Scroll bugs are usually second-order interactions between controller
flags, row measurement, and browser layout. Reproduce with
`make dev DEBUG=1`, then inspect `window.__agentOverflowUiTrace.dump()` or
`.filePath()`.

The trace surface has two tiers (Makefile: `UI_TRACE`, `UI_ORACLES`;
`DEBUG=1` enables both). `UI_TRACE=1` alone is the light tier (event
traces plus the spring chase telemetry below), cheap enough to measure
production-representative frame cadence (`make build-wsl UI_TRACE=1`
builds the minified bundle with only this tier). `UI_ORACLES=1` adds the
standing regression oracles (`timeline.row.resize`,
`timeline.margin.diverge`, `timeline.reasoning.tailJump`, each an extra
per-row ResizeObserver plus a subtree MutationObserver) and the
throttled DOM snapshot walks (`timeline.dom` / `chat.dom` /
`plan-sidebar.dom`), which are the expensive part of the surface during
streaming.

Useful trace records:

- `scroll.spring.chase` is one summary per spring chase (emitted at
  cancel; chases under 3 ticks are skipped unless they paused for a
  selection): tick counts (write / sentinel / `selectionPausedTicks`,
  the frames that re-armed without moving because a selection drag
  crossed the element), a frame-gap histogram (`gapBuckets`, bounds
  `[<9, 9–13, 13–18, 18–26, 26–42, >42]` ms, per
  `CHASE_GAP_BUCKET_BOUNDS_MS`), `maxGapMs`, catch-up clamp count
  (`catchupClamps`), chase-distance snaps (`distanceJumps`, whose field
  name predates the rename; it now counts the `spring.catchupSnap`
  write that, after an observed rAF discontinuity, snaps fully to the
  target instead of animating any of the frozen-tab
  backlog), target changes, sentinel entries
  (stop/restart cycles), long-task count/duration during the chase
  (Chromium `longtask` observer; absent under WebKit), and
  `refusedWrites` (write attempts the element swallowed outright, a
  subset of `writeTicks`, where nonzero means the write-refusal guard
  below was in play). This, not the 1-in-12 sampled `spring.tick`
  spacing, is how to judge whether a chase actually dropped frames; see the
  telemetry footgun note in
  [`settle-flicker-analysis.md`](settle-flicker-analysis.md).
- `scroll.writeRefusal` records that the spring's write-refusal guard
  latched, healed, or was abandoned (`phase`, where 'abandoned' means the
  chase was cancelled while still latched, so no heal was ever observed), with
  element diagnostics (computed `overflowY` / `scrollBehavior` /
  `display` / `position`, `connected`, `surface`, geometry) and the
  wedge's shape (`consecutiveRefusals`, `wedgeMs`). Background
  (bug-report-20260818T003129Z): an ActivityRun clip spent 227s as a
  non-scroll-container (real geometry, every scrollTop write read back
  0) while its content streamed; the spring's simulated position
  coasted to the target, so it busy-wrote the FULL target at display
  rate for 37k ticks and the first accepted write after the element
  healed teleported the clip 940px. The guard (spring.ts, "Write-refusal
  guard") classifies every write three ways from a same-tick
  write+readback: MOVED (heals a latch), REFUSED (no motion on a ≥1.5px
  request, which re-anchors the model to the element's true position, so a
  heal can only be a bounded glide, never a teleport), and INCONCLUSIVE
  (no motion, sub-threshold, evidence of nothing; deliberately does
  NOT heal, so a still-wedged sliver can't silently unlatch). Five
  consecutive refusals latch the whole tick body, forced-layout reads
  included, to ~4Hz samples with a parked-style velocity decay. The
  guard covers spring writes only, deliberately: one-shot placement
  writers fail once and bounded during a wedge, and any sustained wedge
  during bottom-follow reaches the spring, the only writer that can
  busy-loop or teleport. Transitions land here plus a `frontend-errors`
  diagnostic that persists in production, rate-limited to the first
  latch per 10s window and its matching bookend (per controller). The
  trigger for the wedge itself sits below the app (renderer state;
  nothing in the codebase mutates overflow). If it recurs, this
  record's diagnostics are the root-cause capture the original incident
  lacked.
- `scroll.contentRO`: content-geometry delta, width-reflow state, and pin
  decisions (in chat the delivery is engine-sourced, not an RO fire; the
  record name stays for trace continuity, and `settleEvidence` reports
  the warm gate's per-row settle input).
- `scroll.contentRO.widthReflow`: width-only content reflow that armed
  the short layout-correction window.
- `scroll.escape.set`: escape state changes.
- `scroll.spring.selectionPause`: one record per pause session, on the
  first spring tick that re-armed without writing because a selection
  drag crossed the element. The chase summary carries the count; this
  marks WHEN. `springActive:true` with no `spring.tick` writes and no
  such record is a different stall, not a selection.
- `scroll.refreshIsNearBottom`: geometric near-bottom changes.
- `chat.state` / `chat.dom`: MessageTimeline snapshots.
- `timeline.margin.diverge`: settle-flicker regression oracle. Fires when a
  row's bottom margin escapes its content box (the measured row total counts
  it; the row's content-box observer does not), which used to drive a
  `contentRO` transient and an `oscillationSnap`. Must stay silent; see
  [`settle-flicker-analysis.md`](settle-flicker-analysis.md) for the root cause
  and the `[data-row-geometry-content] { display: flow-root }` containment fix.
- `timeline.coldload`: one record per pane per thread-switch cold load
  (`utils/coldLoadTrace.ts`), consolidating the switch-to-warm timeline
  into segments instead of leaving them scattered across the
  `scroll.warmup.*` / `chat.state` records above. Fields: `source`
  (`'cache-restore'` or `'fetch'`), `fetchMs` (switch start → initial
  slice applied; `null` on a cache restore), `itemCount` (rows the pane
  holds after that slice merged, meaning what actually mounts, not the wire
  count), `settleMs` (slice applied → the warm gate's rising edge; on a
  cache restore, switch start → that edge), `totalMs` (switch start →
  warm), `warmReason` (the controller's `warmReason` at that edge:
  `'quiet'`, `'failsafe'`, `'settled'`, or `'skip'`), and `priors` (the
  size-priors replay summary stamped by MessageTimeline's warm-edge
  effect: `{source, validity, rowsResolved}` per
  `SizePriorsReplayStats` in `timelineSizePriors.svelte.ts`, or `null`
  when no timeline stamped one). A large `fetchMs` points at the
  backend/IPC leg; a large `settleMs` with `warmReason: 'failsafe'`
  points at a measurement cascade that never went quiet; `priors`
  distinguishes "no stored entry" from "entry refused (width/expansion
  mismatch)" from "replayed N rows".

  Three fields describe the gate's own cold-load behavior (see
  **Warm-Up And Restore**). `warmBeforeItems` counts warm rising edges
  that landed BEFORE the slice. On a fetch these measure the empty
  pane, so the session survives them rather than closing on one, which
  is what makes `fetchMs`/`itemCount` non-null on a real cold open.
  `warmupRearmed` reports whether applying the slice re-closed the gate;
  a fetch record with `itemCount > 0` and `warmupRearmed: false` is this
  defense regressing. `abandoned` is `null` on a normal close and names
  why otherwise: `'switched-away'` (a new switch replaced a session
  still in flight) or `'thread-changed'` (the pane's warm edge arrived
  for a different thread). Every session emits on exactly one of these
  paths, so a switch that produced no record at all is itself a signal.
  Needs a `make dev DEBUG=1` build (`VITE_AGENT_OVERFLOW_UI_TRACE=1`).
- `frame.loaf`: one record per long animation frame (>50ms, the spec's
  fixed threshold), session-wide (`utils/loafTrace.ts`, light-tier: the
  browser only delivers entries for frames that exceeded the
  threshold, so there is no steady-state cost). Whole-frame duration,
  `blockingMs`, rendering-phase timestamps, and top-3 script
  attribution. This covers what the chase telemetry can't: slow frames
  outside any chase, and frames whose scripts were fine but whose
  style/layout phase blew the budget (invisible to `longtask`). One
  `frame.loaf.install` record per session states whether the observer
  is live, so a capture with the install record and no `frame.loaf`
  records is positive evidence no frame exceeded 50ms. For a visible
  jump, that plus clean chase cadence points post-commit
  (WebView2/DWM presentation), not at the renderer.

Work backward from the visible symptom to the last relevant
`scroll.contentRO`. The record carries the resolver's decision
(`writeCaller`, `startSpring`, `bumpTargetChanged`, `oscillationRecovery`,
`setIsAtBottom`). If the user intended to stick and a negative delta shows
`writeCaller: null` with `setIsAtBottom: false`, check whether the
resolver's pin predicate should use logical intent (`isAtBottomState`) as
well as geometry (`isNearBottomState`).

Do not fix scroll regressions by adding `requestAnimationFrame`, a second
observer, a length-watching `$effect`, or another `scrollTop` writer.
Encode the failing ResizeObserver/geometry sequence in
`scroll/index.svelte.test.ts` or `scroll.test.ts`, then fix the
controller path. If the regression is two mechanisms interacting (a
write landing inside another program's trip), add the missing op or
starting state to `scroll/scrollInterleavings.test.ts` instead. Its
ops × states product holds the frame invariants (bounded motion, no
counter-chase movement, escaped viewports never move, quiet
convergence) across every combination, so the whole defect class stays
covered, not just the reported instance.

## Accepted Tradeoffs

Nested row overflow is allowed for large subagent, wait-group, and command
output bodies. Focus can jump to `<body>` when the windowing unmounts the
focused row. Syntax highlighting is backend span metadata
(`internal/highlight`); the frontend renders spans over text it already
holds.

## Open Defects

- **Run-to-prose transition jumps about half the spring instead of
  gliding**, on some turns, when something is still animating inside the
  activity run. Leading suspect: `ActivityRun.svelte`'s controller teardown
  handover, which writes `clip.scrollTop = clip.scrollHeight` (an instant
  snap) when tail-ness ends with the inner glide mid-flight; the comment in
  the file names the case. Mid-glide APPEND bursts are proven clean
  (`activityRunBurstMotion.browser.test.ts`), so the suspect space is the
  teardown only. Sampler: `scripts/perfprobe/jumpwatch.mjs` samples pane
  scrollers and run clips per frame and flags scrollTop steps beside clip
  teardown frames; a first capture saw 44 teardowns all with zero bottom
  gap, so the mid-flight case needs a longer watch synced to turn ends.
  Next: red browser test for the mid-flight handover, then a fix that lets
  the glide finish or transfers the remaining distance (never a snap).
