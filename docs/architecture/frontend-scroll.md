# Frontend Scroll Architecture

This is the durable contract for chat and discussion scrolling. It keeps
the operational rules out of `AGENTS.md` while preserving the invariants
that matter when changing `ThreadPane`, `MessageTimeline`, `ChannelView`,
or `useStickToBottom`.

## Owners

- `MessageTimeline.svelte` owns the outer chat scroll container.
- `virtua/svelte` owns virtual row geometry and per-row measurement.
- `useStickToBottom.svelte.ts` owns user scroll intent and every allowed
  `scrollTop` write outside virtua internals.
- `ThreadPane` owns the scroll-controller registration slot so shared
  surfaces can pause or notify scrolling without reaching into component
  internals.
- `threadScrollSnapshots.ts` owns semantic per-thread scroll snapshots:
  `{ kind: 'bottom' }` or `{ kind: 'anchor', itemId, offsetTop }`.

Do not add another owner for any of those responsibilities.

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
position through virtua's native `shift` (see **Load Paging** below), so
auto-load-newer never scrolls and auto-load-older leaves the user exactly
where they were reading even if they keep scrolling as the page arrives.

`threadItemCache.ts` is a small LRU of visible-window snapshots, not a
full-history cache. It rejects oversized snapshots, evicts inactive
threads touched by persisted mutations, and force-evicts same-thread
reloads so revert/reload flows do not paint stale rows.

`mergeMissingItemsById` is the merge contract for initial load and older
paging. Existing in-memory rows keep their references; missing rows are
added and the result is sorted. This preserves virtua row identity while
remaining correct under persist-then-emit ordering.

## Load Paging (virtua `shift`)

`loadOlder` and `loadNewer` mutate the window at one end and prune the
other, and they drive virtua's native `shift` so the reading position
holds without an explicit re-anchor. `shift` tells virtua which end a
length change hit: on a **head** change it unshifts (grow) or front-splices
(prune) its size cache and compensates `scrollTop` in the same frame; on a
**tail** change it does neither. Without it virtua assumes every change is
at the tail and misindexes its whole size cache on a prepend, forcing a
re-measure of every visible row — the "viewport shifts, scrollbar jumps
around" load jank.

The store exposes the paging direction as `pane.pendingTimelineShiftAtHead`,
bound into `<Virtualizer shift={...}>`, set synchronously at the `items`
mutation so virtua reads it in the same flush, and reset in the paging
method's `finally`. The grow (prepend/append) and the prune are deliberately
split across **two flushes** (`await tick()` between them): coalesced, a
head-grow plus a tail-shrink collapse into one net length change that a
single `shift` boolean cannot describe — and when the page budget equals
the prune count the net length is unchanged, so virtua dispatches nothing
at all and the cache scrambles. This was confirmed by driving the installed
virtua core directly; the cache semantics are codified in
`virtuaShiftCache.test.ts` as a version-bump tripwire, and the store's
two-flush sequencing + shift direction are covered in
`thread.svelte.test.ts`.

`loadOlder` / `loadNewer` apply the paired prune directly (the dropped end is
always opposite the reading viewport, so there is nothing to veto or restore).
The streaming / settle prune keeps the explicit anchor transaction
(`preserveTimelineWindowAnchor`, below) because it can fire under a
bottom-pinned viewport where the incident-hardened defer-and-restore behavior
matters. That path does not ask the pane's raw item window whether the prune
is a head-drop; `<Virtualizer>` receives filtered/grouped `revealedNodes`.
`MessageTimeline` compares the rendered `timelineNodeKey` list before and
after the prune, and marks a local one-flush `shift` only when the rendered
nodes are a strict suffix. That prevents a prune through a Read group,
notification filter, subagent group, or reveal boundary from splicing virtua's
size cache against the wrong row set.

## Live Window Bounds

The streaming append path caps the loaded window
(`ACTIVE_TIMELINE_WINDOW_MAX_ITEMS`, pruning back to
`ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS`), but the prune **defers to turn
settle** while a turn is active. A mid-stream head-drop collapses content
height under a bottom-pinned viewport, the browser clamps `scrollTop`,
and virtua re-measures — a visible blank flash (incident 2026-06-10).
`ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS` is the memory backstop: a
single turn streaming past it gets pruned mid-turn anyway.

