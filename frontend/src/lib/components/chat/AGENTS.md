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
`activity_run` row: `ActivityRun.svelte`, an always-present summary header
(`ActivityRunHeader.svelte`) over a height-capped clip that scrolls in place
and is what collapsing removes. Architecture:
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
- **Collapse is resolved ONCE, by the registry, and `node.collapsed` is the whole
  answer.** `ActivityRunIdentity.collapsedFor(runId, live)` ranks the three facts:
  a per-run answer from the reader wins outright, then the thread's bulk state,
  and only if nobody has said anything does liveness decide — a working run
  renders open and folds itself shut when it settles
  (`activityRunClipPresence.svelte.ts`). Downstream, `collapsed` means exactly
  "renders without its clip": the presence machine, the structure signature and
  the row estimate all read that one field, and none of them looks at `live`.
  Do not re-introduce a `!collapsed || live` predicate at a consumer — that is
  what made the reader's collapse of the live run inert, moving the chevron and
  nothing else, and it is why the registry now takes liveness as an input to the
  FALLBACK instead of consumers treating it as an override.
- **A collapse set by the reader is instant and beats liveness; a collapse that
  falls out of the defaults is what folds.** `setCollapsed(runId, target)` takes
  the target state rather than toggling, because only the row knows the state on
  screen — a registry-side toggle would invert its own settled answer and hand
  back the state the reader is already looking at. The registry still tracks clip
  presence separately as `clipOpen` (a fold outlives the flag, and
  `saveScrollSnapshot` refuses on THAT, never on `collapsed`).
- **The fold animates a wrapper's height and writes no `scrollTop`.** The
  shrinking box goes through the virtualizer's row observer like any other row
  and the controller decides what it means for the reading position. Do not add
  a scroll write "to keep the bottom pinned" — the engine's compensation and the
  browser's own clamp already produce the right result from every reader
  position, and a second writer would fight the spring.
- **Only a run that stopped being LIVE animates.** Every collapse the reader
  clicked — header, rail, bulk toggle — is instant, and the fold is deferred
  (never cancelled) while the reader is off the clip's newest row. If you add a
  third way to collapse a run, it is manual: it must not acquire the fold by
  accident.
  "Something is expanded inside the run" is NOT a deferral reason and must not
  be re-added as one: `expandedPx` counts default-expanded bodies too, and
  `collapseDiffPreviews` defaults to expanded, so it would silently block the
  fold for every run holding an edit.
- **Anything keyed on liveness must read `node.live`.** It is stamped by the
  projection from the items, withheld nodes included, precisely so it cannot
  flap while the reveal gate opens and closes. Re-deriving "is this the tail"
  from `revealedNodes` reintroduces the flap, which rebuilds the scroll
  controller and aborts a fold mid-animation.
- **Props a run's effects branch on go through a `$derived` primitive first.**
  `run.collapsed` and `live` are plain reads of a prop the projection replaces
  on every streamed row, so an effect reading either directly re-runs every
  pass — measurably, it tore the controller down and rebuilt it mid-gesture.
  Same rule, same reason as `runId`.
- **A clip following its last row is held there while content settles.** The
  mount write happens before the rows inside finish resolving, so without the
  settle observer an expanded run drifts out from under the reader. Whether it
  is following (`followingBottom`) is STORED, never measured: growth moves the
  bottom away from the clip on every row that resolves, and the `scroll` event
  from a write arrives after that growth, so a run that re-derived the answer
  from geometry abandoned the follow on the first row and reopened near its
  top. Only a reader gesture may clear it; every write states it.
- **Collapsing a run forgets where the reader was inside it** (scroll snapshot
  and window anchor both), so it reopens on its newest row. `saveScrollSnapshot`
  refuses a save for a run with no clip because the closing clip's own teardown
  routes through it — do not "fix" that by moving the check to the call site.
