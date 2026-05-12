# frontend/

Svelte 5 + Vite 8 (Rolldown) + Tailwind 4 + TypeScript.

## Commands

- `pnpm run check` — Svelte + TypeScript type check. Must pass.
- `pnpm run build` — production build. Must pass.
- `pnpm test` — Vitest unit tests.

## Layout

- `src/lib/stores/` — runes-based reactive stores. `thread.svelte.ts`
  owns the per-thread `ThreadPane` factory (items, payload meta,
  streaming, approvals, design artifacts, channel messages, token
  usage). `events.ts` declares custom event names. `bindings.ts` wraps
  the auto-generated Wails bindings.
- `src/lib/components/chat/` — timeline rendering. Kind-based
  discrimination; no role/content matching.
- `src/lib/components/composer/` — message composer, mode / effort /
  model pickers.
- `src/lib/components/sidebar/` — projects + thread list.
- `src/lib/components/primitives/` — reusable Menu / Popover / Modal /
  dropdown shells. Every picker in the composer toolbar and sidebar
  composes these rather than rolling its own positioning / focus-trap /
  keyboard handling.
- `src/lib/components/{design,discussion,git,palette,settings,terminal,shared}/` —
  per-feature component groups.
- `src/lib/types/` — shared TypeScript types.
- `src/lib/utils/` — pure helpers.
- `src/lib/transport/` — WebSocket client + the `@wailsio/runtime` shim
  the production build aliases the Wails generator's import to. Bindings
  end up calling `wsClient.ts` over WS in production. Don't import from
  here directly in feature code; go through `stores/bindings.ts`.
- `bindings/` — Wails-generated TypeScript. Never edit by hand.

## Responsibility boundary

- What BELONGS here:
  - UI rendering, routing between panes, user input capture.
  - Reactive state for the visible thread (items, approvals, streaming
    flags, token usage).
  - On-demand fetching of heavy payloads via bindings.
- What does NOT belong here:
  - Business decisions about turns, forks, approvals — those are
    decided in Go and surfaced via events or bindings.
  - Direct `window.runtime` calls — always go through the typed
    wrappers in `stores/bindings.ts`.
  - Parallel state slices for streaming. `ThreadPane` is the sole
    owner.

## State shape

- `ThreadPane` factory (in `stores/thread.svelte.ts`) owns all
  per-thread reactive state — items, payload meta, streaming,
  approvals, design artifacts, channel messages, token usage.
- Panes live in a registry; v1 runs a single main pane but the factory
  shape leaves room for tiling / multi-pane without a rewrite.
- The sidebar thread list is its own store — it doesn't hold pane
  state.

## Thread switch — cache + tail-only initial load

`pane.switchThread` is the entry point. The flow has four pieces:

- **`threadItemCache`** (`stores/threadItemCache.ts`) — bounded LRU
  (default cap 5) of `{ items, oldestLoadedTurnIndex, hasMoreHistory,
  latestSettledTurn }` snapshots keyed by thread id. The outgoing pane
  writes its current state on every `switchThread`; the incoming pane
  reads it and paints synchronously when present. Memory cost per
  snapshot is dominated by per-item `summary` and `payloadMeta` strings
  (unbounded provider text); a soft cap of `MAX_CACHED_SNAPSHOT_ITEMS`
  (1000 rows) on the write side rejects pathologically large snapshots
  where the cost-to-benefit inverts. Strings are reference-shared with
  the live pane until the user navigates away — once a snapshot is the
  sole root, GC reclaims it on eviction. Three eviction paths keep the
  cache fresh: (1) LRU drop when capacity is hit; (2) `events.ts`
  evicts every thread touched by an `applyItemUpserts` batch so a
  persisted mutation never reads stale; (3) `removeThread` evicts on
  delete so a deleted thread can't wedge a multi-MB snapshot.
  Same-thread re-switch (the revert-then-switchThread UX) skips the
  outgoing snapshot AND force-evicts the entry so the load fetches
  fresh state instead of flashing the stale view.