The streaming / settle window prune goes through `MessageTimeline` when a
timeline is mounted (the paging prunes use `shift` instead — see **Load
Paging**). The pane owns the window decision, but the timeline owns the
DOM/virtua anchor transaction: bottom intent pins to the new bottom, and
reading state preserves the first visible item when that item survives
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

Fresh virtua mounts first estimate row heights and then correct them as
ResizeObservers fire. On long threads that correction can shift the
viewport by hundreds of pixels, so chat hides `contentEl` while
`!stick.isWarm`.

Warm-up stays closed until a content ResizeObserver event has fired and
then gone quiet for `QUIET_MS`, or until `FAILSAFE_MS` elapses. The quiet
timer is gated on ResizeObserver evidence; do not replace it with a plain
wall-clock delay.

A consumer that knows its async typesetting (svelte-streamdown's
shiki/katex/mermaid) has settled can pass `quietContextSignal` to shorten
the quiet window to `SETTLED_QUIET_MS` (~one frame). That shortcut is
itself gated on **geometry stability**: `quietContextSignal` is blind to
virtua's estimate→measure cascade, which grows `scrollHeight` over a
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
**revisited** thread the cascade is instead *eliminated*: `MessageTimeline`
replays the previous visit's measured-size snapshot into virtua (see "Row
And Payload State" below), so the surface mounts at its final height and
the gate sees ~zero contentRO deltas and reveals immediately. The geometry
gate therefore now guards only the **first** visit to a thread within a
session — where no snapshot exists yet and the estimate→measure cascade is
unavoidable — and is best-effort there. Both defenses coexist: replay
removes the cascade when it can, the gate hides it when it can't.

On thread switch, `MessageTimeline` must call `stick.armWarmup()` from
`$effect.pre` so `isWarm=false` before the new DOM paints. The restore
effect then calls `stick.forceStick({ reason: 'restore' })` and schedules
one rAF `notifyContentMaybeGrew()` settle pass for late composer padding,
virtua measurement, or Streamdown layout changes. The rAF pass is
escape-aware.

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
- `armRestoreSnap()` followed by `forceStick({ reason: 'restore' })` for
  thread/channel restore.
- `markAtBottom()` for empty-timeline restore without writing scrollTop.
- `runExternalScroll()` for virtua `scrollToIndex` calls.
- `pauseAutoScroll()` for drag/resize leases.

Never write `scrollTop` directly from feature code. Never pass
`smooth: true` to virtua `scrollToIndex`; native smooth scrolling emits
asynchronous scroll events that race the controller's tagging.

Every controller write is preceded by virtua manual-scroll marking: the
controller's `onBeforeScrollTopWrite` hook — wired by MessageTimeline to
the patched `VirtualizerHandle.markProgrammaticScroll()`
(`patches/virtua@0.49.1.patch`) — fires before each `scrollTop`
assignment so virtua never classifies a pin write as a user scroll-down.
Unmarked writes latch virtua's scroll direction and drop the entire
above-viewport buffer, the remount churn behind the streaming settle
flicker (`settle-flicker-analysis.md`, 2026-07-01 streaming
settle-flicker entry). virtua clears the mark at scrollend, which is why
the hook fires per write, not per burst. Controller write paths inherit
the marking automatically as long as they route through
`writeProgrammaticScrollTop`; a raw `scrollEl.scrollTop` assignment
anywhere else reintroduces the churn. Regression coverage:
`src/test/integration/virtua-patch-buffer-retention.browser.test.ts`
(patch tripwire + marked-write guard) and
`messageTimelineVirtuaMarking.test.ts` (component wiring seam).

`prefers-reduced-motion: reduce` forces sync-pin behavior regardless of
requested animation mode.

## Virtua Compensation Routing

Virtua's internal `$fixScrollJump` compensates for above-viewport row
remeasures while measurements settle — the one internal `scrollTop` write
our virtua configuration produces. It no longer writes the DOM directly:
the patched scroll-applier seam (`patches/virtua@0.49.1.patch`, wired in
`MessageTimeline` via `listRef.setScrollApplier`) hands each compensation
to the controller's `applyVirtuaScrollCompensation`, which delegates the
decision to the pure `resolveVirtuaCompensation`
(`utils/scroll/resolver.ts`) and applies at most one write through the
controller's chokepoint. The controller is the single `scrollTop` writer
during follow by construction (scheduled navigations still write inside
virtua under `runExternalScroll` tagging, and browser clamps are native);
the old property-descriptor gate on `scrollEl.scrollTop` is gone.

