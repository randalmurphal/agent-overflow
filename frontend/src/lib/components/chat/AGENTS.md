# components/chat/

Chat-surface components. The owning module is `MessageTimeline.svelte`.

## Scroll contract

See `frontend/AGENTS.md` § Scroll architecture for the high-level
shape. Operational rules for code in this directory:

- Use `listRef.findItemIndex(offset)` / `listRef.getItemOffset(index)` /
  `listRef.scrollToIndex(...)` for anything that needs to know "where am
  I in the timeline" or "go to row X". `listRef` is now a
  `VirtualizerHandle` (we use `<Virtualizer>` with our own `scrollRef`
  rather than `<VList>` which would own the scroller). Don't query the
  DOM for first-visible-item or write `scrollTop` directly.
- Programmatic scrolls go through `useStickToBottom` (`forceStick`,
  `markAtBottom`, `notifyContentMaybeGrew`, `pauseAutoScroll`,
  `stopScroll`) or directly via `listRef.scrollToIndex(...)`. **Always
  call `stick.stopScroll()` BEFORE any `listRef.scrollToIndex(...)` and
  never pass `smooth: true`** — virtua's smooth path uses
  `scrollEl.scrollTo({behavior:'smooth'})` natively, which would fight
  the controller. Never `el.scrollIntoView()` on a row that lives
  inside the virtualizer; virtua won't see it and will fight the
  scroll.