- **Single initial load.** `App.ListThreadSliceAround(threadID,
  anchorItemID, 50)` returns a viewport-sized slice (~50 items
  around the saved scroll anchor, or the tail when the anchor is empty
  / unknown). That's the only items fetch on switch. It runs
  concurrently with `SwitchThread`, `hydrateThreadLiveState`,
  `ListRecentTurns`, and `refreshCheckpointsForThread` under a single
  `Promise.allSettled` so the wall-clock cost of a switch is bounded
  by the slowest fetch, not their sum. On cache hit, the load is
  skipped entirely — the snapshot already covers the visible window
  and is invariant-fresh (events.ts evicts on every persisted
  mutation). The previous "Phase 2 wider window" load is gone:
  scrollHeight stays bounded to ~50 rows from frame 0, eliminating the
  applyJump per-row anchor preservation fight that produced visible
  scrollTop oscillation on cache miss.
- **Auto-load-older.** Older items page in lazily as the user scrolls
  toward the top. `MessageTimeline.maybeAutoLoadOlder` (driven from
  Virtualizer's `onscroll`) fires `pane.loadOlder()` when offset is
  within `AUTO_LOAD_OFFSET_PX=800` of the top AND the topmost
  rendered row is one of the first `AUTO_LOAD_INDEX_THRESHOLD=5`
  items. A `autoLoadAttemptedAtFloor` guard prevents hammering the
  same query while the user lingers near the top — cleared when
  `pane.oldestLoadedTurnIndex` advances or on thread switch. The
  manual "Load older messages" button (`MessageTimeline.svelte`) is
  the explicit fallback. `ListRecentThreadItems` is no longer used on
  switch; it survives only as the wider-window probe inside
  `pane.refreshFromBackend()` (transport-gap recovery).
- **`mergeMissingItemsById`** is the merge contract for both initial
  load and `loadOlder`: rows already in `pane.items` (from cache or
  streamed events that landed mid-load) keep their reference; missing
  rows are added and the array is re-sorted. Reference equality on
  unchanged rows keeps virtua's per-row ResizeObserver from firing
  spuriously, and triage's persist-then-emit ordering means any
  in-flight stream event is already baked into the slice's SQL —
  preferring the in-memory row over a re-fetch is always correct.
- **Spinner-flash gate.** `pane.loading` flips true the moment
  `switchThread` starts; `MessageTimeline` reads
  `pane.showLoadingSpinner` instead, which only resolves to true after
  `SPINNER_THRESHOLD_MS` (100ms — the Doherty perception threshold)
  AND when `pane.items.length === 0`. Cache hits never flash the
  spinner because items render immediately; sub-100ms cache misses
  skip it because the slice load populates items before the timer
  fires.

## Events in

- `app.Event.On('provider-event', ...)` — fan out to active panes.
- `app.Event.On('error', ...)` — toast + status bar.
- Custom event names per feature are defined in `stores/events.ts`.

## Scroll architecture

The MessageTimeline scroll surface is built on **`virtua/svelte`**'s
`<Virtualizer scrollRef={ourScrollEl}>`. Virtua owns geometry and per-row
anchor preservation (ResizeObserver + binary-searched jump-correction);
the frontend owns the scroll container itself, which is the outer
`<div class="overflow-y-auto">` in `MessageTimeline.svelte`. Owning the
container is what lets the controller observe content growth **before
paint** (single content-element ResizeObserver) and write `scrollTop`
synchronously in the same paint cycle — eliminating the rAF gap between
content layout and scroll correction that was the flicker source. The
frontend layers on top:

- **`useStickToBottom.svelte.ts`** — Svelte-5 port of stackblitz-labs'
  `use-stick-to-bottom`. Single owner of intent (`isAtBottom` flag) and
  the only writer to `scrollTop` outside virtua's internals. Two
  animation behaviors for autonomous content growth, selected per-fire
  by the consumer via the `animationMode` option on
  `createUseStickToBottomController({ animationMode })`:
  - **`'instant'` (default)**: sync-pin. ContentRO callback writes
    scrollTop synchronously in the same paint frame as contentEl
    growth — no perceptible scroll motion, content just arrives at
    the bottom. This is the default for Discussion's `ChannelView`
    (polled batches, no streaming chunks to chase) and for chat
    whenever a turn is NOT actively running.
  - **`'spring'`**: velocity-spring chase. The viewport interpolates
    toward the moving bottom across rAF ticks so the user sees a
    smooth scroll-follow. Used by chat MessageTimeline while a turn
    is in flight (`getActiveTurn(threadId) != null`) so streaming
    chunks flow in with the familiar "viewport follows the text" UX.
    Spring defaults to upstream's tuning (`damping: 0.7,
    stiffness: 0.05, mass: 1.25`); override via `{ spring: {...} }`.
  Both paths share a **warm-up gate** that defends against the
  e00723f regression (mount-time virtua remeasurement + Streamdown
  async typesetting would spring-chase a thread restore visibly).
  After `attach()`, `forceStick()`, or `armWarmup()`, the controller
  stays in sync-pin mode until either at least one contentRO event
  has fired AND `QUIET_MS = 100ms` of contentRO silence has passed
  (the common case — virtua's per-row ResizeObservers fire on row
  measurement, then go quiet) OR `FAILSAFE_MS = 2500ms` elapses (the
  worst case — joining a thread mid-stream where contentRO never
  goes quiet). The quiet timer is INTENTIONALLY gated on contentRO
  evidence — the original implementation armed it eagerly inside
  `beginWarmup`, which firing at QUIET_MS regardless of whether any
  cascade had started. On uncached re-entries to long threads, the
  slice fetch could exceed QUIET_MS while MessageTimeline rendered
  the loading-spinner branch (no contentEl, no RO); by the time
  items arrived and contentEl mounted, warm had already flipped via
  the empty quiet window, the hide-gate had reopened, and the
  measurement cascade was visible — landing scrollTop at the
  estimated bottom rather than the measured one (the user-visible
  "lands half-screen high" symptom). After warm, the
  selected `animationMode` takes effect. The warm state is exposed as
  `controller.isWarm` (state-backed) so consumers can co-gate UI on
  it; chat's MessageTimeline binds `visibility:hidden` to
  `!stick.isWarm && pane.cachedVirtuaCache === undefined` on
  contentEl, covering the uncached-load measurement cascade behind a
  brief blank rather than a visible "lands wrong, jumps to correct"
  sequence (the larger the thread, the larger the scrollTop clamp +
  row-offset shift — a 216-item sample produced a 461px jump).
  `armWarmup()` is the public re-arm hook for consumers that need
  `isWarm=false` BEFORE their next DOM flush — chat's MessageTimeline
  calls it from `$effect.pre` on thread switch because `forceStick()`
  in the restore `$effect` fires AFTER the DOM update (the new
  thread's first paint would otherwise inherit the prior thread's
  settled `isWarm=true`). `attach()` does not re-arm when scrollEl/
  contentEl are unchanged across a thread switch — MessageTimeline
  isn't keyed on threadId so those refs are stable, and `attach()`'s
  early-return path is hit on every switch.
  Programmatic scrolls go through `forceStick()` / `markAtBottom()` /
  `notifyContentMaybeGrew()` / `pauseAutoScroll()` / `stopScroll()` /
  `animateScrollTo()`; the one place virtua writes scrollTop is
  `listRef.scrollToIndex(...)`, which MUST be preceded by
  `stick.stopScroll()` so the controller doesn't auto-restick mid-jump
  if virtua's measurement loop happens to land near the bottom. Never
  write `scrollTop` directly.
  `prefers-reduced-motion: reduce` forces the sync-pin path
  unconditionally — the spring is suppressed regardless of
  `animationMode`.
  - `forceStick()` clears escape, sets sticky, and writes `scrollTop`
    to the current target. Used by the scroll-to-bottom chip, by
    Discussion's initial channel load, and by chat's bottom-snapshot
    restore on thread switch (paired with the per-thread virtua
    row-size cache, which makes the target correct from frame 0 — see
    below). Chat's bottom restore additionally schedules a single rAF
    `notifyContentMaybeGrew()` after `forceStick()` to catch late layout
    settling — composer-height RO updating scrollEl's padding-bottom
    (padding-only growth doesn't refire contentRO), virtua's per-row
    remeasurement after mount, and the first burst of Streamdown async
    typesetting can each shift the bottom by a few pixels one frame
    later. The trailing pin is escape-aware, so a user wheel-up between
    frames cancels it.
  - `markAtBottom()` flips the controller flags to sticky-bottom
    WITHOUT writing `scrollTop`. Used for the empty-timeline branch of
    bottom-snapshot restore (no rows to anchor against yet, but the
    first streamed row's contentRO sync-pin must land at the bottom).
    Don't pair with `listRef.scrollToIndex(last, 'end')` — that
    creates two writers (virtua's measurement loop + the controller's
    sync-pin) targeting slightly different scrollTop values for the
    same content-grow trigger, and they oscillate around the middle of
    the viewport.
  - `animateScrollTo(target, {durationMs})` runs an easeOutCubic
    interpolation for arbitrary timeline jumps (load-older, scroll-to-
    item). Owns the scrollTop writes so programmatic-write tagging
    stays in one place.
- **`pane.scrollController`** — registration slot. Both
  `MessageTimeline.svelte` (chat) and `ChannelView.svelte` (Discussion)
  publish their `useStickToBottom` controller on mount; external
  surfaces (sidebar resizers, resizable drawers) acquire
  `pauseAutoScroll()` during their drag to keep auto-follow from
  yanking the user mid-gesture. The lease is depth-counted and
  idempotent. The pane only knows the minimal `PaneScrollController`
  interface (`pauseAutoScroll(): () => void` +
  `notifyContentMaybeGrew(): void`), so a single set of resizer/drawer
  hooks works on both surfaces.
- **`threadScrollSnapshots.ts`** — per-thread LRU of
  `{kind:'bottom'} | {kind:'anchor', itemId, offsetTop}`. Snapshots are
  semantic (item id + offset), not virtua's internal cache shape, so
  they survive virtua version bumps.
- **Per-thread virtua row-size cache.** `threadItemCache.ts`'s
  `ThreadItemSnapshot` carries an optional `virtuaCache` field
  (virtua's `CacheSnapshot`). On `switchThread`, the pane calls a
  getter registered by `MessageTimeline` (matched-pair
  `pane.attachVirtuaCacheGetter(getter)` /
  `pane.detachVirtuaCacheGetter(getter)`, symmetric with the scroll
  controller pair) that returns `listRef.getCache()` — captured
  synchronously while the OLD virtualizer is still mounted. On
  switch-back, the snapshot surfaces as `pane.cachedVirtuaCache` and
  the timeline passes it to `<Virtualizer cache={...}>`. The
  Virtualizer is wrapped in `{#key pane.threadId}` so this prop is
  re-read at mount (virtua's `cache` is consumed once at
  `createVirtualStore(...)`, not reactively). Without the cache,
  virtua's lazy mount-time measurement underestimates `totalSize` at
  `ESTIMATED_ROW_SIZE × N`
  until per-row ResizeObservers fire, and a `{kind:'bottom'}`
  restoration lands above the eventual bottom — the controller would
  then absorb the gap through repeated sync re-pins as rows grew.
  ChannelView (no virtualizer) leaves the getter unregistered.
- **Negative-delta re-pin honors logical intent, not just geometry.**
  The controller's contentRO re-stick on a shrink fires when EITHER
  `isAtBottomState` (logical intent) OR `isNearBottomState` (geometric
  ≤70 px) is true — gated by `!escapedFromLockState && pauseDepth ===
  0` in both cases. The geometric branch matches the upstream
  use-stick-to-bottom semantics. The intent branch defends against
  virtua's row-remeasurement cascade: when rows above the viewport
  remeasure during the warm-up window (per-row ResizeObservers fire
  after mount, Streamdown async typesetting growing math/code blocks
  above the viewport), virtua's binary-searched `applyJump` shifts
  `scrollTop` by the same delta to preserve the visible row. On
  uncached loads that shift can be hundreds of pixels, flipping the
  geometric near-bottom check to false purely as a downstream effect
  of layout — not user intent. Without the disjunct, the controller
  abandoned the pin and left the viewport stuck mid-cascade until
  some later shrink happened to land scrollTop at the new bottom by
  coincidence (user-visible "half-screen jump" on heavy uncached
  threads). Regression coverage lives in
  `useStickToBottom.svelte.test.ts` under "content ResizeObserver"
  (the disjunction + the escape and pause guards that must still
  override it).
- **Layout decoupling** — `ChatView.svelte` positions the composer +
  live-turn UI + below-bar as an absolute overlay inside the timeline's
  relative container. A `--composer-height` CSS variable, written by a
  ResizeObserver on the overlay, drives the timeline's `padding-bottom`
  on `scrollEl` so composer growth, working/todo panels, attachment
  trays, and approval panels never alter the scroll surface's
  `clientHeight`. The padding lives on scrollEl (not contentEl) because
  the controller's contentRO defaults to observing the content-box —
  per W3C ResizeObserver spec, padding-only changes neither fire the
  callback nor change `entry.contentRect.height`, so a contentEl
  padding wouldn't re-pin via the contentRO seam. ChatView's composer
  RO calls `notifyContentMaybeGrew()` to stamp the resize and re-pin
  scrollTop after the padding update flows through. The re-pin runs
  **synchronously inside the RO callback** — the callback writes
  `--composer-height` directly on chatColumn via
  `style.setProperty(...)` (bypassing Svelte's reactive flush
  microtask, which would otherwise leave the layout-relevant change
  un-applied until after the callback returned), then
  `notifyContentMaybeGrew()` forces a layout read inside
  `targetScrollTop()` so the post-grow scrollHeight is what
  scrollTop is pinned against. The previous rAF-deferred path left a
  1-frame gap between composer growth and re-pin — visible as content
  "appearing then settling" on uncached loads where a working/todo or
  approval panel mounts late, after the warm gate has already revealed
  contentEl; the gap is `composerDelta` pixels, large enough on big-
  composer threads (~200–400 px) to flicker the scroll-to-bottom chip
  on the way to settling. The reactive `composerHeight` state binding
  on chatColumn stays in place for any future consumer of the value;
  Svelte's microtask flush writes the same CSS-variable value a second
  time, which is idempotent. ChannelView's composer-section RO calls
  the same `notifyContentMaybeGrew` hook for an analogous reason: in
  Discussion the composer sits OUTSIDE scrollEl (different layout —
  flex sibling, not absolute overlay), so composer growth there changes
  `scrollEl.clientHeight` rather than `scrollHeight`, and the contentRO
  also doesn't see it.
- **`overflow-anchor: none` on `scrollEl`.** Both MessageTimeline and
  ChannelView set `overflow-anchor: none` on the scroll container. The
  browser's default scroll-anchor heuristic adjusts `scrollTop` when
  content above the viewport changes size — well-intentioned for static
  documents, but it actively fights virtua's measurement-loop jump
  correction AND the controller's contentRO sync-pin. Streamdown async
  typesetting (shiki / KaTeX / mermaid) growing rows above the viewport
  on a sticky session would produce visible scrollTop oscillation
  between the browser's anchor adjustment and our re-pin without this
  opt-out. virtua already sets `overflow-anchor: none` on its inner
  container, so the controller doesn't need a defensive copy on
  contentEl — only the outer scrollEl needs the opt-out.
- **Reserved-slot banners** — `ProviderStatusBanner.svelte` and
  `TransportStatusBanner.svelte` both use `min-h-N` wrappers +
  `transition:fade` so banner mount/unmount does not animate adjacent
  height. Cost: ~100px of always-reserved chrome across the two
  surfaces; banners appear in a stable location and never push the
  scroll viewport.
- **Row state survives virtua remount via pane-level registries.**
  Expansion state for tool-call payloads, attachment-blob URLs, and
  subagent-group expanded flag all live on the `ThreadPane` keyed by
  `item.id` / `payloadId`. Row components read the handle out of the
  pane on each mount (using `untrack` so reads don't bind the row to
  its initial value). This means scrolling a row past the
  `bufferSize=900` window and back preserves "show full output"
  toggles, loaded payload chunks, and any image blobs. Registries are
  cleared on `switchThread` to bound memory.
- **Expansion-state memory tradeoff.** The expansion registry keeps
  loaded payload chunks until the user collapses a row or switches
  thread. Open transcript rows are user-owned UI state; collapsing one
  from an unrelated row's load changes timeline height outside the
  user's interaction path and fights virtua.
- **Stable transcript rows.** Anything rendered inside `<Virtualizer>`
  is a stable history record. A row may update text/content in place, but it
  should not change its outer shell after first render: no static
  div-to-button swaps, no late chevron insertion, no completion-time
  summary cards appended inside history, and no live working/todo UI in
  the virtualized data. New transcript structures must decide their
  shell from provider metadata available at first render and keep later
  details inside reserved slots. Disclosure-style rows should compose
  `TranscriptDisclosureHeader.svelte` so toggle chrome and trailing
  actions keep the same DOM shape across loading/completion updates.
- **Shiki diff token cache.** `tokenCache.ts` partitions cached lines
  by `${theme}:${threadId}:${lang}:…` so a thread switch can drop
  every line tokenized under the outgoing thread without disturbing
  any other thread's tokens. `pane.switchThread` calls
  `clearTokensForThread(prevThreadId)` exactly once per switch; the
  partition + clear-on-switch is what bounds long-session memory.
  The fixed-cap LRU (5000 entries, ~5 MB worst case) only exists to
  absorb repeat-visit pressure within a single thread; it's
  deliberately large enough that a multi-thousand-line diff doesn't
  self-evict during initial render.

`ChannelView.svelte` (Discussion mode) shares the same
`useStickToBottom` controller. It scrolls a plain DOM container with no
virtualizer, but the controller's content-element ResizeObserver is
agnostic to what's inside contentEl — so the same sync-pin handles
streaming-driven growth on both surfaces (Discussion's
`svelte-streamdown` async typesetting passes count as positive
contentRO deltas just like virtua row remeasurement). Discussion's
contentEl wraps the `{#each}` over channel messages; the scroll
element is the surrounding `overflow-y-auto` div. The intervening
`<div bind:this={contentEl} class="space-y-3">` is intentional — it
gives the content-RO a target whose height tracks message-list growth
without including the scroll container's padding, mirroring chat's
`<div bind:this={contentEl}>` wrapper around the `<Virtualizer>`.

What NOT to add:
- Manual `scrollTop` writes outside the controller.
- A row-height signature cache. virtua re-measures via ResizeObserver.
- A scroll-anchor compensation pass on top of virtua's jump algorithm.
- A second virtualizer over the same data.
- A length-watching auto-follow `$effect` that calls
  `listRef.scrollToIndex(last, 'end')` on streaming. The content-RO inside
  `useStickToBottom` reproduces this synchronously before paint; a
  duplicate effect re-introduces the rAF gap that this architecture
  eliminated.
- `smooth: true` on any `listRef.scrollToIndex(...)` call. Virtua's
  smooth path uses the native `scrollTo({behavior:'smooth'})` which
  fires its own scroll events asynchronously and would race the
  controller's auto-restick / programmatic-write tagging. Always pair
  `scrollToIndex` with a preceding `stick.stopScroll()` and let virtua
  jump synchronously.
- `transition:slide` adjacent to the scroll area — animated height
  shifts visible content under the user's cursor.
- Late transcript adornments on completion. If the UI needs a marker,
  attach it to the row boundary when that row first appears; don't add
  a separate end-of-turn row after the virtualizer has measured the
  previous bottom.

## Search

- Full-thread message search uses the in-app `MessageSearch` palette
  (Ctrl/Cmd+F, see `palette/MessageSearch.svelte`). The query goes
  through the `SearchThreadMessages` Wails binding which reads SQLite
  directly — coverage is independent of which rows are currently
  mounted in virtua. This is the canonical search surface, not the
  browser-native find which only sees mounted rows.
- A search hit calls `pane.requestScrollToItem(itemId)`;
  `MessageTimeline` reacts by paging older items in via
  `pane.loadUntilItem(id)` and then, after `stick.stopScroll()` +
  `stick.setEscapedFromLock(true)`, `listRef.scrollToIndex(idx, { align:
  'center' })`. The two-step (load-then-scroll) is necessary because
  virtua only knows about items present in `pane.items`. Never pass
  `smooth: true` — virtua's smooth path uses
  `scrollEl.scrollTo({behavior:'smooth'})` which fires its own scroll
  events asynchronously and races the controller; if smooth-to-hit is
  wanted later, route through a controller-owned animateScrollTo call
  with the per-row offset computed from `listRef.getItemOffset(idx)`.

## Accepted scroll-surface tradeoffs

- **SubagentGroup inner overflow.** The expanded subagent body uses an
  internal `max-h-[20rem] overflow-y-auto` instead of nesting a
  virtualizer. Children are rendered eagerly when expanded, so a
  subagent with 200 children pays full DOM cost on expand. In
  practice subagents top out around 50 children (~100 KB DOM); the
  dense overview UX wins over micro-optimizing a worst case we don't
  see. Revisit only if a real thread shows DOM cost from a
  200+-child subagent.
- **Focus survival across virtua remount.** When the focused element
  belongs to a row that scrolls past `bufferSize=900` and unmounts,
  focus jumps to `<body>`. Tab through a long virtualized timeline
  is therefore fragile. Industry chat surfaces (Slack, Discord, VS
  Code chat) accept the same tradeoff. Revisit only if user
  feedback surfaces real keyboard-navigation pain.
- **`shiki` is still a dependency.** The Go-side SSR plan moved diff
  highlighting off the client, but `svelte-streamdown` ships a
  `HighlighterManager` that dynamically loads shiki for code blocks
  inside assistant markdown (and a small set of payload expansions).
  Module-level caches inside the library keep per-row remount cheap.
- **Click-anchor preservation and pointerdown-defers-forceStick are
  deliberately NOT implemented in `useStickToBottom`.** The legacy
  Discussion controller had both: clicking a `<details>` / `<button>`
  inside a message would adjust `scrollTop` to keep the clicked element
  fixed in viewport, and a `forceStick` while the user was mid-drag of
  the scrollbar would defer until pointerup. Neither is reproduced in
  the unified controller: chat's transcript rows don't expand-collapse
  in place (every disclosure has a stable header that's part of the
  row's first-paint shell), and Discussion message bodies are plain
  Markdown without expandable affordances. The pointerdown-defer is a
  rare-input-mode case (mouse-drag of scrollbar + concurrent post)
  with no recorded user impact in the chat surface; treat as an
  accepted simplification for the unification.

## Diagnosing scroll regressions

The scroll controller is the hardest surface in the frontend to
reason about by reading code alone — three independent layers
(controller flags, virtua's measurement loop, browser layout) write
`scrollTop` near each other, and the user-visible symptom is usually
the second-order effect of one layer's correction interacting with
another's. The recurring class of bug looks like "viewport lands
slightly off, then snaps to where it should be" or "half-screen jump
on uncached load" — both produced by a controller decision made
against state that another layer was about to change.

The `uiRenderTrace.ts` surface exists for exactly these bugs. Enable
it with `make dev DEBUG=1` (sets `VITE_AGENT_OVERFLOW_UI_TRACE=1`).
Trace records are written to disk via `AppendUIRenderTraceBatch` and
also exposed through `window.__agentOverflowUiTrace` in the dev
console (`.dump()` / `.recent(50)` / `.filePath()`). The scroll
controller records around every decision point: `scroll.contentRO`
(every resize delta with `positiveWillPin` / `negativeWillPin` /
`isAtBottomState` / `isNearBottomState` / `escapedFromLockState` /
`pauseDepth` snapshot), `scroll.escape.set` (when escape flips),
`scroll.refreshIsNearBottom` (when the geometric flag changes),
plus `chat.state` / `chat.dom` snapshot traces from MessageTimeline.

When a regression is reported, the diagnostic flow is:

1. Reproduce with the trace enabled. Capture the dump immediately
   after the symptom — `window.__agentOverflowUiTrace.dump()` or
   the file path. Threads with multi-hundred items + heavy
   Streamdown content (math, code, mermaid) are the most reliable
   reproducers.
2. Scan backward from the user-visible symptom for the LAST
   `scroll.contentRO` record before the viewport landed wrong.
   Read `positiveWillPin` / `negativeWillPin` directly. A pin that
   should have fired but shows `false` means one of the gates
   blocked it; cross-reference the surrounding flags.
3. The recurring failure mode: `isAtBottomState=true,
   isNearBottomState=false, negativeWillPin=false` — a geometric
   flag flickered because of an upstream layout correction
   (virtua's `applyJump`, browser scroll-anchor, composer-height
   pad), the controller read the flicker, and abandoned the pin.
   The fix in this codebase has always been to add the
   intent-disjunct: gate on `(isAtBottomState ||
   isNearBottomState)`, with `!escapedFromLockState &&
   pauseDepth === 0` outside the disjunct. Pre-existing
   regression tests for that pattern are in
   `useStickToBottom.svelte.test.ts` under "content ResizeObserver".
4. Reproduce in a unit test before touching controller code. Each
   regression in this surface has corresponded to a specific
   contentRO firing sequence — encode it in
   `useStickToBottom.svelte.test.ts` so the assertion fails
   without the fix and passes with it. The test file's
   `MockResizeObserver` + `stubGeometry` helpers cover the
   geometry combinations that matter.

What NOT to do when chasing a scroll bug:

- Don't add a defensive `requestAnimationFrame` to "let layout
  settle." The whole point of the synchronous contentRO seam is to
  pin scrollTop in the same paint frame as the height change;
  deferring re-introduces the gap the architecture eliminated.
- Don't add a second observer or `$effect` watching `pane.items`
  / `scrollHeight` / `scrollTop`. The controller already owns
  these decisions. A parallel watcher creates two writers that
  race.
- Don't relax the warm-gate or hide-gate without evidence that
  the cascade-cover is unnecessary on the affected thread —
  uncached loads of 200+-item threads with heavy Streamdown
  content are the canonical stressor and the cascade is real.

## Raw-content rendering

Raw content is canonical. Go sends raw item summaries, channel message
content, and payload data; the frontend owns rendering as a viewport-local
projection.

Assistant text, discussion messages, and proposed plans render through
`ChatMarkdown.svelte`, which mounts a `<Streamdown>` (`svelte-streamdown`)
with our own thin host wrappers (`StreamdownCodeHost`, `StreamdownMermaidHost`,
`StreamdownMathHost`) that re-stamp the original source on `data-code-source`,
`data-mermaid-source`, and `data-math-source` so `markdownSerialize.ts`'s
copy-as-markdown round-trip keeps working. Streamdown owns markdown
parsing (via `marked`), shiki highlighting, KaTeX typesetting, mermaid
rendering, link/image URL prefix safety, and graceful incomplete-token
auto-close while streaming (`parseIncompleteMarkdown={streaming}`).
The library uses a token-keyed `{#each}` over marked blocks under the
hood, so DOM identity is preserved across content updates — text
selection, scroll-within-code, and previously-rendered shiki/mermaid
nodes all survive streaming chunks. Two custom post-process passes
remain in `markdownEnhance.ts`: project-relative path linkification
(`enhancePathLinks`) and the document-level markdown-aware copy
delegate (`ensureMarkdownCopyDelegate`).

ANSI-like payloads render through `AnsiText.svelte`, which builds an
HTML string from raw bytes and applies it to a stable `<pre>` via
`Idiomorph.morph(...)`. Idiomorph diffs the live DOM against the new
HTML and patches only changed nodes — text selection survives streaming
chunks, no per-line re-tokenization on each update.

Do not add a server-rendered chat HTML field or a global DOM observer.
Copy/download paths read raw `summary` / `content` / `data`.

## Extension points

- To add a new event kind rendered by chat: add a kind constant in
  `stores/events.ts`, a renderer in `components/chat/`, and a
  `ThreadPane` reducer branch. See
  `docs/architecture/how-to.md#add-a-new-event-kind`.
- To add a composer mode / picker: compose the primitives under
  `components/primitives/`; don't roll custom positioning.
- To regenerate Wails bindings: run `wails3 task common:generate:bindings`,
  which passes `-ts`. Never edit files in `bindings/` by hand.

## Anti-patterns

- Do NOT create legacy stores. Runes only — `$state`, `$derived`,
  `$effect`, `$props`. No `export let`, no `$:`.
- Do NOT maintain a parallel state slice for streaming next to the
  persisted timeline. One owner per pane.
- Do NOT discriminate timeline items by role or content substring.
  Discriminate via `kind`.
- Do NOT re-order items per render. Upsert by `(turnIndex, itemIndex)`
  and let the store stay sorted.
- Do NOT implement count-based slicing for virtualization (forge's
  `useDeferredValue` ping-pong, count-window approaches). Heavy
  content is on-demand — expand-to-load, not preload.
- Do NOT stretch a `.svelte` file past ~300 lines. Extract instead.
- Do NOT add business logic to templates. Derive in `<script>`, render
  in the template.
- Do NOT call `window.runtime` directly. Use `stores/bindings.ts`.
- Do NOT preload heavy content. Diffs, command output, thinking —
  fetch via bindings when the user expands.

## Testing

- Store logic: unit-test with Vitest under `src/lib/stores/*.test.ts`.
- Component rendering: coverage is thin; when you add or change
  behavior, add a component test that would fail without the change.
- A failing `pnpm run check` is a blocker, not a warning.

## References

- Forge web app: `/Users/randy/repos/forge/apps/web/src/` — UX
  reference for ambiguous decisions.
- `docs/references/spike-policy.md` — when Wails binding behavior is
  unclear.
- Root `CLAUDE.md` principle 4 ("Frontend memory is bounded by the
  visible thread").
