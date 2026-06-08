# components/chat/

Chat-surface components. `MessageTimeline.svelte` owns the virtualized
timeline; row components render stable transcript records.

## Scroll Contract

Read
[`docs/architecture/frontend-scroll.md`](../../../../../docs/architecture/frontend-scroll.md)
before editing `MessageTimeline.svelte`, `ChatView.svelte`, row expansion,
load-older behavior, or any code that touches `scrollTop`.

Operational rules for this directory:

- Use `listRef.findItemIndex(offset)`, `listRef.getItemOffset(index)`,
  and `listRef.scrollToIndex(...)` for timeline position. Do not query
  DOM rows for first-visible-item math.
- Route programmatic scrolls through `useStickToBottom`: `forceStick`,
  `markAtBottom`, `notifyContentMaybeGrew`,
  `notifyLiveContentMaybeGrew`, `pauseAutoScroll`, `runExternalScroll`,
  `stopScroll`, `animateScrollTo`, and `armRestoreSnap`.
- Wrap every virtua `scrollToIndex` call in
  `stick.runExternalScroll(() => listRef.scrollToIndex(...))`.
- Never pass `smooth: true` to virtua and never call `scrollIntoView()` on
  a virtualized row.
- Keep `overflow-anchor: none` on the outer scroll container.
- Keep composer-clearance padding on `scrollEl`, not `contentEl`, and keep
  `ChatView`'s composer `ResizeObserver` notifying the scroll controller
  after writing `--composer-height`. Use the live-capable notification path
  so active output can spring through activity-rail height changes; idle
  composer geometry must still sync-pin.

Thread-switch restore is intentionally split: `$effect.pre` arms warm-up
and restore consent before DOM flush; the restore `$effect` calls
`forceStick({ reason: 'restore' })` and schedules one rAF
`notifyContentMaybeGrew()` settle pass. Same-thread reloads must watch
`pane.switchGeneration`, not only `pane.threadId`.

## Row Contract

Every row rendered inside `<Virtualizer>`:

- Lives inside a `[data-row-index]` wrapper. Only `TimelineLeaf` emits
  `data-item-id` on its root; structural nodes such as `SubagentGroup`
  do not.
- Keeps its outer shell stable after first render. Do not swap static rows
  into buttons, insert chevrons late, animate body height inside the
  scroll surface, or append completion-only history rows.
- Uses `TranscriptDisclosureHeader.svelte` for disclosure headers unless
  there is a specific reason not to.
- Survives virtua remount. Row-local state disappears when scrolled out of
  the rendered window, so remembered state belongs in per-pane registries
  keyed by `item.id`, `payloadId`, or `groupKey`.

Use these pane registries instead of local row state:

- `pane.expansionStateFor(item)` for payload expansion handles.
- `pane.attachmentCacheFor(itemId)` for image attachment blob URLs.
- `pane.isSubagentGroupExpanded(groupKey)` /
  `pane.toggleSubagentGroupExpanded(groupKey)` for subagent cards.

Payload bytes go through `utils/payloadDataCache.ts`, keyed by
`(threadId, payloadId, version)` and byte-bounded by its LRU. Per-pane
registries track expansion intent; the data cache tracks loaded bytes.

Heavy work such as Mermaid render, Shiki highlight, KaTeX typeset, and
attachment image load should stay lazy and row-local. Module-level
singletons in the markdown pipeline keep remounts cheap.

## Right-Side Panels

`RhsSidebarShell.svelte` hosts plan, diff checkpoint, and diff payload
panels. Visibility, width, and per-thread snapshot/restore live in
`stores/rhsPanelSlot.svelte.ts`.

Panel bodies receive `ctx: PanelContext`, not `pane: ThreadPane`. Keep
the body contract narrow so panels cannot accidentally subscribe to every
chat tick. The shell itself keeps `pane` because it owns resizer chrome
and the scroll-controller pause lease.

To add a panel kind:

1. Extend the `RhsPanel` union and `clonePanel` in
   `stores/rhsPanelSlot.svelte.ts`.
2. Add the component to `PANEL_COMPONENTS` in `RhsSidebarShell.svelte`.
3. Add a render branch in the keyed shell body.

Clamp panel width through the owning pane (`pane.getRhsSidebarMaxWidth`),
not `window.innerWidth` or the app shell.

## Markdown Rendering

`ChatMarkdown.svelte` mounts `svelte-streamdown`, with host wrappers under
`components/chat/markdown/` for Code, Mermaid, and Math. Those wrappers
stamp original source on `data-code-source`, `data-mermaid-source`, and
`data-math-source` so markdown copy and diagram actions keep working.

Path linkification runs inside marked parsing from the server-validated
`PathRef[]` allowlist on item metadata. The generated href includes a
per-page-load nonce and is the only `agent-overflow:open` form admitted
by Streamdown's `transformUrl`.

The `svelte-streamdown@3.0.1` pnpm patch is intentional. Parser bugs in
that pipeline go upstream-then-patch; do not duplicate parser fixes in
`markdownEnhance.ts` or the host wrappers. Regression coverage lives in
`AssistantMessage.test.ts`.

## Test Notes

happy-dom reports zero geometry, so `MessageTimeline.svelte` enables
virtua `ssrCount` only under `import.meta.env.MODE === 'test'`.

`scroll.test.ts` covers MessageTimeline-level behavior: snapshot
save/restore, load-older flow, scroll-to-item routing, and layout
invariants. Individual row behavior belongs in row-specific tests.