- **Bottom-snapshot restore on thread switch goes through
  `stick.forceStick()` synchronously inside the restore `$effect`,
  followed by a single rAF `notifyContentMaybeGrew()` settle pass.**
  The synchronous forceStick is the primary writer — running it inline
  (no `pauseAutoScroll` lease, no `await tick()`) keeps the
  contentRO sync-pin enabled across the new-thread mount and avoids
  the microtask boundary that races with virtua's deferred scroller
  attach (`Virtualizer.svelte`'s `tick().then(observe)` defers
  scrollEl observation by one tick). The trailing rAF
  `notifyContentMaybeGrew()` covers late layout settling: composer-
  height RO updating scrollEl's `padding-bottom` (padding-only growth
  doesn't refire contentRO), virtua's per-row ResizeObservers refining
  sizes one frame after mount, and the first burst of Streamdown async
  typesetting (shiki / KaTeX / mermaid / parseIncompleteMarkdown
  rebalance). The trailing pin is escape-aware (`notifyContentMaybeGrew`
  bails on `escapedFromLockState || pauseDepth>0 || !isAtBottomState`)
  so a user wheel-up between frames cancels it; thread-switch is also
  guarded explicitly via a captured-`restoredThreadId` check inside
  the rAF. The per-thread virtua row-size cache (replayed via
  `<Virtualizer cache={pane.cachedVirtuaCache}>` inside
  `{#key pane.threadId}`) gives virtua the correct `totalSize` from
  frame 0, so the synchronous `forceStick` target is right at first
  paint. Subsequent contentEl growth from streaming or further
  typesetting gets handled invisibly by the controller's contentRO
  sync-pin: each positive delta re-pins to the new bottom inside the
  RO callback, before paint. **Don't pair `listRef.scrollToIndex(last,
  'end')` with `stick.markAtBottom()` here** — virtua's measurement
  loop would keep writing scrollTop on every ACTION_ITEM_RESIZE tick
  for ~150ms while the controller's sync-pin (enabled by markAtBottom)
  ALSO wrote scrollTop on every positive contentRO delta, targeting a
  slightly different value. They oscillate visibly around the middle
  of the viewport on every Streamdown async typesetting tick. Single
  synchronous writer (forceStick) plus a deferred escape-aware re-pin
  (notifyContentMaybeGrew) — never the two-loop fight.
- **`overflow-anchor: none` on `scrollEl` is load-bearing.** Browser
  default scroll anchoring adjusts `scrollTop` whenever an element above
  the viewport changes size, to keep the topmost-visible element fixed.
  That heuristic fights both virtua's measurement-loop jump correction
  AND the controller's contentRO sync-pin — Streamdown async typesetting
  growing rows above the viewport on a sticky session would produce
  visible scrollTop oscillation between the browser's anchor adjustment
  and our re-pin. virtua already sets `overflow-anchor: none` on its
  own container, so a defensive copy on contentEl is redundant; only
  the outer scrollEl needs the opt-out.
- **`padding-bottom` for composer clearance stays on `scrollEl`, not
  on `contentEl`.** Tempting to move it to contentEl so the controller's
  contentRO catches `--composer-height` changes natively — but the
  controller observes content-box (W3C ResizeObserver default) and
  `entry.contentRect.height` reports content-box even when the callback
  does fire. Padding-only changes to contentEl would not show up as a
  positive delta and the sync-pin would short-circuit. ChatView's
  composer RO calls `notifyContentMaybeGrew()` to handle this seam
  explicitly — that call is the load-bearing pin for composer-height-
  driven re-pinning on the chat surface; don't drop it.
- **`<Virtualizer>` is wrapped in `{#key pane.threadId}`** so its
  `cache` prop is re-read on thread switch. The cache itself comes
  from `pane.cachedVirtuaCache` (sourced from the LRU snapshot in
  `threadItemCache.ts`); MessageTimeline registers the capture getter
  via the matched-pair `pane.attachVirtuaCacheGetter(getter)` on
  mount and `pane.detachVirtuaCacheGetter(getter)` on destroy
  (symmetric with `pane.attachScrollController` /
  `pane.detachScrollController` — the same getter reference must be
  passed to detach so a stale teardown can't dispose a freshly
  remounted timeline's getter during fast switches). Without the
  `{#key}` the cache prop silently goes stale on thread switch
  (`createVirtualStore` reads it once); without the getter the LRU
  snapshot has no `virtuaCache` field and a re-entered thread eats
  the underestimate-then-grow pass at first paint.
- Don't add a parallel virtualizer over `pane.items` or `groupedNodes`.
- The auto-follow `$effect` is gone. Streaming flow is: text rewrites in
  the streaming row → row's height changes → virtua's per-row RO bumps
  `totalSize` → `contentEl.scrollHeight` changes → our content-RO fires
  before paint → controller sync-pins scrollTop to the new target in
  the same paint. Don't reintroduce a length-watching effect that
  calls `scrollToIndex(last)`.
- **Auto-load-older trigger.** Virtualizer's `onscroll` calls
  `maybeAutoLoadOlder(offset)` which fires `pane.loadOlder()` when the
  user scrolls within `AUTO_LOAD_OFFSET_PX=800` of the top AND the
  topmost rendered row index is `<= AUTO_LOAD_INDEX_THRESHOLD=5`. Both
  gates matter: the offset gate keeps fast scrolls past the trigger
  zone from oversubscribing the binding; the index gate prevents an
  idle small-thread render from auto-loading just because the whole
  thing fits in the viewport. Restoration must finish first
  (`restoredThreadId === pane.threadId`) so `handleLoadOlder`'s anchor
  capture isn't racing an unstable scrollTop. A
  `autoLoadAttemptedAtFloor` guard prevents re-firing while
  `pane.oldestLoadedTurnIndex` hasn't advanced — cleared on thread
  switch. The "Load older messages" button at the top of the timeline
  is the explicit fallback path; both routes funnel into the same
  `handleLoadOlder()`.
- Scroll-behavior tests live in `scroll.test.ts`. Component-shape tests
  for individual rows (TimelineLeaf, SubagentGroup, CommandOutput, etc.)
  stay in their own `*.test.ts` files.

## Row contract

Every row rendered inside `<Virtualizer>`'s children snippet:

- Lives inside a `[data-row-index]` outer wrapper. The wrapper is
  structural and intentionally has NO `data-item-id`. Only `TimelineLeaf`
  emits `data-item-id` on its root — that's what test queries, message
  search, and row-boundary markers anchor on. `SubagentGroup` is
  structural and does not carry `data-item-id`; response dividers
  therefore can only ever sit before a leaf, not before a subagent card.
  `shouldRenderTurnBoundaryBefore` in `MessageTimeline.svelte`
  enforces that contract by returning false for non-leaf nodes. The
  divider has two visual modes — labeled (`line | gap | pill | gap |
  line`) when the leaf is the final assistant_text of a settled turn,
  unlabeled (one continuous full-width line) otherwise. Both modes
  share a fixed wrapper height (`h-[1.625rem]`); the pill uses
  `leading-tight` to keep its content inside that wrapper across
  font-loading variance. Promoting an intermediate divider to "final"
  on turn settle therefore swaps the inner branch without changing row
  geometry — satisfies the "no late transcript adornments on
  completion" rule in `frontend/CLAUDE.md`. Tests discriminate the two
  modes via `data-final-response` on the wrapper plus presence/absence
  of "Response" in `divider.textContent`.
- Keeps its outer shell stable after first render. If a tool row might
  eventually have payload, render the header affordance from the start
  and disable the action until the body exists. Do not swap static rows
  into buttons, insert chevrons late, animate body height inside the
  scroll surface, or append completion-only history rows.
- Uses `TranscriptDisclosureHeader.svelte` for transcript disclosure
  headers unless there is a specific reason not to. The primitive keeps
  the chevron/button shell stable, uses `aria-disabled` for temporarily
  inert disclosures, and renders trailing actions as siblings so editor
  links / side-panel buttons are never nested inside another button.
- Is safe to remount when virtua scrolls a row out of and back into the
  rendered window. Snippets re-receive `pane`, `item`, `depth` on
  remount; nothing inside should depend on `onMount` running exactly
  once per item lifetime.
- Reads any "remembered" state (expansion toggles, loaded payload
  chunks, attachment blob URLs) out of a per-pane registry on the
  `ThreadPane`, NOT from local `let foo = $state(false)`. Local row
  state is wiped when virtua remounts the row; the registries are
  keyed on `item.id` / `payloadId` and survive remount. See:
  - `pane.expansionStateFor(item)` — payload expansion handle
    (preview/full toggle, loaded chunks). Used by
    `GenericToolCallRow`, `CommandOutput`, `ThinkingBlock`,
    `LazyContentBlock`. The handle survives virtua remount but is
    cleared on `switchThread` (toggle state is per-pane, not
    cross-thread). `DiffFileStack` deliberately uses a LOCAL
    `createPayloadExpansion` instead — diff rows render always-inline
    so there is no user-facing expand/collapse, and a per-row local
    handle keeps the fetch wired straight to the prop without going
    through the pane's payloadId-by-item lookup (which can lag the
    prop by a tick during fast switches).
  - `pane.attachmentCacheFor(itemId)` — image-attachment blob URL
    cache. `UserMessage` threads this into `createAttachmentPreviews`
    so a user-message row doesn't re-fetch `GetAttachmentData` on
    every scroll-back.
  - `pane.isSubagentGroupExpanded(groupKey)` /
    `toggleSubagentGroupExpanded(groupKey)` — collapse state for
    subagent cards. Use `SubagentGroupNode.groupKey`, not a raw parent
    item id.
  Read pattern: `const handle = $derived(pane.expansionStateFor(item))`,
  with any local fallback wrapped in `untrack(() => createPayloadExpansion(...))`
  so the fallback doesn't bind to initial prop values.
- Reads any payload BYTES through the module-level data cache in
  `utils/payloadDataCache.ts`. The per-pane registry above tracks
  toggle/expansion intent; the data cache tracks the bytes themselves,
  keyed by `(threadId, payloadId, version)` with NUL delimiters and
  type-tagged version keys, byte-bounded by a 16 MB LRU. The cache
  survives `switchThread` so re-entering a thread with already-loaded
  payloads paints synchronously at full height from frame 0 —
  eliminating the empty-then-loaded oscillation that whipsaws
  virtua's per-row size cache and produces visible scroll-anchoring
  jumps. `createPayloadExpansion` reads the cache synchronously in
  its constructor and writes back after every successful
  `loadPreview` / `showFull`. The `loadOnMount` option pairs with
  the cache: callers like `DiffFileStack` whose body always renders
  open opt in to "synchronously hydrate AND auto-expand on cache
  hit". Toggle-style consumers (the `expansionStateFor` callers
  above) leave `loadOnMount` false so their thread-switch reset of
  `expanded=false` survives a cache hit — the data is hydrated in
  the background and flashes in instantly when the user later clicks
  expand. Eviction: `removeThread` drops the deleted thread's slice
  via `clearPayloadCacheForThread`; the byte cap evicts oldest
  entries when exceeded; the `loadOnMount` effect fires once per
  unique `payloadId` so a future collapse path cannot re-trigger an
  unwanted re-expand.
