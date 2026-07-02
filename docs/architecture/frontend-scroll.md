# Frontend Scroll Architecture

This is the durable contract for chat and discussion scrolling. It keeps
the operational rules out of `AGENTS.md` while preserving the invariants
that matter when changing `ThreadPane`, `MessageTimeline`, `ChannelView`,
the timeline virtualizer, or the scroll controller (`utils/scroll/`).

## Owners

- `MessageTimeline.svelte` owns the outer chat scroll container.
- `components/chat/TimelineVirtualizer.svelte` + `utils/virtual/` own
  virtual row geometry. The split inside:
  - `utils/virtual/` — the bespoke windowing engine, pure data + math
    with no DOM and no Svelte: `sizes.ts` (the size store: measured px
    or estimate per row, memoized offsets), `window.ts` (visible-range
    math), `engine.ts` (the reducer: scroll/resize/measurement/length
    inputs in → mount window + totalSize + at most one compensation
    observation out), `priors.ts` (per-thread measured-size persistence
    and the estimate resolver), `types.ts` (the shared shapes).
    Design doc: [`virtualizer-replacement-plan.md`](virtualizer-replacement-plan.md).
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
  - `intent.ts` — the event-sourced intent machine: wheel/scroll/pointer/
    key/touch listeners, escape and re-stick, restore-snap consent, and
    programmatic-write tagging. Intent is never geometry-inferred.
  - `spring.ts` — chase kinematics: HOW a spring advances scrollTop
    frame to frame once the controller decides one runs.
  - `observers.ts` — the content-geometry delivery pipeline, the warm-up
    (quiescence) gate, and resize classification. Two sources feed the
    one pipeline: engine-sourced samples in chat
    (`externalContentGeometry`), a contentEl ResizeObserver everywhere
    else (ChannelView). Each delivery is gathered here, decided by the
    resolver, and applied through the controller's chokepoint, so "a
    content delivery" reads in one place.
- `ThreadPane` owns the scroll-controller registration slot so shared
  surfaces can pause or notify scrolling without reaching into component
  internals.
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
checkpoint refresh, and the initial slice under one `Promise.allSettled`.
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

## Load Paging (head-splice `shift`)

`loadOlder` and `loadNewer` mutate the window at one end and prune the
other, and they drive the virtualizer's `shift` head-splice so the reading
position holds without an explicit re-anchor. `shift` tells the engine
which end a length change hit: on a **head** change `applyLength` splices
the size store at the head (`spliceHead`), remaps the estimate's
index-keyed priors (`RowEstimate.shiftBase`), and reports a `head-splice`
compensation whose target keeps the viewport stationary — applied in the
same flush; on a **tail** change it does neither. Without it every change
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
correction and holds the gate exactly as before. The geometry gate
therefore guards only the **first** visit to a thread within a session —
where no priors exist yet and the estimate→measure cascade is
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
requested animation mode.

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
- **width-reflow pass** — during the width-reflow settle window the paired
  contentRO sync-pins, so the compensation lands in the same paint.
- **mid-chase decline** — a spring is in flight (or sentinel-alive) and
  the jump is within one viewport: decline. A decline needs no follow-up:
  the engine's scroll offset syncs from real scroll events, so an
  unapplied compensation cannot desync its model — the content simply
  shifts under the stationary viewport. Larger jumps are bulk layout
  corrections (fresh-mount estimate→measure, late shiki/katex/mermaid
  typesetting) and fall through to the final pass so they snap in one
  paint instead of becoming a multi-hundred-px spring chase.
- **pass** — anything else applies verbatim.

There is deliberately no `animationMode` tier: keying the decline on
`springActive` alone makes mode-latch timing irrelevant to compensation
handling, which is what retired the `SPRING_MODE_HOLD_MS >
RETAIN_ANIMATION_DURATION_MS` cross-file invariant.

Routed writes are controller writes: attributed (`engine.compensation` /
`engine.anchorRedirect` in the `scroll.write` trace) and self-tagged for
the scroll handler like every other programmatic write. Decision-level
coverage lives in `resolver.test.ts`; the controller wiring in
`scroll/index.svelte.test.ts` ("engine compensation applier"); the
adapter seam (delivery timing, windowing, measurement) in
`timelineVirtualizer.browser.test.ts`; and the user-visible outcomes in
`compensationOutcome.browser.test.ts`.

## Live Content Animation

Chat chooses animation mode with a content-keyed latch. `ThreadPane`
stamps `lastLiveContentAt` whenever live timeline content advances:
assistant prose, thinking, compaction reasoning, direct text patches, and
visible-field updates to already mounted rows — a running tool row growing
its output preview per flush window, or running→completed result chrome
landing. `MessageTimeline` returns `spring` for `SPRING_MODE_HOLD_MS`
after that stamp and `instant` otherwise.

The spring is keyed on content arrival, not provider turn state. It
therefore covers end-of-turn smoother drains and text-stream gaps, while
tool row INSERTS (whose estimate→remeasure churn would spring-chase
transient targets) and late Streamdown typesetting on settled content
sync-pin invisibly by default. The stamp is window-wide, not
viewport-local: a rendered-field change anywhere in the loaded window
opens the hold, so an unrelated bottom reflow landing within it springs
instead of pinning — accepted bleed, since the window is short and keyed
to real content changes. The 500ms hold is pure tuning — the historical
requirement that it outlast the spring sentinel retain duration died with
the descriptor gate (see Engine Compensation Routing).