The resolver's decision order (each tier's regression provenance is
documented at the function):

- **shift pass** — `shift: true` compensations (head prepends from load
  paging) apply verbatim.
- **reading pass** — not warm, not at bottom, escaped, or paused: virtua's
  anchor preservation lands unchanged (mount cascades, mid-thread
  reading).
- **anchor-redirect** — DOM already pinned to true bottom (within
  `AUTO_FOLLOW_BOTTOM_EPSILON_PX`) and the compensation requests a target
  meaningfully *below* it: the write is rewritten to `targetScrollTop()`.
  virtua's `delta` only compensates above-viewport remeasures, not the
  at/below-fold growth that pushed the bottom down, so letting the
  requested value land paints one frame short of bottom — the cold
  thread-switch flicker.
- **width-reflow pass** — during the width-reflow settle window the paired
  contentRO sync-pins, so the compensation lands in the same paint.
- **mid-chase decline** — a spring is in flight (or sentinel-alive) and
  the jump is within one viewport: decline. The patched core responds to a
  decline by poking its own store at the current DOM offset
  (manual-mark + `ACTION_SCROLL`), so virtua's internal offset model can
  never desync from the DOM — the failure mode that made the old gate's
  silent drops regression-prone. Larger jumps are bulk layout corrections
  (fresh-mount estimate→measure, late shiki/katex/mermaid typesetting) and
  fall through to the final pass so they snap in one paint instead of
  becoming a multi-hundred-px spring chase.
- **pass** — anything else applies verbatim.

There is deliberately no `animationMode` tier: keying the decline on
`springActive` alone makes mode-latch timing irrelevant to compensation
handling, which is what retired the `SPRING_MODE_HOLD_MS >
RETAIN_ANIMATION_DURATION_MS` cross-file invariant.

Routed writes are controller writes: attributed (`virtua.jump` /
`virtua.anchorRedirect` in the `scroll.write` trace), self-tagged for the
scroll handler, and preceded by the virtua manual-scroll marking hook like
every other programmatic write. Decision-level coverage lives in
`resolver.test.ts`; the applier wiring in `useStickToBottom.svelte.test.ts`
("virtua compensation applier"); the patch seam itself in
`virtua-patch-scroll-applier.browser.test.ts`; and the user-visible
outcomes in `compensationOutcome.browser.test.ts`.

## Live Content Animation

Chat chooses animation mode with a content-keyed latch. `ThreadPane`
stamps `lastLiveContentAt` whenever smooth text-like live timeline content
advances: assistant prose, thinking, compaction reasoning, and direct text
patches. `MessageTimeline` returns `spring` for `SPRING_MODE_HOLD_MS` after
that stamp and `instant` otherwise.

The spring is keyed on content arrival, not provider turn state. It
therefore covers end-of-turn smoother drains and text-stream gaps, while
tool rows and late Streamdown typesetting on settled content sync-pin
invisibly by default. The 500ms hold is pure tuning — the historical
requirement that it outlast the spring sentinel retain duration died with
the descriptor gate (see Virtua Compensation Routing).

Structural transcript appends have a narrower override:
`markStructuralContentPending()` makes the next near-term command/tool row
growth spring-eligible even when the content latch currently returns
`instant`. This is intentionally one-shot. After the structural append
spring arrives it cancels instead of entering the streaming sentinel, so
routed virtua compensations are not declined after the append settles.

The effect that calls `markStructuralContentPending()` re-baselines its
active-turn tail signature — recording it without marking — across a thread
switch, a same-thread reload (`pane.switchGeneration` bump), and the initial
slice load (while `pane.loading` is true, which on a cache miss outlives the
generation bump). That signature embeds the thread id and tail row identity,
so a switch into an actively-streaming thread, and its async first slice,
both change it; treating either as an append would arm the structural-append
spring and make the post-restore measurement backlog a visible scroll. Only a
genuine append to the settled, mounted timeline reaches the mark.