- Defers heavy work (Mermaid render, Shiki highlight, KaTeX typeset,
  attachment image load) to dynamic imports / IntersectionObserver
  triggered from the row itself. Module-level singletons in
  `markdownEnhance.ts` cache the underlying highlighter / mermaid
  instance so per-row remount is just DOM work.

## Right-side panels

`RhsSidebarShell.svelte` hosts the right-side panels (plan, diff
checkpoint, diff payload). Visibility, width, and per-thread snapshot/
restore live in `stores/rhsPanelSlot.svelte.ts`; the discriminated
`RhsPanel` union is the wire-shape for the snapshot, so widening it
without updating `clonePanel` will silently drop fields on thread
switch.

Panel **bodies** receive `ctx: PanelContext` (defined in
`rhsPanelSlot.svelte.ts`), not `pane: ThreadPane`. The narrow contract
exposes `threadId`, `paneId`, `workspacePath`, `close()`, and
`replaceThread(thread)` — bodies cannot reach into chat-only state
(`pane.items`, `pane.timelineRevision`, streaming flags) and therefore
cannot accidentally re-render on every chat tick. The legacy
`pane: ThreadPane` shape on `DiffPanelDrawer` / `LazyDiffSidebar` is
kept until those bodies need to grow; new panels MUST take
`PanelContext`.

