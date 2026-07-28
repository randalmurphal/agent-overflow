# Frontend Scroll Architecture

This is the durable contract for chat and discussion scrolling. It keeps
the operational rules out of `AGENTS.md` while preserving the invariants
that matter when changing `ThreadPane`, `MessageTimeline`, `ChannelView`,
the timeline virtualizer, or the scroll controller (`utils/scroll/`).

## Owners

- `MessageTimeline.svelte` owns the outer chat scroll container.
- `components/virtual/TimelineVirtualizer.svelte` + `utils/virtual/` own
  virtual row geometry. The split inside:
  - `utils/virtual/` — the bespoke windowing engine, pure data + math
    with no DOM and no Svelte: `sizes.ts` (the size store: measured px
    or estimate per row, memoized offsets), `window.ts` (visible-range
    math), `engine.ts` (the reducer: scroll/resize/measurement/length
    inputs in → mount window + totalSize + at most one compensation
    observation out), `priors.ts` (per-thread, per-row measured-size
    persistence and the estimate resolver — DOM-free; see its header for
    the per-row signature model and the storage-adapter seam),
    `priorsStorage.ts` (the localStorage-backed adapter that makes
    priors survive an app restart, not just a same-session thread
    switch), `types.ts` (the shared shapes). Design doc:
    [`virtualizer-replacement-plan.md`](virtualizer-replacement-plan.md).
  - `TimelineVirtualizer.svelte` — the adapter binding the engine to the
    DOM: one lazy ResizeObserver for the scroller and every mounted row,
    scroll-event feed, spacer + absolute row positioning (`VirtualRow`),
    scrollend synthesis, and the imperative handle
    (`TimelineVirtualizerHandle` in `utils/virtual/types.ts`). It is also
    the controller's **content-geometry source** in chat: the spacer
    height it writes IS the content height, so it delivers
    `ContentGeometrySample`s (`onContentGeometry` →
    `stick.deliverContentGeometry`) post-flush instead of the controller
    re-observing the same element with a second ResizeObserver — same
    observation shape, one frame earlier, no duplicate layout read on
    the streaming hot path.
  - **The engine never writes `scrollTop`.** Geometry changes that would
    move content above the viewport surface as `EngineCompensation`
    observations; imperative scrolls (`scrollToIndex`) compute their
    target in the engine and perform the write through the controller
    chokepoint (the `applyScrollTarget` prop). There is no second
    scrollTop writer to guard against by construction.
- `utils/scroll/` owns user scroll intent and **every** programmatic
  `scrollTop` write. Inside the package:
  - `resolver.ts` — the pure per-delivery reducer. Every contentRO
    delivery and every engine compensation observation becomes an
    observation; **the resolver's decision is the only authority on
    what, if anything, gets written**. Adding a scroll behavior means
    adding a decision branch here, not a write site somewhere else.
  - `index.svelte.ts` — the controller: the reactive flags templates
    subscribe to, geometry reads, the single `writeScrollTop` chokepoint
    every programmatic write routes through, wiring for the three
    machines below, and the public API. `types.ts` holds the consumer
    contract; consumers import from `utils/scroll/index.svelte`.
    The chokepoint also owns the **fractional glide residue**: spring
    writes are fractional, the engine rounds `scrollTop` to whole CSS
    pixels, and the sub-pixel remainder is composited as a `translateY`
    on `contentEl` (which carries a permanent `will-change-transform`
    for it) so slow spring tails render continuously instead of as
    1px steps. Two display-physics guards ride along: an epsilon
    `rotate(0.0001deg)` defeats WebKit's compositor pixel alignment
    (which would round the sub-pixel translate to whole device pixels
    and oscillate around the trajectory), and the spring holds a
    refresh-aware "fusion floor" while decelerating: derived from
    devicePixelRatio plus the spring's measured rAF cadence so the
    glide advances 1/k device pixels per displayed frame
    (k = ⌊refresh/60⌋). Every harmonic of the bilinear-resample
    breathing on thin rows then either phase-locks (constant resample
    weights — invisible) or patterns at ≥60Hz, above flicker fusion;
    sub-120Hz displays get the full one-pixel-per-frame lock (zero
    breathing). A refresh-blind floor aliased into a visible ~12Hz
    beat on 144Hz panels — see `fusionFloorPxPerFrame` in spring.ts
    for the derivation. The residue is a render detail, not a second scroll
    writer — it never changes `scrollTop` and fires no scroll events.
    Release rules: a clear that accompanies a real write (any
    non-spring caller, or detach) is instant; a release with no write
    (spring catch-up, selection pause, sentinel entry, cancel) is
    EASED to zero over ~6 frames — snapping the parked ~0.5px residue
    once per quantum read as a faint vibration during bursty output.
    Either way text at rest ends crisp at translate 0.
  - `intent.ts` — the event-sourced intent machine: wheel/scroll/pointer/
    key/touch listeners, escape and re-stick, restore-snap consent, and
    programmatic-write tagging. Intent is never geometry-inferred.
  - `spring.ts` — chase kinematics: HOW a spring advances scrollTop
    frame to frame once the controller decides one runs. Also owns the
    chase-distance clamp: after an observed rAF discontinuity (tick gap
    ≥1s, or document visibility resumed ≤2s ago) a chase more than one
    viewport behind jump-enters the glide (`spring.catchupJump` write)
    so exactly one viewport animates — distance alone never clamps.
  - `observers.ts` — the content-geometry delivery pipeline, the warm-up
    (quiescence) gate, and resize classification. Two sources feed the
    one pipeline: engine-sourced samples in chat
    (`externalContentGeometry`), a contentEl ResizeObserver everywhere
    else (ChannelView). Each delivery is gathered here, decided by the
    resolver, and applied through the controller's chokepoint, so "a
    content delivery" reads in one place.