- **Every collapse/expand runs inside `withViewportBottomHeld`** (header, rail,
  and the header bar's bulk toggle). It holds the viewport's BOTTOM edge, so the run
  opens upward over rows the reader is already reading rather than pushing them
  down the page, and it pauses the spring so a toggle while bottom-pinned is
  instant instead of an animated ride across the delta. A new caller that
  mutates run collapse state must go through it —
  `docs/architecture/frontend-scroll.md` §Reader-Requested Height Changes.
- **The run's header is present in BOTH states and is the per-run control.**
  It used to render only while collapsed, so expanding removed the element the
  reader had just clicked and left the invisible rail strip as the only way
  back. Do not make it conditional again — the estimate, the signature, and the
  fold's seamless landing all now assume a header that never moves. The rail
  stays as a second, larger target on the same `toggle`, but **pointer-only**
  (`aria-hidden` + `tabindex="-1"`, both, since hiding a focusable element is
  its own defect): now that it duplicates a header that is always there, ARIA on
  it means a focus ring on a transparent 16px strip and the run's state announced
  twice from two buttons naming one region. The header bar's
  `activity-runs-toggle` remains the THREAD-level bulk action, rendering from
  `activityRuns.bulkCollapsed` and never from a survey of the rendered runs.
- **Nothing that changes per streaming delta belongs on the node.** The
  header's counts, failure, and running label resolve from current items through
  `utils/activityRunSummary.ts`; a node field would rebuild the
  virtualizer's data array every chunk.
- **Whether a run follows its tail is the ROW's call, not the registry's.**
  While the inner controller is escaped, `ActivityRun.svelte` pins the mount
  window (`setWindowAnchor`) so appended rows cannot slide the reader's rows
  up the clip; returning to the clip's bottom releases it. Do not add a
  geometric release — a pin means "the reader is up here." A new controller
  reads the existing pin and starts escaped, for the same reason it carries
  the snapshot's escape flag: a historical run that a jump pinned had no
  controller to record the event on.
- **A tail-following window's head advance is compensated, and the growth it
  hides is stated.** An appended row drops one off the head in the same flush,
  so the clip's total height barely changes: the reader's rows jump up a row
  height and the controller's own observer sees nothing to chase. The pair of
  effects in `ActivityRun.svelte` holds the incoming head row's viewport
  position across the flush, then calls
  `markStructuralContentPending()` + `observe('live-content')` so the spring
  glides the new row in. Both halves are load-bearing — the hold alone leaves
  the run a row short of its newest activity, further short on every append
  after that. Compensation writes on the live run go through
  `applyEngineCompensation({ kind: 'head-splice' })`; a bare `scrollTop =`
  reads as a reader gesture and escapes bottom-follow.
- **Reaching the top of a run's window pages the next chunk in.** The
  `· · · N earlier` boundary is a button *as well*, not instead — do not make
  it the only way past the window. The trigger refuses a clip that is not
  scrollable past its runway, which is what stops it overriding
  `activityRunWindowRows`; keep that guard if you touch the threshold. It also
  refuses a scroll event no gesture preceded — the run writes its own position
  on mount, after a prepend, and on a jump, and the mount write lands inside
  the runway because nothing inside is measured yet. Route any new position
  write through `positionWritten`, never `syncPosition` alone, or the run pages
  backwards through its own history.
- **A jump into a run goes through `revealActivityRunItem`**
  (`utils/activityRunWindow.ts`), which expands, relocates the window, and
  leaves the focus request together. Do not call the three registry methods
  separately — a partial application scrolls nowhere or shows nothing.
- **The clip's scrollbar must consume zero width** and take no gutter, or
  the run's rows leave the rail and its text re-wraps on every overflow
  transition. Geometry that depends on that lives in
  `activityRunClip.browser.test.ts` / `activityRunScroll.browser.test.ts`;
  happy-dom cannot see any of it.
- **The overlay bar is a SIBLING of the clip, not a descendant.** Nothing that
  starts on the strip reaches the clip on its own: wheel is applied by the
  control and taken out of the tree, touch is held by `touch-action: none`,
  and every gesture states its intent through
  `onUserScrollStart` / `onUserScrollEnd`. A gesture the control forwards
  without stating intent moves the clip and then gets yanked back by the next
  chunk; one it leaves alone scrolls the conversation instead.

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