The shell itself keeps `pane: ThreadPane` because it owns the resizer
chrome (`pane.getRhsSidebarMaxWidth`, `pane.setRhsSidebarWidthLive`,
`pane.persistRhsSidebarWidth`) and the scroll-controller lease
(`pane.scrollController`). Don't migrate the shell.

**Future transcript-style panels** (e.g. a subagent's full transcript
with tool calls): the row primitives in this directory —
`TimelineLeaf`, `SubagentGroup`, `GenericToolCallRow`, `CommandOutput`,
etc. — are reusable inside a sidebar transcript when the items rendered
belong to the same pane. Per-pane registries (`expansionStateFor`,
`attachmentCacheFor`, `isSubagentGroupExpanded`) key by `item.id` /
`groupKey`, not array position, so a filtered subset of `pane.items`
remounts cleanly across the chat and the sidebar simultaneously. To
expose the registries to a transcript panel, extend `PanelContext`
with the specific accessors that panel needs — do **not** widen back
to `pane: ThreadPane`. The "no parallel virtualizer over `pane.items`
or `groupedNodes`" rule prohibits a competing virtualizer over the
**full** chat list; a sidebar virtualizer over a filtered subset is
fine (precedent: `DiffSidebarBody` runs its own file-level
virtualizer).

Adding a new panel kind:

1. Extend the `RhsPanel` union in `stores/rhsPanelSlot.svelte.ts`. If
   the variant carries data, add a `clonePanel` branch in the same
   file so snapshot/restore keeps the field across thread switches.
2. Add an entry to `PANEL_COMPONENTS` in `RhsSidebarShell.svelte` —
   the `satisfies` clause makes the type-check fail until you do.
