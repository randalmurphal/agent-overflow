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
- `pane.activityRuns` for a run's collapse override, inner scroll position,
  mount window, and pending jump focus (see "Activity Runs" below).

Payload bytes go through `utils/payloadDataCache.ts`, keyed by
`(threadId, payloadId, version)` and byte-bounded by its LRU. Per-pane
registries track expansion intent; the data cache tracks loaded bytes.

Heavy work such as Mermaid render, syntax-span requests, KaTeX
typeset, and attachment image load should stay lazy and row-local.
Module-level singletons in the markdown pipeline keep remounts cheap.

## Activity Runs

Consecutive activity rows (tool calls, completions, thinking, and the group
cards on the same rail) are wrapped by the projection's last pass into ONE
`activity_run` row: `ActivityRun.svelte`, a height-capped clip that scrolls
in place, or `ActivityRunChip.svelte`, a one-line count chip. Architecture:
[`docs/architecture/activity-runs.md`](../../../../../docs/architecture/activity-runs.md).

Operational rules for this directory:

- **The rail belongs to the run.** One continuous border plus one hit strip
  per run, not per row — do not reintroduce per-row `isRail` styling.
- **Rows inside a run are still rows.** They keep their own components,
  leases, and margins; the run wraps them in a per-child
  `[data-run-child]` index wrapper (a jump's only handle on a non-leaf row,
  since only leaves emit `data-item-id`) and mounts a window of them.
- **A run's own state goes in `pane.activityRuns`**, keyed by the
  registry-assigned `runId` — never by a member item id, which changes at
  both window edges.
- **Nothing that changes per streaming delta belongs on the node.** Chip
  counts, failure, and the running label resolve from current items through
  `utils/activityRunSummary.ts`; a node field would rebuild the
  virtualizer's data array every chunk.
- **A jump into a run goes through `revealActivityRunItem`**
  (`utils/activityRunWindow.ts`), which expands, relocates the window, and
  leaves the focus request together. Do not call the three registry methods
  separately — a partial application scrolls nowhere or shows nothing.
- **The clip's scrollbar must consume zero width** and take no gutter, or
  the run's rows leave the rail and its text re-wraps on every overflow
  transition. Geometry that depends on that lives in
  `activityRunClip.browser.test.ts` / `activityRunScroll.browser.test.ts`;
  happy-dom cannot see any of it.

## Companion Panes

Plan, design preview, and review surfaces mount as companion panes through
`components/panes/CompanionPane.svelte`, snapped next to their source
thread pane by `stores/companionPanes.svelte.ts` and the pane layout store.
Chat components should open these via the companion/review store helpers,
not by mounting sidebars inside `ChatView`.

Panel bodies receive `ctx: PanelContext`, not `pane: ThreadPane`. Keep the
body contract narrow so companion panes cannot accidentally subscribe to
every chat tick.

The review pane (`components/review/`) owns full-diff workflows: local
scope selection, file-tree navigation, virtualized diff rendering, and
comment drafts. Inline diff affordances should route there through
`reviewTrigger.ts` / `openReviewCompanion`.

## Markdown Rendering

`ChatMarkdown.svelte` mounts `svelte-streamdown`, with host wrappers under
`components/chat/markdown/` for Code, Mermaid, and Math. Those wrappers
stamp original source on `data-code-source`, `data-mermaid-source`, and
`data-math-source` so markdown copy and diagram actions keep working.

Path linkification runs inside marked parsing from the server-validated
`PathRef[]` allowlist on item metadata. The generated href includes a
per-page-load nonce and is the only `agent-overflow:open` form admitted
by Streamdown's `transformUrl`.

Code-block spans come from the backend (`HighlightCode` via
`markdown/codeSpanCache.ts`); remote clients additionally ingest
backend-pushed seeds (`highlight:seed` → `markdown/liveCodeSeeds.svelte.ts`,
wired through `stores/eventsHighlight.ts`) so streaming fences color
without a WAN round trip per growth step. Seeds are hash-verified
cache-warmers — a non-matching seed is inert and the RPC path takes
over. Loopback clients never receive seed frames.

Settled history rows skip the RPC entirely: `AssistantMessage.svelte`
and `DiffFileStack.svelte` ingest persisted span blobs
(`items.meta` `codeSpans`, `Item.payloadPreviewSpans`) via
`utils/persistedSpans.ts` — synchronously at component init, before
the code/diff hosts mount and take their first cache reads. Blobs are
version-stamped (`hv`) and content-addressed; a stale blob is dropped
and the RPC path recomputes. The ingest is deliberately not memoized:
thread switches evict the span caches, and remounts must be able to
re-seed.

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