- The MessageTimeline scroll-session modules (extracted siblings in
  `components/chat/`; MessageTimeline keeps the thin `$effect` bodies
  that call into them):
  - `timelineRestore.svelte.ts` — thread-switch restore sessions:
    switch-edge bookkeeping, scroll-snapshot save/restore, and the
    scroll-to-item flow.
  - `timelineSizePriors.svelte.ts` — per-thread, per-row size priors,
    including the `ROW_KIND_ESTIMATE_PX` floor-biased kind estimates,
    the priors capture/persist cadence, and the lazy-once
    width/expansion validity check. Installs the localStorage
    persistence adapter (`utils/virtual/priorsStorage.ts`) at module
    scope.
  - `timelinePaging.ts` — load-older/load-newer gates and handlers.
  - `timelineWindowAnchor.svelte.ts` — prune-shift anchoring when the
    live window drops rows off the top.
  - `timelineRowProjection.svelte.ts` — the node-derivation pipeline:
    structural grouping (subagent/wait/read groups), the reveal gate
    (`revealedNodes`), rail classification, and response-pill duration.
  - `timelineDiagnostics.ts` — render/state tracing and the dev-only
    memory-stats, pane-geometry, row-resize, margin-divergence, and
    reasoning-tail-jump probes.
  - `timelineRowUiPrune.ts` — bounds per-row expansion-handle retention
    to a buffer around the visible range plus the tail, on a prune
    cadence (structural changes + scroll end).
- `components/chat/ActivityRun.svelte` owns the one nested scroller that runs
  the same physics as the pane: a height-capped clip over a stretch of
  activity rows, with a second controller instance on the live run only.
  Geometry and window math live in `utils/activityRun{Clip,Window}.ts`,
  per-run state in `stores/threadActivityRuns.svelte.ts`. Full architecture:
  [`activity-runs.md`](activity-runs.md).