3. Add a render branch in the `{#key}`-wrapped `{#if}` chain inside
   the shell. The `{#key pane.thread.id + ':' + activePanel.kind}`
   wrapper resets the body cleanly on thread switch and on panel-kind
   swap; rely on it instead of inner `{#key}` wrappers.

## Markdown rendering pipeline

`ChatMarkdown.svelte` mounts `<Streamdown>` (svelte-streamdown), which
owns marked-based parsing, shiki highlighting, KaTeX typesetting,
mermaid rendering, and `parseIncompleteMarkdown` auto-close for
streaming sources. Three thin Svelte hosts under
`components/chat/markdown/` wrap the library's built-in Code, Mermaid,
and Math components and stamp the original source onto a wrapping
element (`data-code-source` / `data-mermaid-source` / `data-math-source`
+ legacy `math-inline` / `math-display` / `mermaid` classes) so
`markdownSerialize.ts`'s copy-as-markdown round-trip and
`DiagramInteractionHost`'s right-click "copy source" still work.

`markdownEnhance.ts` is now a thin file that re-exports the markdown-
aware copy delegate (`ensureMarkdownCopyDelegate`) and ships
`enhancePathLinks(container, workspacePath)` for the project-relative
path linkifier — the only post-Streamdown enhancement we still own.
The path linkifier walks text nodes for `src/lib/foo.ts:L:C` style
patterns, replacing them with `<a class="editor-link">` anchors that a
document-level click delegate routes to the `OpenInEditor` binding.
ChatMarkdown calls it from a `$effect` once `streaming` flips false.

All other module-level caches (shiki highlighter, mermaid SVG promise
cache, language extension map) live inside `svelte-streamdown` itself.
Per-row remount is still cheap because Streamdown's caches survive
component remounts at the library level.

We ship a pnpm patch against `svelte-streamdown@3.0.1`
(`frontend/patches/svelte-streamdown@3.0.1.patch`) that fixes two
upstream defects in `parseIncompleteMarkdown`: (1) `Block.svelte` did
not honor the `parseIncompleteMarkdown={false}` prop, so the
auto-balancer ran on settled blocks; (2) the single-asterisk and
single-underscore plugins counted delimiters inside backtick inline-
code spans, balancing them with a stray trailing copy at end-of-
paragraph. Parser bugs in this pipeline go upstream-then-patch — do
not duplicate the fix in `markdownEnhance.ts` or in `ChatMarkdown`'s
host wrappers. Regression coverage lives in
`AssistantMessage.test.ts` (`'does not append a stray ...'` cases).
Pin `svelte-streamdown` to an exact version in `package.json` so a
`pnpm update` cannot silently move past the patch target.

## Test environment notes

happy-dom returns 0 for `clientHeight` / `clientWidth`, which would make
virtua mount zero rows. `MessageTimeline.svelte` switches virtua into
`ssrCount` mode under `import.meta.env.MODE === 'test'` so component
tests can assert on rendered DOM. Production (`vite dev` / `vite build`)
sees the default `undefined`, leaving virtua free to virtualize.

`useStickToBottom.svelte.test.ts` covers the controller's full state
machine in isolation: forceStick / markAtBottom / animateScrollTo,
content-RO positive/negative deltas (sync-pin gating on
escapedFromLockState / pauseDepth), wheel/scroll/keydown/touchmove
gesture handlers, programmatic-write tagging (`ignoreScrollToTop`),
pause-lease depth-counting, and lifecycle (re-attach detaches old
listeners; detach clears all timers). Geometry is stubbed per-test via
`Object.defineProperty` on `scrollHeight`/`clientHeight`/`scrollTop`,
and `performance.now` is mocked to advance 16.67ms per `nextFrame()` so
animateScrollTo's easeOutCubic interpolation is deterministic.

`scroll.test.ts` covers the MessageTimeline-level integration: snapshot
save/restore, load-older flow, scroll-to-item routing, and layout
invariants (composer-height variable, reserved-slot banners). Heavy
reliance on real layout is avoided — assertions are written so they
hold under happy-dom's missing geometry.
