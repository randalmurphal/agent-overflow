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

The store exposes the direction as `pane.pendingTimelineShiftAtHead`, bound
to `<Virtualizer shift={...}>`, set synchronously at the `items` mutation
so virtua reads it in the same flush and reset in the paging method's
`finally`. The grow (prepend/append) and the prune are deliberately split
across **two flushes** (`await tick()` between them): coalesced, a
head-grow plus a tail-shrink collapse into one net length change that a
single `shift` boolean cannot describe — and when the page budget equals
the prune count the net length is unchanged, so virtua dispatches nothing
at all and the cache scrambles. This was confirmed by driving the installed
virtua core directly; the cache semantics are codified in
`virtuaShiftCache.test.ts` as a version-bump tripwire, and the store's
two-flush sequencing + shift direction are covered in
`thread.svelte.test.ts`.

Only `loadOlder` / `loadNewer` use `shift`; they apply the paired prune
directly (the dropped end is always opposite the reading viewport, so there
is nothing to veto or restore). The streaming / settle prune keeps the
explicit anchor transaction (`preserveTimelineWindowAnchor`, below) — it
fires under a bottom-pinned viewport mid-turn where the incident-hardened
defer-and-restore behavior matters — and leaves `pendingTimelineShiftAtHead`
false.

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
- `animateScrollTo()` for controller-owned arbitrary jumps.
- `runExternalScroll()` for virtua `scrollToIndex` calls.
- `pauseAutoScroll()` for drag/resize leases.

Never write `scrollTop` directly from feature code. Never pass
`smooth: true` to virtua `scrollToIndex`; native smooth scrolling emits
asynchronous scroll events that race the controller's tagging.

`prefers-reduced-motion: reduce` forces sync-pin behavior regardless of
requested animation mode.

## Virtua Write Gate

Virtua's internal `$fixScrollJump` can write `scrollTop` while row
measurements settle. The controller installs a descriptor gate on
`scrollEl.scrollTop` so controller-owned writes pass through and external
writes are evaluated against intent.

The gate drops virtua writes only when all of these are true:

- warm-up is complete,
- the controller is logically at bottom,
- the user has not escaped,
- no pause lease is active,
- animation mode is `spring`, or a one-shot structural append spring is in
  flight,
- a spring chase is in flight.

This is intentionally narrow. Pre-warm writes are hidden, instant-mode
writes target the same sync-pin destination, and no-spring writes have
nothing to protect. If code changes any gate condition, run the
`useStickToBottom.svelte.test.ts` blocks for external write gates,
spring sentinel lifetime, pause-depth races, and gate coupling.

## Live Content Animation

Chat chooses animation mode with a content-keyed latch. `ThreadPane`
stamps `lastLiveContentAt` whenever smooth text-like live timeline content
advances: assistant prose, thinking, compaction reasoning, and direct text
patches. `MessageTimeline` returns `spring` for `SPRING_MODE_HOLD_MS` after
that stamp and `instant` otherwise.

The spring is keyed on content arrival, not provider turn state. It
therefore covers end-of-turn smoother drains and text-stream gaps, while
tool rows and late Streamdown typesetting on settled content sync-pin
invisibly by default.
`SPRING_MODE_HOLD_MS` must remain greater than the spring sentinel
retain duration.

Structural transcript appends have a narrower override:
`markStructuralContentPending()` makes the next near-term command/tool row
growth spring-eligible even when the content latch currently returns
`instant`. This is intentionally one-shot. After the structural append
spring arrives it cancels instead of entering the streaming sentinel, so
virtua/browser `scrollTop` corrections are not suppressed after the append
settles.

`spring` is an eligibility signal, not an unconditional animation. If
`contentRO` observes a content-width change, the controller opens a short
width-reflow settle window and sync-pins paired height corrections. This
keeps pane, sidebar, and window reflows — including Mermaid `useMaxWidth`
height changes in virtua's rendered buffer — from producing a visible
half-viewport spring chase just because live content advanced recently.
During that window, virtua's anchor-preserving `scrollTop` writes pass
through the external-write gate for the same reason.

Negative content deltas usually sync-pin when the user intends to stick,
but a small negative correction during an active spring is absorbed by
the spring so estimate/correct row measurement pairs do not snap the
viewport. Width-driven shrink corrections and large overshoots still
snap immediately.

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

Do not replay virtua row-size caches across thread switches. Measurements
depend on expanded payloads, loaded thumbnails, and row-local layout
state; the UI state is deliberately cleared on switch to bound memory.

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

Work backward from the visible symptom to the last relevant
`scroll.contentRO`. If the user intended to stick and
`negativeWillPin=false`, check whether the gate should use logical intent
(`isAtBottomState`) as well as geometry (`isNearBottomState`).

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
