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
  `markAtBottom`, `observe(kind)`, `pauseAutoScroll`, and
  `armRestoreSnap`. `listRef.scrollToIndex(...)` needs no wrapper — the
  virtualizer performs the write through the controller chokepoint
  (`applyScrollTarget`), so it arrives tagged.
- `scrollToIndex` is instant-only by design (native smooth scrolling
  races the controller's tagging); never call `scrollIntoView()` on a
  virtualized row.
- Keep `overflow-anchor: none` on the outer scroll container.
- Keep composer-clearance padding on `scrollEl`, not `contentEl`, and keep
  `ChatView`'s composer `ResizeObserver` notifying the scroll controller
  after writing `--composer-height`. Observe as `'composer-geometry'`
  (the live-capable path) so active output can spring through
  activity-rail height changes; idle composer geometry must still
  sync-pin.

Thread-switch restore is intentionally split: `$effect.pre` arms warm-up
and restore consent before DOM flush (`armRestoreSnap` carries the
defensive escape); the restore `$effect` calls
`forceStick({ reason: 'restore' })` and schedules one rAF
`observe('content')` settle pass. Same-thread reloads must watch
`pane.switchGeneration`, not only `pane.threadId`.

The scroll-session machinery extracted from `MessageTimeline.svelte`
lives in seven sibling modules: `timelineRestore.svelte.ts` (thread-switch
restore sessions and scroll snapshots), `timelineSizePriors.svelte.ts`
(per-thread row size priors, incl. `ROW_KIND_ESTIMATE_PX`),
`timelinePaging.ts` (load-older/newer gates and handlers),
`timelineWindowAnchor.svelte.ts` (prune-shift anchoring),
`timelineRowProjection.svelte.ts` (the node-derivation pipeline: grouping,
the reveal gate, rail classification, response-pill duration),
`timelineDiagnostics.ts` (render/state tracing and the dev-only memory /
pane-geometry / row-resize / margin-divergence / reasoning-tail-jump
probes), and `timelineRowUiPrune.ts` (offscreen row-UI-state pruning).
`MessageTimeline` keeps only the thin `$effect` bodies that call into
them.

## Row Contract

Every row rendered inside `<TimelineVirtualizer>`:

- Lives inside a `[data-row-index]` wrapper. Only `TimelineLeaf` emits
  `data-item-id` on its root; structural nodes such as `SubagentGroup`
  do not.
- Keeps its outer shell stable after first render. Do not swap static rows
  into buttons, insert chevrons late, animate body height inside the
  scroll surface, or append completion-only history rows.
- Uses `TranscriptDisclosureHeader.svelte` for disclosure headers unless
  there is a specific reason not to.
- Survives windowing remount. Row-local state disappears when scrolled out
  of the rendered window, so remembered state belongs in per-pane registries
  keyed by `item.id`, `payloadId`, or `groupKey`.

Use these pane registries instead of local row state:

- `useLeasedItemExpansion(...)` / `useLeasedPayloadExpansion(...)` for
  mounted row payload expansion handles. The bare `pane.expansionStateFor*`
  APIs are for store code, tests, or non-component callers that will not
  keep reading a handle after timeline pruning can dispose it.
- `pane.attachmentCacheFor(itemId)` for image attachment blob URLs.
- `pane.isSubagentGroupExpanded(groupKey)` /
  `pane.toggleSubagentGroupExpanded(groupKey)` for subagent cards.
- `pane.diffCardExpandedOverride(itemId, filePath)` /
  `pane.setDiffCardExpanded(itemId, filePath, expanded)` for inline
  diff-card expand/collapse overrides. Tri-state: an absent entry
  follows the `collapseDiffPreviews` setting default; pass `undefined`
  to clear the override.

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

The `svelte-streamdown@3.1.2` pnpm patch is intentional. Parser bugs in
that pipeline go upstream-then-patch; do not duplicate parser fixes in
`markdownEnhance.ts` or the host wrappers. Regression coverage lives in
`AssistantMessage.test.ts`.

## Test Notes

happy-dom reports zero geometry, so `MessageTimeline.svelte` enables the
virtualizer's `renderAll` (mount-every-row) seam only under happy-dom
test runs (`MODE === 'test'` AND the `window.happyDOM` marker). The
Chromium browser project keeps real windowing — its outcome tests count
row unmounts.

Layout invariants that happy-dom cannot see (margin containment, flush,
oscillation geometry) run in the real-Chromium `browser` vitest project
(Playwright); name those files `*.browser.test.ts`. Cascade-coupled tests
import the production `app.css` so the assertion runs against the real
styles — e.g. `rowMarginContainment.browser.test.ts` guards the
settle-flicker fix (`[data-row-geometry-content] { display: flow-root }`);
cascade-independent ones (e.g. `timelineVirtualizer.browser.test.ts`, the
adapter-seam suite) skip the import. `pnpm test` runs the
unit project only, so the `make test` / `make verify` gate needs no browser
binary; run the browser suite explicitly with `pnpm test:browser`, which needs a
chromium build: `pnpm exec playwright install chromium`. See
`frontend/vitest.config.ts`.

`scroll.test.ts` covers MessageTimeline-level behavior: snapshot
save/restore, load-older flow, scroll-to-item routing, and layout
invariants. Individual row behavior belongs in row-specific tests.