`spring` is an eligibility signal, not an unconditional animation. If
`contentRO` observes a content-width change, the controller opens a short
width-reflow settle window and sync-pins paired height corrections. This
keeps pane, sidebar, and window reflows — including Mermaid `useMaxWidth`
height changes in virtua's rendered buffer — from producing a visible
half-viewport spring chase just because live content advanced recently.
During that window, the compensation resolver passes virtua's
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
notifying the scroll controller. Waiting for Svelte's microtask flush
would pin against stale layout. Idle composer geometry resolves to the
same same-paint pin as `notifyContentMaybeGrew()`; when live content is
inside the spring hold window, the live-capable hook keeps following the
moving bottom instead of sync-pinning mid-chase.

`overflow-anchor: none` belongs on the outer scroll container. Browser
scroll anchoring otherwise fights both virtua's jump correction and the
controller's sync-pin.

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

virtua row-size caches ARE replayed across thread switches, but only under
a strict validity key (`utils/threadVirtuaSizeCache.ts`). The `{#key
pane.threadId}` block remounts the `<Virtualizer>` on every switch, so
without a replay it re-runs the full estimate→measure cascade — the
thread-switch flicker, identical for cached and uncached threads because
the item cache avoids the *fetch*, not the virtua *remeasure*. Replaying
the previous visit's `CacheSnapshot` (`listRef.getCache()`) makes virtua
mount at the already-measured total: it computes total size from the
restored sizes on the first frame, and its resize handler no-ops every
re-measure that matches a restored size (verified against the installed
virtua 0.49.1 core — the action-3 handler filters out items whose new size
equals the cached size and bumps no state version). Zero re-render, zero
scroll jump.

The replay is sound only when the rows re-render at the heights they were
measured at, so a snapshot is refused (virtua falls back to the flat
`itemSize` estimate — the old behavior, never worse) unless three things
still match: **scroll-pane width** (the wrap point), a **structure
signature** (`timelineStructureSignature` over the rendered node sequence
plus per-leaf content — id, status, `summary.length`, `updatedAt`), and a
**non-default expansion signature** (`pane.expansionSignature()` over
expanded subagent groups, diff overrides, and payloads). A handful of global
display settings (`fontSize`, the sans/mono fonts, `collapseDiffPreviews`) also
change row height but are deliberately **not** keyed — a documented, benign
residual: toggling one mid-session then revisiting a thread replays stale
heights, which the warm-up gate masks as a cold first visit (the estimate→
measure cascade re-runs and corrects them), never a crash or stuck viewport.
Keying them would make the residual airtight but buys no visible change (same
masked cascade either way) at the cost of a drift-prone signature; the choice is
recorded in `threadVirtuaSizeCache.ts`. The structure signature superseded an
earlier version of this key that read `pane.timelineRevision` — a monotonic
per-pane counter that is never restored on a cache-hit re-entry, so every
revisit computed a strictly-greater revision than capture and the replay
**never matched** (the cache was inert; the switch-back flicker was never
actually fixed). `pane.timelineRevision` itself remains as the
timeline-derivation reactivity trigger; it was just the wrong input to key the
size replay on. The signature is reproducible instead: revisiting a settled
thread yields the identical string, and it is content-aware (Go bumps
`updatedAt` on every streaming append), so a backgrounded thread that
changed and got reloaded is refused on the key alone — eviction
(`thread.svelte.ts` removal/reswitch, `threads.svelte.ts removeThread`) is
memory housekeeping, not the correctness guard. Row-UI state is reset to
default on every switch (`rowUiState.clear()`), so at restore time the
expansion signature is the default one — which is exactly why a thread that
was idle-at-default replays cleanly and a thread that had something expanded
(taller rows) is correctly refused.

Captures must store the **settled** sizes or the replay restores a
mid-cascade height. They ride two triggers, both routed through one
size-gated persist (gated on `getScrollSize()` so a 60Hz spring does not
re-slice the array): the scroll-position snapshot (`saveScrollSnapshot`),
and the rising edge of `stick.isWarm` — the controller's
"measurement-cascade-settled" signal — which guarantees a final-height
capture for a thread the user views but never scrolls (the scroll triggers
alone can miss settle if the bottom-pin re-pins do not reach virtua's
`onscroll`). The store is a bounded session-only LRU (memory: ~one float
per loaded row per recent thread) — it does not persist to SQLite and does
not violate the visible-thread memory budget. This replays revisits within a
session; a genuine first visit has no snapshot and still cascades (hidden by
the warm-up gate, above).