Structural transcript appends have a narrower override:
`markStructuralContentPending()` makes the next near-term command/tool row
growth spring-eligible even when the content latch currently returns
`instant`. This is intentionally one-shot. After the structural append
spring arrives it cancels instead of entering the streaming sentinel, so
routed engine compensations are not declined after the append settles.

The pane data layer is the sole owner of the arm
(`armStructuralSpring` in `thread.svelte.ts`), with two call sites:
`applyProviderItemUpserts` (a wire append to the loaded tail) and
`recomputeRevealPass` (the reveal gate releasing withheld rows — rows
already in `pane.items` mount without any upsert in that flush). Both run
synchronously with the data change, strictly before the Svelte flush in
which the virtualizer measures the new/released rows and delivers their
geometry sample. An effect-based arm (MessageTimeline's former
live-follow signature effect) loses that ordering race — the append's own
growth resolves instant and only the follow-up remeasure springs — and a
turn-keyed effect is blind to appends landing after turn end (interrupt
echo, force-closed tool rows), which sync-pinned as whole-viewport
teleports (bug-report-20260702T193212Z). The arm is a TTL refresh
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

Status banners are absolute overlays, not reserved layout slots. They
must not change the scroll surface height on mount/unmount.

## Row And Payload State

Virtualized rows can remount at any time. User-visible row state that must
survive remount lives on `ThreadPane` registries keyed by item id,
payload id, or subagent group key. Loaded payload bytes live in the
byte-bounded module cache in `payloadDataCache.ts`.

Measured row heights ARE replayed across thread switches, but only under
a strict validity key (`utils/virtual/priors.ts`). The `{#key
pane.threadId}` block remounts the `<TimelineVirtualizer>` on every
switch, so without a replay it re-runs the full estimate→measure cascade —
the thread-switch flicker, identical for cached and uncached threads
because the item cache avoids the *fetch*, not the *remeasure*. The priors
replay makes the mount start at the already-measured total: the engine's
`RowEstimate` resolves each unmeasured row from the previous visit's
persisted measurement (falling back per-row to the kind-height table,
then the flat default), and the per-row ResizeObserver's first delivery
matches the estimated size, so `applyMeasurements` no-ops it. Zero
re-render, zero scroll jump.

The replay is sound only when the rows re-render at the heights they were
measured at, so the stored sizes are refused (every row falls back to its
kind estimate — the cold-mount behavior, never worse) unless three things
still match: **scroll-pane width** (the wrap point), a **structure
signature** (`timelineStructureSignature` over the rendered node sequence
plus per-leaf content — id, status, `summary.length`, `updatedAt`), and a
**non-default expansion signature** (`pane.expansionSignature()` over
expanded subagent groups, diff overrides, and payloads). A handful of global
display settings (`fontSize`, the sans/mono fonts, `collapseDiffPreviews`)
also change row height but are deliberately **not** keyed — a documented,
benign residual: toggling one mid-session then revisiting a thread replays
stale heights, which the warm-up gate masks as a cold first visit (the
estimate→measure cascade re-runs and corrects them), never a crash or stuck
viewport. Keying them would make the residual airtight but buys no visible
change (same masked cascade either way) at the cost of a drift-prone
signature; the choice is recorded in `priors.ts`. The structure signature
superseded an earlier version of this key that read `pane.timelineRevision`
— a monotonic per-pane counter that is never restored on a cache-hit
re-entry, so every revisit computed a strictly-greater revision than
capture and the replay **never matched** (the cache was inert; the
switch-back flicker was never actually fixed). `pane.timelineRevision`
itself remains as the timeline-derivation reactivity trigger; it was just
the wrong input to key the size replay on. The signature is reproducible
instead: revisiting a settled thread yields the identical string, and it
is content-aware (Go bumps `updatedAt` on every streaming append), so a
backgrounded thread that changed and got reloaded is refused on the key
alone — eviction (`thread.svelte.ts` removal/reswitch,
`threads.svelte.ts removeThread`) is memory housekeeping, not the
correctness guard. Row-UI state is reset to default on every switch
(`rowUiState.clear()`), so at restore time the expansion signature is the
default one — which is exactly why a thread that was idle-at-default
replays cleanly and a thread that had something expanded (taller rows) is
correctly refused.

Captures must store the **settled** sizes or the replay restores a
mid-cascade height. They ride two triggers, both routed through one
size-gated persist (gated on `getScrollSize()` so a 60Hz spring does not
re-slice the array): the scroll-position snapshot (`saveScrollSnapshot`),
and the rising edge of `stick.isWarm` — the controller's
"measurement-cascade-settled" signal — which guarantees a final-height
capture for a thread the user views but never scrolls. The store is a
bounded session-only LRU (memory: ~one float per loaded row per recent
thread) — it does not persist to SQLite and does not violate the
visible-thread memory budget. This replays revisits within a session; a
genuine first visit has no priors and still cascades (hidden by the
warm-up gate, above). Kind estimates for priors-miss rows are **floors,
not averages** (`ROW_KIND_ESTIMATE_PX` in `MessageTimeline`): an estimate
above a row's real height shrinks `totalSize` on first measurement — a
scrollbar dip plus a synchronous browser `scrollTop` clamp at exact
bottom — while an undershoot only grows `totalSize`, absorbed invisibly
by remeasure-above compensation at the cost of a few extra transiently
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

## Diagnostics

Scroll bugs are usually second-order interactions between controller
flags, row measurement, and browser layout. Reproduce with
`make dev DEBUG=1`, then inspect `window.__agentOverflowUiTrace.dump()` or
`.filePath()`.

Useful trace records:

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
focused row. Shiki remains a frontend dependency through `svelte-streamdown`
for assistant markdown and a few payload expansions.