- `ThreadPane` owns the scroll-controller registration slot so shared
  surfaces can pause or notify scrolling without reaching into component
  internals. It is **single-occupancy and belongs to the surface that owns
  the pane's scroll container** — `MessageTimeline`, or `ChannelView` on a
  discussion pane, whichever is mounted. A controller *nested inside* one
  of those (the activity run's) never registers.

  The slot is `$state.raw`, and that is load-bearing rather than a
  micro-optimization: `detachScrollController` decides whether a teardown
  is stale by comparing the incoming controller against the registered one,
  and a plain `$state` proxies the object on assignment, so the comparison
  is false even for the same controller. It never cleared — a torn-down
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
(`createAutoLoadGate` in `timelineScroll.ts`). It is gesture-armed —
`disarm()` after every load, re-armed only by a real wheel/touchmove/
keydown (the post-load `shift` compensation is a programmatic scroll and
must not re-arm) plus a 350ms cooldown fallback — and its progress guard
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
instead of leaving them position-keyed. The remap — not an
invalidation — matters because a moved row keeps its DOM size, so no
ResizeObserver delivery follows the move; a stale position-keyed entry
would never self-correct and rows below the move point would render at
wrong offsets (overlap) until an unrelated resize. The reorder's
compensation is anchor-based: the row under the viewport top (or the
nearest surviving row after it, when a mid-list splice removed it) is
held stationary — exact for length-changing keyed splices such as the
review pane's collapse/expand, not just same-length reorders.

## Load Paging (head-splice `shift`)

`loadOlder` and `loadNewer` mutate the window at one end and prune the
other, and they drive the virtualizer's `shift` head-splice so the reading
position holds without an explicit re-anchor. `shift` tells the engine
which end a length change hit: on a **head** change `applyLength` splices
the size store at the head (`spliceHead`) and reports a `head-splice`
compensation whose target keeps the viewport stationary — applied in the
same flush; on a **tail** change it does neither. Priors need no remap
step across the splice: they resolve per-row against a content
signature (`utils/virtual/priors.ts`), not a position, so there is no
index-keyed prior state left to shift. Without the `shift` hint itself, every change
is treated as tail growth/shrink and a prepend misindexes the whole size
store — every measured height lands on the wrong row, forcing a
re-measure of everything visible: the "viewport shifts, scrollbar jumps
around" load jank.

The store exposes the paging direction as `pane.pendingTimelineShiftAtHead`,
bound into `<TimelineVirtualizer shift={...}>`, set synchronously at the
`items` mutation so the virtualizer's length `$effect.pre` reads it in the
same flush, and reset in the paging method's `finally`. The grow
(prepend/append) and the prune are deliberately split across **two
flushes** (`await tick()` between them): coalesced, a head-grow plus a
tail-shrink collapse into one net length change that a single `shift`
boolean cannot describe — and when the page budget equals the prune count
the net length is unchanged, so the virtualizer sees no length change at
all and the size store scrambles. Head-splice semantics are covered in
`utils/virtual/engine.test.ts` and `sizes.test.ts`; the store's two-flush
sequencing + shift direction are covered in `thread.svelte.test.ts`.

`loadOlder` / `loadNewer` apply the paired prune directly (the dropped end is
always opposite the reading viewport, so there is nothing to veto or restore).
The streaming / settle prune keeps the explicit anchor transaction
(`preserveTimelineWindowAnchor`, below) because it can fire under a
bottom-pinned viewport where the incident-hardened defer-and-restore behavior
matters. That path does not ask the pane's raw item window whether the prune
is a head-drop; `<TimelineVirtualizer>` receives filtered/grouped
`revealedNodes`. `MessageTimeline` compares the rendered `timelineNodeKey`
list before and after the prune, and marks a local one-flush `shift` only
when the rendered nodes are a strict suffix. That prevents a prune through a
Read group, notification filter, subagent group, or reveal boundary from
splicing the size store against the wrong row set.

## Live Window Bounds

The streaming append path caps the loaded window
(`ACTIVE_TIMELINE_WINDOW_MAX_ITEMS`, pruning back to
`ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS`), but the prune **defers to turn
settle** while a turn is active. A mid-stream head-drop collapses content
height under a bottom-pinned viewport, the browser clamps `scrollTop`,
and the window re-measures — a visible blank flash (incident 2026-06-10).
`ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS` is the memory backstop: a
single turn streaming past it gets pruned mid-turn anyway.

The streaming / settle window prune goes through `MessageTimeline` when a
timeline is mounted (the paging prunes use `shift` instead — see **Load
Paging**). The pane owns the window decision, but the timeline owns the
DOM/virtualizer anchor transaction: bottom intent pins to the new bottom,
and reading state preserves the first visible item when that item survives
the prune. If a normal recent-window prune would drop the visible anchor,
the pane defers it and retries when bottom intent returns instead of
re-asking on every append. The hard ceiling is the only exception; it
forces the prune even when anchor preservation vetoes the operation, and
it is independent of provider turn state.

Subagent child rows get a tighter bound than the window cap. Streaming
children must live in `pane.items` (the delta pipeline applies only to
loaded rows), but once a child settles and nothing can render it —
collapsed inline card, backgrounded launch, Codex spawn — the pane evicts
the row and folds its count/preview into a per-anchor registry
(`utils/subagentFold.ts`). Collapsing an expanded card evicts its settled
subtree the same way. Card expansion re-hydrates from SQLite via
`ListSubagentDescendants` and reclaims the folded ids — an id is folded
XOR loaded, never both. Folds ride the thread-switch snapshot cache with
the window they describe, and a folded id arriving again over the wire
(reconnect replay) is dropped, not re-inserted. Net effect: per-pane
subagent memory is O(active children), not O(transcript), and the window
cap counts only renderable rows.

## Run Height Changes

A deliberate change to a run's height goes through
`PaneScrollController.preserveViewportBottom` (`timelineWindowAnchor.svelte.ts`,
the same module as the prune transaction, reached from components via
`withViewportBottomHeld`). Today that is every activity-run collapse or
expand: the ones the reader asked for (a toggle, the header's collapse-all)
and the auto-collapse gate's batched off-screen releases. The releases are
not reader-requested — the gate fires precisely because the reader is
provably elsewhere — but they ride the same transaction: the runs they
change are out of sight by construction, and the anchor restore is what
makes that a guarantee for a mid-list reader rather than a property of
engine estimate compensation. One caveat follows from being unasked: the
gate defers while `autoScrollInFlight()` reports a glide running or armed,
because the transaction's bottom-pinned restore is a direct write and would
snap an animation the reader is watching; explicit clicks keep their
instant behavior — for them the snap IS the intent. The transaction shares the prune's shape —
capture intent, pause the spring, run the change, restore after the flush —
and differs in which edge it holds:

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
whole delta — for a collapse-all, most of the conversation, animated. Under the
transaction the sticky branch instantly re-pins the new bottom, so there is
nothing left to animate.

Both restores rely on `scrollToIndex` converging: the target is recomputed as
measurements land, so one `tick()` is enough to schedule a restore whose rows
have not been measured yet.

**Bounded by the scrollback that exists.** Opening upward spends `scrollTop`,
and a run near the top of a thread may not have enough — the write clamps at 0
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

A consumer that knows its async typesetting (svelte-streamdown's
shiki/katex/mermaid) has settled can pass `quietContextSignal` to shorten
the quiet window to `SETTLED_QUIET_MS` (~one frame). That shortcut is
itself gated on **geometry stability**: `quietContextSignal` is blind to
the engine's estimate→measure cascade, which grows `scrollHeight` over a
series of contentRO fires spaced wider than `SETTLED_QUIET_MS`. Revealing
on the short window mid-cascade shows the surface land right, flicker as
the cascade finishes, then land right again — the idle-thread flicker. So
the controller only takes the short window when the latest contentRO
height delta is `≤ WARMUP_SETTLE_EPSILON_PX`; a larger delta (or the
first fire, which has no baseline) holds the conservative `QUIET_MS`
window, which each cascade fire resets so it closes only once the cascade
goes quiet. A height delta of exactly `0` (a width-only / padding-var
reflow) carries no new height information, so the gate keeps the prior
magnitude rather than reading it as "settled" — otherwise a reflow firing
in the gap between two cascade steps would trip the short window early
(the cold-boot residual, where steps are far apart and font/layout
reflows fire in the gaps). Reveal tracks `scrollHeight` stability, never a
guess at how long the cascade takes. This lengthens the hidden window for
genuinely cascading threads (the ones that would have flickered) by the
minimum needed; `FAILSAFE_MS` still caps the worst case.

The signal MessageTimeline supplies is composed:
`anyMarkdownSettledSinceArm || mountedMarkdownCount === 0`. The second
term is **settled-by-absence** — a mounted window containing zero
`ChatMarkdown` rows (all tool output / terminals / images) has no async
typesetting coming, so it must not sit behind the conservative window
until the failsafe. Presence is a live count registered through
`CHAT_MARKDOWN_PRESENCE_CONTEXT`, which makes the signal *withdrawable*:
a markdown row mounting after the quiet timer armed flips the signal
back to falsy, and `notifyQuietContextSignalChanged` then DISARMS the
armed timer — the settled-by-absence license is gone, and only an earned
settle (or the failsafe) may open the gate.

The geometry gate only ever masks the cascade — it cannot prevent it,
because the cascade settles in bursts spaced wider than any safe quiet
window (trace `bug-report-20260622T225817Z`: a final +200–500px burst
landed ~160ms after reveal, longer than even `QUIET_MS`). For a
**revisited** thread the cascade is instead *eliminated*: the engine's
row estimate resolves each unmeasured row from the previous visit's
persisted measurement (the priors replay — see "Row And Payload State"
below), so the surface mounts at its final height. Chat's engine-sourced
geometry samples carry the per-row settle evidence that proves it —
every mounted row measured, all within `WARMUP_SETTLE_EPSILON_PX` of
their estimates — and the warm gate then reveals **immediately**
(`markWarm('settled')`), skipping the quiet wait entirely, once the
markdown-settled signal also confirms no late typesetting wave is
coming. A cold mount's corrections are tens-to-hundreds of px, so it can
never fast-path; late async growth (mermaid, images) lands as a
correction and holds the gate exactly as before. Priors are per-row and
persist past a restart (`utils/virtual/priorsStorage.ts`), so the
"revisited" case above now also covers a thread reopened after a fresh
app boot, not just a same-session thread switch — the boot window only
needs to be a suffix of a previously-captured, possibly much larger,
window (see priors.ts's header for why the per-row key makes that
robust to window-composition changes). The geometry gate therefore
guards only a **genuine first-ever** visit to a thread, or rows whose
content changed since their last capture (a per-row signature miss) —
where no valid prior exists and the estimate→measure cascade is
unavoidable — and is best-effort there. Both defenses coexist: priors
remove the cascade when they can, the gate hides it when they can't.

On thread switch, `MessageTimeline` must call `stick.armWarmup()` from
`$effect.pre` so `isWarm=false` before the new DOM paints. The restore
effect then calls `stick.forceStick({ reason: 'restore' })` and schedules
one rAF `observe('content')` settle pass for late composer padding, row
measurement, or Streamdown layout changes. The rAF pass is escape-aware.

Same-thread reloads must watch `pane.switchGeneration`, not just
`pane.threadId`, because revert/reload can replace items without changing
the thread id.

The controller also exposes `warmReason` alongside `isWarm` — which of
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

Programmatic scrolls go through the controller:

- `forceStick({ reason: 'user' })` for explicit bottom-follow.
- `armRestoreSnap()` (sets the defensive escape, then arms consent)
  followed by `forceStick({ reason: 'restore' })` for thread/channel
  restore.
- `markAtBottom()` for empty-timeline restore without writing scrollTop.
- `observe(kind)` for out-of-content geometry changes ('content',
  'live-content', 'composer-geometry', 'host-layout').
- `pauseAutoScroll()` for drag/resize leases.

The virtualizer's own imperative scrolls route through the same
chokepoint: `listRef.scrollToIndex(...)` computes the target in the
engine and performs the write via the controller's `applyScrollTarget`
(wired as a `<TimelineVirtualizer>` prop), so index scrolls are tagged
and attributed like every other controller write — no wrapper call at the
call site. Engine compensation observations arrive through
`applyEngineCompensation` (see **Engine Compensation Routing**).

Never write `scrollTop` directly from feature code. The virtualizer's
`scrollToIndex` is instant-only by design — native smooth scrolling emits
asynchronous scroll events that race the controller's tagging; do not add
a smooth option, and never call `scrollIntoView()` on a virtualized row.

`prefers-reduced-motion: reduce` forces sync-pin behavior regardless of
requested animation mode. The app's `lowPowerMode` setting rides the same
gate (`motionReduced()` in the controller): spring glides are the app's
dominant GPU cost — one compositor frame per vsync for a whole chase — so
low power means instant placement everywhere, including landing an
in-flight chase on its next tick when the setting flips mid-glide. The
same setting also snaps the streaming reveal to per-wire-chunk mutations
(`PerItemSmoother`'s `revealImmediately` seam) and suppresses the
activity shimmer.

## Engine Compensation Routing

The engine compensates for geometry changes that move content above the
viewport — above-viewport row remeasures (`remeasure-above`) and load-page
head splices (`head-splice`) — by **reporting** an absolute scroll target
instead of writing `scrollTop` itself. `TimelineVirtualizer` forwards each
`EngineCompensation` to the controller's `applyEngineCompensation`, which
delegates the decision to the pure `resolveEngineCompensation`
(`utils/scroll/resolver.ts`) and applies at most one write through the
controller's chokepoint. The controller is the single `scrollTop` writer
during follow by construction (browser clamps are native and untagged).

Delivery timing is load-bearing: the virtualizer queues the compensation
and delivers it from a post-flush `$effect` — after the spacer height and
row offsets reflect the new geometry, still before paint — so the
resolver samples live `scrollHeight`, and a target beyond the old maximum
cannot clamp. The rationale lives in `TimelineVirtualizer.svelte`
("Post-flush write timing"); the regression test is the "write timing"
describe in `timelineVirtualizer.browser.test.ts`.

### The row that spans the viewport top

Rows entirely above the top compensate by their exact size delta; rows at
or below it need nothing. The at-most-one row **straddling** the top is
neither — growth in its off-screen-above part shifts everything visible
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
position and nothing else — so the two corrections compose without any
chance of double-counting.

The engine stays DOM-free: `applyMeasurements` takes an optional measurer
and calls it only for the straddling row, only when that row's height
actually moved. `boundStraddleShift` then clamps the answer to the row's
own delta (same sign, no greater magnitude) — a physical bound, since the
part above the reading position is a *part* of the whole. That is what
keeps the DOM measurement non-load-bearing: a stale anchor, a re-rendered
subtree, or a NaN can only pull the correction back toward zero — toward
the historical behavior — never past it into an over-correction.

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

- **head-splice pass** — head mutations (load-older prepend, paged
  head-drop prune) apply verbatim: the engine's offset math is exact and
  the anchor must hold.
- **reading pass** — not warm, not at bottom, escaped, or paused: the
  compensation lands unchanged (mount cascades, mid-thread reading).
  Above-viewport visual stability is the whole point; suppressing these
  visibly shifts the reading anchor.
- **anchor-redirect** — DOM already pinned to true bottom (within
  `AUTO_FOLLOW_BOTTOM_EPSILON_PX`) and the compensation requests a target
  meaningfully *above* it (numerically smaller than the bottom target):
  the write is rewritten to `targetScrollTop()`.
  The engine's `delta` only compensates above-viewport remeasures, not
  the at/below-fold growth that pushed the bottom down, so letting the
  requested value land paints one frame short of bottom — the cold
  thread-switch flicker.
- **pass** — anything else applies verbatim, mid-chase included. The
  compensation is an exact coordinate shift: layout moved the content
  under the viewport by `delta`, and the write moves the viewport by the
  same `delta` before paint, holding the visual field stationary. The
  spring re-reads `el.scrollTop` every tick, so an applied write
  mid-chase just relocates the chase — the remaining gap is unchanged
  and the glide continues seamlessly.

There is deliberately no **mid-chase decline** tier. The virtua-era gate
declined sub-viewport compensations while a spring chase was in flight,
but declining an exact compensation is what *caused* the visible jump: a
background completion patching its collapsed tool row above the viewport
shifted the content under a stationary viewport by the row's height
delta, then the spring re-chased the same distance (2026-07-21). Nor is
there an animation tier: the resolver's engaged tiers key on observed
geometry (`pinned` + `moves-away`), which makes spring-lifecycle timing
irrelevant to compensation handling — what retired the cross-file
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
asks only whether springing is *allowed* right now —
`springGateIsOpen({springStopRequested, paused, isAtBottom, escaped,
prefersReducedMotion})` — every input being an explicit signal about
scroll state or user preference, never a guess about what produced the
pixels.

This is deliberate. The controller cannot distinguish "content arrived"
from "layout got corrected": a shiki highlight resolving, KaTeX
typesetting, a mermaid diagram sizing, an image decoding, and a text
chunk landing all reach it as the same thing — the content box got
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
already mounted rows — a running tool row growing its output preview per
flush window, or running→completed result chrome landing — and wire
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
   instead of cancelling — that is what holds `springActive` true for
   the springActive-keyed resolver carve-outs (negative-delta,
   overshoot, idle deadband) through an inter-chunk pause. Without the
   distinction the sentinel would be immortal: a permanent 60Hz rAF per
   pane, since growth alone would always want a spring.
2. **The viewport path** (`notifyLiveContentMaybeGrew`). That entry
   point also carries *viewport* changes — the composer growing under a
   multi-line draft — where an instant pin is correct while the thread
   is idle. Liveness (or a pending structural append) is what
   distinguishes the two there.

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
`thread.svelte.ts`: `armLiveContentAppendSpring` (arm + liveness stamp)
for `applyProviderItemUpserts` (a wire append to the loaded tail) and
`recomputeRevealPass` (the reveal gate releasing withheld rows — rows
already in `pane.items` mount without any upsert in that flush), and bare
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
in-turn append — bug-report-20260622T041049Z), the reveal arm
additionally requires that the boundary change actually releases rows
that still exist (`boundaryChangeReleasesRows` — a gate dropping because
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
keeps pane, sidebar, and window reflows — including Mermaid `useMaxWidth`
height changes in the rendered window — from producing a visible
half-viewport spring chase just because live content advanced recently.
During that window, the compensation resolver passes the engine's
anchor-preserving writes for the same reason.

Negative content deltas usually sync-pin when the user intends to stick,
but a small negative correction during an active spring is absorbed by
the spring so estimate/correct row measurement pairs do not snap the
viewport. Width-driven shrink corrections and large overshoots still
snap immediately.

At idle — spring settled (`springToken === 0`) and pinned at the bottom —
the content box height can flip ±1-2px every ResizeObserver delivery when
the fractional sub-pixel total lands on an X.5 boundary under a fractional
device-pixel-ratio (WSLg / HiDPI). The bottom target
(`scrollHeight - clientHeight`) flips with it, and the `contentRO`
positive/negative-delta sync-pins re-pin `scrollTop` to that moving target
on every wobble frame — a self-sustaining ±2px limit cycle (the whole idle
viewport visibly vibrates). The **idle re-pin deadband** breaks it: when no
spring is in flight and `scrollTop` is already within
`IDLE_REPIN_DEADBAND_PX` of the target, the re-pin is skipped
(`idlePinWithinDeadband`, folded into both pin predicates — since the
Stage-2 extraction this decision lives in `utils/scroll/resolver.ts`,
the controller's pure decision core). It keys on distance-from-target,
not delta magnitude,
so genuine growth moves the target ≥ a line height (gap ≫ deadband) and
pins normally; the `springToken === 0` gate makes it idle-scoped by
construction — during streaming the spring holds its token across
inter-chunk gaps, so an active chase is never touched. Full mechanism + the
capture it was root-caused from:
[`settle-flicker-analysis.md`](settle-flicker-analysis.md). Coverage: the
net-zero `±2px` oscillation test (≤2 `scrollTop` writes with the gate, 24
without).

## Layout Rules

`ChatView.svelte` positions the composer and live-turn UI as an absolute
overlay. A `--composer-height` CSS variable drives `scrollEl`
`padding-bottom`, keeping composer growth from changing the scroll
surface's `clientHeight`.

The composer ResizeObserver writes `--composer-height` directly before
observing `'composer-geometry'` on the scroll controller. Waiting for
Svelte's microtask flush would pin against stale layout. Idle composer
geometry resolves to the same same-paint pin as a `'content'`
observation; when live content is inside the spring hold window, the
live-capable path keeps following the moving bottom instead of
sync-pinning mid-chase.

`overflow-anchor: none` belongs on the outer scroll container (and is
also set on the virtualizer's spacer). Browser scroll anchoring otherwise
fights the engine's compensation targets and the controller's sync-pin.

`scrollbar-gutter: stable both-edges` belongs on the chat scroll
container. The styled `::-webkit-scrollbar` (app.css) is a classic,
space-consuming bar, not an overlay, so a bare `overflow-y: auto` shifts
the centered `mx-auto max-w-[62rem]` rows ~5px left — out of alignment
with the absolute composer overlay — the moment the bar appears.
`both-edges`, not single-edge `stable`, is required: WebKitGTK reserves
the gutter only while the bar is actually present, so a single edge still
shifts the column on the idle→scrolling transition. Symmetric reservation
holds the center in both states and reserves nothing when idle (no
always-visible bar). `ChannelView` is left-aligned, so the bar reflows
only its right edge and needs no gutter — keep this directive chat-only.

A gutter does not transfer to a scroller *inside* the centered column: it
would inset that box's content relative to the prose above and below. The
activity-run clip therefore suppresses its native bar to zero width and
renders an out-of-flow overlay thumb instead — see
[Nested scrollers](#the-activity-run-a-nested-scroller-with-the-panes-physics).

Status banners are absolute overlays, not reserved layout slots. They
must not change the scroll surface height on mount/unmount.

## Nested Scrollers And Gesture Attribution

Rows may own scrollable bodies (command output, subagent children,
wait-group children, tool-result patches, payload bodies). Wheel and
touch events bubble, so without help the outer intent machine cannot
tell a gesture aimed at the pane from one a nested box just absorbed —
and treating the latter as "the user left the bottom" broke follow while
the outer pane had not moved at all.

Every such box opts in with the `nestedScroll` action
(`utils/scroll/wheelAttribution.ts`). On each gesture the intent machine
walks `event.target` up to its own scroll element, and the first
registered scroller that can still move in that direction owns the
event; the machine then ignores it. Nothing registered can consume it →
the gesture belongs to the boundary, and native scroll chaining takes it
there. That chaining is deliberate — browsing up out of a nested box has
to reach the pane — which is why nested boxes keep the default
`overscroll-behavior` rather than `contain`.

A registry, not a computed-style probe: wheel handling runs while layout
is dirty mid-stream, so `getComputedStyle` over every ancestor of every
wheel event would force reflows at gesture rate. Geometry reads stay
confined to explicitly marked elements — usually zero or one per gesture.

The same helper serves nested controllers: an inner controller passes its
own clip as the boundary, so a command-output box inside an activity run
attributes correctly against both levels.

Adding a new scrollable row body means adding `use:nestedScroll` to it.
Contract: [`scroll-contracts.md`](scroll-contracts.md) C7.

### The activity run: a nested scroller with the pane's physics

Most nested bodies are inert boxes. An activity run's clip is not: the run
holding the live tail gets its own `createUseStickToBottomController`, same
spring and glide compositing as the pane, so streaming activity chases inside
the cap while the prose above it stays put. Rules that matter from the outer
side ([`activity-runs.md`](activity-runs.md) has the rest):

- **Only the live run.** Historical runs are plain `overflow-y: auto` with a
  restored `scrollTop`; a controller per run in the buffer would be a spring,
  an observer set, and intent listeners each for physics one of them can use.
- **The clip's outer height changes only on explicit events** — growth toward
  the cap, item expansion, a collapse toggle — never from inner streaming.
  That is what keeps the outer engine quiet, and it keeps `rowDelta === 0` for
  the straddling row during inner scrolling, so the reading-anchor measurer
  never sees inner movement.
- **The nested controller leaves `externalContentGeometry` unset** (no
  virtualizer inside a run, so its own contentEl ResizeObserver is the right
  source — the `ChannelView` precedent) and never touches
  `pane.attachScrollController`.
- **Inner scroll position lives in the per-pane registry**, keyed by the
  registry-assigned `runId`, so a run the reader scrolled inside does not snap
  back to its tail every time the virtualizer evicts its row.
- **The clip's native scrollbar is suppressed to zero width** and the
  affordance is a `components/shared/OverlayScrollbar.svelte` in the column's
  padding. A gutter cannot be used inside the centered column — it would inset
  the run's rows off the rail the run draws — and a bar that takes width would
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

Measured row heights ARE replayed across thread switches — and across an
app restart — under a per-row validity model (`utils/virtual/priors.ts`).
The `{#key pane.threadId}` block remounts the `<TimelineVirtualizer>` on
every switch, so without a replay it re-runs the full estimate→measure
cascade — the thread-switch flicker, identical for cached and uncached
threads because the item cache avoids the *fetch*, not the *remeasure*.
The priors replay makes the mount start at the already-measured total:
the engine's `RowEstimate` resolves each unmeasured row from the previous
visit's persisted measurement (falling back per-row to the kind-height
table, then the flat default), and the per-row ResizeObserver's first
delivery matches the estimated size, so `applyMeasurements` no-ops it.
Zero re-render, zero scroll jump.

Priors are keyed by a **per-row content signature** (`nodeSignature` in
`utils/timelineStructureSignature.ts` — id, status, `summary.length`,
`updatedAt` for a leaf; key + member count for a group), not by
position. Each thread's `SizePriorsEntry` is `{ width, expansionSig,
rows: Map<signature, measuredPx> }`: **width** (the wrap point) and
**expansionSig** (`pane.expansionSignature()`, non-default subagent/diff/
payload expansion) gate the whole entry — a mismatch on either refuses
every row in it, degrading the mount to the kind/flat estimate chain,
same as a cold first visit. The per-row signature gates each row
independently within a valid entry: a row whose content changed simply
misses on its own map key, without invalidating its still-valid
siblings. This replaced an earlier design that keyed ONE positional
`sizes: number[]` snapshot against a **whole-window** structure
signature (the newline-join of every loaded row's signature) — a key
that a fresh app boot's small initial window essentially never matched
against a full session window of hundreds of streamed/paged rows, so
restart replay was effectively dead. The per-row map fixes that: a boot
window's rows are a *suffix* of a larger captured window, and each one
resolves independently. A handful of global display settings (`fontSize`,
the sans/mono fonts, `collapseDiffPreviews`) also change row height but
are deliberately **not** keyed — a documented, benign residual: toggling
one mid-session then revisiting a thread replays stale heights, which the
warm-up gate masks as a cold first visit (the estimate→measure cascade
re-runs and corrects them), never a crash or stuck viewport. Keying them
would make the residual airtight but buys no visible change (same masked
cascade either way) at the cost of a drift-prone signature; the choice is
recorded in `priors.ts`. Row-UI state is reset to default on every switch
(`rowUiState.clear()`), so at restore time the expansion signature is the
default one — which is exactly why a thread that was idle-at-default
replays cleanly and a thread that had something expanded (taller rows) is
correctly refused.

The width/expansion validity check is deliberately **lazy-once**, not
eager: `resolveRowEstimateOnThreadEdge` (`timelineSizePriors.svelte.ts`)
runs in `$effect.pre` before the virtualizer remounts, and on a fresh app
boot the scroll surface has not been laid out yet — an eager check would
read width 0 and spuriously refuse every restart replay. The check
instead runs on the first `RowEstimate.at()` call and is memoized for
the rest of that mount. Even at first use the width can still
legitimately read 0: the width signal is RO-only (async by the
one-source rule in `scrollSurfaceWidth.ts`), while the engine's first
`at()` calls run synchronously when the virtualizer mounts with data —
on boot, whichever lands first is a machine-speed race. Width 0
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
and the rising edge of `stick.isWarm` — the controller's
"measurement-cascade-settled" signal — which guarantees a final-height
capture for a thread the user views but never scrolls.

The in-memory store is a bounded LRU (memory: ~one float per loaded row
per recent thread) — a WORKING SET over a persistent backing store, not
the store itself. `utils/virtual/priorsStorage.ts` is a
localStorage-backed adapter (installed at module scope of
`timelineSizePriors.svelte.ts`, so it is active before any pane mounts)
that makes priors survive an app restart: writes are debounced (~1s,
coalesced per thread) and flushed early on `pagehide`/hidden
`visibilitychange`; reads lazily hydrate a memory miss. A 50-thread
storage cap and 50-thread in-memory cap both evict LRU-oldest — a memory
eviction never touches the persistent store (the thread's priors are
still on disk and rehydrate on its next visit); only an explicit
`clearThreadSizePriors` (thread deletion, item mutation, same-thread
reswitch — `thread.svelte.ts`, `threads.svelte.ts removeThread`) deletes
from storage too. Storage failures (quota, corrupt JSON from a stale
schema version or a hand-edited profile) warn once and degrade to
"priors don't persist this session" rather than throwing. None of this
violates the visible-thread memory budget or touches SQLite — it is
still a session-scoped cache in memory, just one that can rehydrate
itself from disk. Kind estimates for priors-miss rows are **floors, not
averages** (`ROW_KIND_ESTIMATE_PX` in
`components/chat/timelineSizePriors.svelte.ts`): an estimate above a
row's real height shrinks `totalSize` on first measurement — a scrollbar
dip plus a synchronous browser `scrollTop` clamp at exact bottom — while
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
clean bottom landings) — see
[`scroll-rearchitecture-plan.md`](scroll-rearchitecture-plan.md) §3.
Async-short remount content is bridged at the content layer (streamdown
mermaid/math rendered-height caches, the attachment blob cache), and
`remountReturn.browser.test.ts` pins the outcomes. Two pieces survive the
deletion:

- The scroll surface width signal: the **content-box** width, observed
  asynchronously through `observeScrollSurfaceContentWidth`
  (`scrollSurfaceWidth.ts`), feeds the priors validity key —
  never a synchronous or border-box read (`getBoundingClientRect`,
  `clientWidth`). A second, disagreeing width source turns the width signal
  into a self-sustaining oscillation that re-renders every row at idle
  (CPU/heap-churn incident 2026-06-26, commit `a5a5d032`).
- Margin containment: the `[data-row-geometry-content]` row wrapper and the
  app.css `display: flow-root` rule (commit `4b3759a1`) — independent of the
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
`listRef.scrollToIndex(index, { align: 'center' })` — an escaped,
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
`afterSeq = -1` — the exclusive "everything" cursor, since message
sequences are zero-based and a `0` cursor would silently drop the
channel's first message.

The current speaker's in-flight text streams like chat: a discussion
child thread (one participant's own session) has no mounted pane, so
`eventsItemStream.ts` feeds its `assistant_text` upserts/deltas through a
side-channel registry (`discussionLiveTail.ts`) into the channel state's
`liveTail`, keyed by the roster `discussion:state` last published.
`ChannelView` renders that tail as a streaming card and lets it fall
away once the matching agent message lands.

Growth animates exactly as it does in chat — unconditionally, subject
only to the signal-based spring gate (see Live Content Animation above).
The channel keeps its own liveness stamp for the sentinel and viewport
paths: the channel state stamps `lastLiveContentAt` on every
genuinely-new message and on live-tail growth, and `ChannelView` calls
`isLiveContentActive(now, pane.channelLastLiveContentAt,
LIVE_CONTENT_ACTIVE_HOLD_MS)` for the controller's `liveContentActive`
option. This is the channel's own stamp, independent of the chat
timeline's `lastLiveContentAt` — the two
surfaces never mount at once, but each pane tracks both so switching a
pane between chat and discussion mode never reads a stale latch.

## Diagnostics

Scroll bugs are usually second-order interactions between controller
flags, row measurement, and browser layout. Reproduce with
`make dev DEBUG=1`, then inspect `window.__agentOverflowUiTrace.dump()` or
`.filePath()`.

The trace surface has two tiers (Makefile: `UI_TRACE`, `UI_ORACLES`;
`DEBUG=1` enables both). `UI_TRACE=1` alone is the light tier — event
traces plus the spring chase telemetry below — cheap enough to measure
production-representative frame cadence (`make build-wsl UI_TRACE=1`
builds the minified bundle with only this tier). `UI_ORACLES=1` adds the
standing regression oracles (`timeline.row.resize`,
`timeline.margin.diverge`, `timeline.reasoning.tailJump` — each an extra
per-row ResizeObserver plus a subtree MutationObserver) and the
throttled DOM snapshot walks (`timeline.dom` / `chat.dom` /
`plan-sidebar.dom`), which are the expensive part of the surface during
streaming.

Useful trace records:

- `scroll.spring.chase` — one summary per spring chase (emitted at
  cancel; chases under 3 ticks are skipped): tick counts (write /
  sentinel), a frame-gap histogram (`gapBuckets`, bounds
  `[<9, 9–13, 13–18, 18–26, 26–42, >42]` ms — see
  `CHASE_GAP_BUCKET_BOUNDS_MS`), `maxGapMs`, catch-up clamp count
  (`catchupClamps`), chase-distance jumps (`distanceJumps` — the
  `spring.catchupJump` write that, after an observed rAF discontinuity,
  re-enters the glide one viewport from the target instead of animating
  the whole frozen-tab backlog), target changes, sentinel entries
  (stop/restart cycles), and long-task count/duration during the chase
  (Chromium `longtask` observer; absent under WebKit). This — not the 1-in-12 sampled `spring.tick` spacing —
  is how to judge whether a chase actually dropped frames; see the
  telemetry footgun note in
  [`settle-flicker-analysis.md`](settle-flicker-analysis.md).
- `scroll.contentRO` — content-geometry delta, width-reflow state, and pin
  decisions (in chat the delivery is engine-sourced, not an RO fire; the
  record name stays for trace continuity, and `settleEvidence` reports
  the warm gate's per-row settle input).
- `scroll.contentRO.widthReflow` — width-only content reflow that armed
  the short layout-correction window.
- `scroll.escape.set` — escape state changes.
- `scroll.refreshIsNearBottom` — geometric near-bottom changes.
- `chat.state` / `chat.dom` — MessageTimeline snapshots.
- `timeline.margin.diverge` — settle-flicker regression oracle. Fires when a
  row's bottom margin escapes its content box (the measured row total counts
  it; the row's content-box observer does not), which used to drive a
  `contentRO` transient and an `oscillationSnap`. Must stay silent; see
  [`settle-flicker-analysis.md`](settle-flicker-analysis.md) for the root cause
  and the `[data-row-geometry-content] { display: flow-root }` containment fix.
- `timeline.coldload` — one record per pane per thread-switch cold load
  (`utils/coldLoadTrace.ts`), consolidating the switch-to-warm timeline
  into segments instead of leaving them scattered across the
  `scroll.warmup.*` / `chat.state` records above. Fields: `source`
  (`'cache-restore'` or `'fetch'`), `fetchMs` (switch start → initial
  slice applied; `null` on a cache restore), `itemCount` (rows in that
  slice), `settleMs` (slice applied — or switch start on a cache
  restore — → the warm gate's rising edge), `totalMs` (switch start →
  warm), `warmReason` (the controller's `warmReason` at that edge —
  `'quiet'`, `'failsafe'`, `'settled'`, or `'skip'`), and `priors` (the
  size-priors replay summary stamped by MessageTimeline's warm-edge
  effect — `{source, validity, rowsResolved}` per
  `SizePriorsReplayStats` in `timelineSizePriors.svelte.ts`, or `null`
  when no timeline stamped one). A large `fetchMs` points at the
  backend/IPC leg; a large `settleMs` with `warmReason: 'failsafe'`
  points at a measurement cascade that never went quiet; `priors`
  distinguishes "no stored entry" from "entry refused (width/expansion
  mismatch)" from "replayed N rows".
  Needs a `make dev DEBUG=1` build (`VITE_AGENT_OVERFLOW_UI_TRACE=1`).

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
controller path.

## Accepted Tradeoffs

Nested row overflow is allowed for large subagent, wait-group, and command
output bodies. Focus can jump to `<body>` when the windowing unmounts the
focused row. Syntax highlighting is backend span metadata
(`internal/highlight`); the frontend renders spans over text it already
holds.