There is deliberately NO per-row min-height floor system. An earlier
per-pane row-geometry reservation (row-key + signature + width height cache
applied as a temporary `min-height` on remount) was deleted post-`f42dc6e6`:
with the virtua manual-scroll marking patch and fractional height caching in
place, a capture experiment on the scroll-away/return remount path showed
floors-OFF outcome-identical to floors-ON (zero scrollHeight dips, zero
scrollTop reversals, identical unmount batches, clean bottom landings) — see
[`scroll-rearchitecture-plan.md`](scroll-rearchitecture-plan.md) §3.
Async-short remount content is bridged at the content layer (streamdown
mermaid/math rendered-height caches, the attachment blob cache), and
`remountReturn.browser.test.ts` pins the outcomes. Two pieces survive the
deletion:

- The scroll surface width signal: the **content-box** width, observed
  asynchronously through `observeScrollSurfaceContentWidth`
  (`scrollSurfaceWidth.ts`), feeds the virtua `CacheSnapshot` validity key —
  never a synchronous or border-box read (`getBoundingClientRect`,
  `clientWidth`). A second, disagreeing width source turns the width signal
  into a self-sustaining oscillation that re-renders every row at idle
  (CPU/heap-churn incident 2026-06-26, commit `a5a5d032`).
- Margin containment: the `[data-row-geometry-content]` row wrapper and the
  app.css `display: flow-root` rule (commit `4b3759a1`) — independent of the
  floors; see [`settle-flicker-analysis.md`](settle-flicker-analysis.md).

Rows inside the transcript should keep their shell stable after first
render. Add details inside reserved slots; do not append completion-only
history rows or late adornments that change row geometry.

## Search

Full-thread search goes through the `MessageSearch` palette and the
`SearchThreadMessages` binding. Browser find only sees mounted virtual
rows and is not a complete search surface.

A search hit calls `pane.requestScrollToItem(itemId)`. `MessageTimeline`
loads older rows until the item is present, then scrolls through
`stick.runExternalScroll(() => listRef.scrollToIndex(index, { align:
'center' }))`.

A hit inside a subagent transcript never appears in a history window
(windows hold top-level rows only). `loadUntilItem` walks the parent
chain to the launch root, slices the window around that root, hydrates
the subtree via `ListSubagentDescendants`, and the scroll resolves to
the containing `SubagentGroup` card.

## Discussion Mode

`ChannelView.svelte` shares `useStickToBottom` without a virtualizer. Its
content element wraps the channel-message list so the same ResizeObserver
path handles message growth and async Streamdown layout. Composer-section
resize must also notify the controller because discussion's composer is a
flex sibling that changes `scrollEl.clientHeight`.

## Diagnostics

Scroll bugs are usually second-order interactions between controller
flags, virtua measurement, and browser layout. Reproduce with
`make dev DEBUG=1`, then inspect `window.__agentOverflowUiTrace.dump()` or
`.filePath()`.

Useful trace records:

- `scroll.contentRO` — resize delta, width-reflow state, and pin decisions.
- `scroll.contentRO.widthReflow` — width-only content reflow that armed
  the short layout-correction window.
- `scroll.escape.set` — escape state changes.
- `scroll.refreshIsNearBottom` — geometric near-bottom changes.
- `chat.state` / `chat.dom` — MessageTimeline snapshots.
- `timeline.margin.diverge` — settle-flicker regression oracle. Fires when a
  row's bottom margin escapes its content box (virtua counts it in its measured
  total; the row's content-box observer does not), which used to drive a
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
`useStickToBottom.svelte.test.ts` or `scroll.test.ts`, then fix the
controller path.

## Accepted Tradeoffs

Nested row overflow is allowed for large subagent, wait-group, and command
output bodies. Focus can jump to `<body>` when virtua unmounts the focused
row. Shiki remains a frontend dependency through `svelte-streamdown` for
assistant markdown and a few payload expansions.
