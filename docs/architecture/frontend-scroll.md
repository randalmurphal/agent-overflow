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
the manual "Load older messages" button as the explicit fallback.

`threadItemCache.ts` is a small LRU of visible-window snapshots, not a
full-history cache. It rejects oversized snapshots, evicts inactive
threads touched by persisted mutations, and force-evicts same-thread
reloads so revert/reload flows do not paint stale rows.

`mergeMissingItemsById` is the merge contract for initial load and older
paging. Existing in-memory rows keep their references; missing rows are
added and the result is sorted. This preserves virtua row identity while
remaining correct under persist-then-emit ordering.

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
- animation mode is `spring`,
- a spring chase is in flight.

This is intentionally narrow. Pre-warm writes are hidden, instant-mode
writes target the same sync-pin destination, and no-spring writes have
nothing to protect. If code changes any gate condition, run the
`useStickToBottom.svelte.test.ts` blocks for external write gates,
spring sentinel lifetime, pause-depth races, and gate coupling.

## Live Content Animation

Chat chooses animation mode with a content-keyed latch. `ThreadPane`
stamps `lastLiveContentAt` whenever genuine live timeline content
advances; `MessageTimeline` returns `spring` for
`SPRING_MODE_HOLD_MS` after that stamp and `instant` otherwise.

The spring is keyed on content arrival, not provider turn state. It
therefore covers end-of-turn smoother drains and wire-round gaps, while
late Streamdown typesetting on settled content sync-pins invisibly.
`SPRING_MODE_HOLD_MS` must remain greater than the spring sentinel
retain duration.

Negative content deltas usually sync-pin when the user intends to stick,
but a small negative correction during an active spring is absorbed by
the spring so estimate/correct row measurement pairs do not snap the
viewport. Large overshoots still snap immediately.

## Layout Rules

`ChatView.svelte` positions the composer and live-turn UI as an absolute
overlay. A `--composer-height` CSS variable drives `scrollEl`
`padding-bottom`, keeping composer growth from changing the scroll
surface's `clientHeight`.

The composer ResizeObserver writes `--composer-height` directly before
calling `notifyContentMaybeGrew()`. Waiting for Svelte's microtask flush
would pin against stale layout.

`overflow-anchor: none` belongs on the outer scroll container. Browser
scroll anchoring otherwise fights both virtua's jump correction and the
controller's sync-pin.

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

- `scroll.contentRO` — resize delta and pin decisions.
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
