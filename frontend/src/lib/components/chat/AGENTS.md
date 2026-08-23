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
lives in eight sibling modules: `timelineRestore.svelte.ts` (thread-switch
restore sessions and scroll snapshots), `timelineSizePriors.svelte.ts`
(per-thread row size priors, incl. `ROW_KIND_ESTIMATE_PX`),
`timelinePaging.ts` (load-older/newer gates and handlers),
`timelineWindowAnchor.svelte.ts` (prune-shift anchoring),
`timelineRowProjection.svelte.ts` (the node-derivation pipeline: grouping,
the reveal gate, rail classification, response-pill duration),
`timelineDiagnostics.ts` (render/state tracing and the dev-only memory /
pane-geometry / row-resize / margin-divergence / reasoning-tail-jump
probes), `timelineQuietWork.ts` (the quiet scheduler: one cadence for
the deferred structural passes — recent-window prune retry,
auto-collapse releases, row-UI prune — with geometry-mutating work
gated on "no glide running or armed"), and `timelineRowUiPrune.ts`
(offscreen row-UI-state pruning, a quiet-work pass).
`MessageTimeline` keeps only the thin `$effect` bodies that call into
them.

`ChatView.svelte` is split the same way: the edit-and-resend flow (stage
machine, confirm gate, the destructive RPC and every failure branch)
lives in `editResendFlow.svelte.ts`, and the component keeps prop wiring,
two invalidation `$effect` bodies and the confirm dialog's markup.

On saga success the flow lands the pane at the thread's new tail with
bottom-follow engaged, through the controller adapter's optional
`stickToLatest` (MessageTimeline maps it to `paging.jumpToLatest`, which
reconciles a windowed tail before pinning). This is the one deliberate
divergence from "a send never yanks a scrolled-up reader": the height
the reader was parked at measured rows the revert just destroyed, and
they asked for this message to become the tail. Success only — every
failure branch and the mid-RPC-thread-switch case leave the scroll
untouched.

## Row Contract

Every row rendered inside `<TimelineVirtualizer>`:

- Lives inside a `[data-row-index]` wrapper. Only `TimelineLeaf` emits
  `data-item-id` on its root; structural nodes such as `SubagentGroup`
  do not.
- Keeps its outer shell stable after first render. Do not swap static rows
  into buttons, insert chevrons late, animate body height inside the
  scroll surface, or append completion-only history rows.
- Puts header ACTIONS (open-in-pane, background, stop) before the meta
  columns. `ToolHeaderMeta` renders status slot, duration, timestamp in
  that order on every row so the columns line up down the transcript;
  its `actions` slot is its FIRST child. Never append a control after
  the `<time>` — it shifts the timestamp column on exactly the rows
  that carry the control.
- Keeps conditionally rendered header actions height-neutral across every
  state transition. `opacity-0` hides paint, not layout, so a running-only
  control still sets the row height until it is removed at completion. Icon
  buttons inside text-bearing headers must establish explicit flex geometry
  (`inline-flex items-center justify-center`) so inherited text line boxes and
  padding cannot make the active row taller. Any control that appears or
  disappears between running and settled states needs a real-browser
  regression comparing the row root's height before and after the transition;
  happy-dom cannot prove this geometry.
- Uses `TranscriptDisclosureHeader.svelte` for disclosure headers unless
  there is a specific reason not to.
- Survives windowing remount. Row-local state disappears when scrolled out
  of the rendered window, so remembered state belongs in per-pane registries
  keyed by `item.id`, `payloadId`, or `groupKey`.
- May be dimmed by `MessageTimeline`'s `pendingCutAfter` prop while an
  edit-and-resend saga's revert is actually in flight (rows strictly after
  the anchor in display order get `chat-row-pending-cut`). That is a CLASS
  toggle on the existing row wrapper and nothing else: the timeline is not
  truncated until the backend's `user_message:reverted` lands, so a row must
  not change DOM shape or write per-row state on the strength of a pending
  destruction. The base `transition-opacity` on those wrappers is
  unconditional on purpose — a CSS transition needs the property present
  BEFORE the value changes, so gating the class on `pendingCutAfter` would
  make the dim snap in and out instead of fading.
- **Runs no CSS transitions.** The timeline transition kill rule in
  `app.css` zeroes `transition-duration` for everything inside the
  scroller: an active animation licenses the compositor to present a
  frame whose tiles have not finished rastering, and a bottom-held toggle
  invalidates the tiles of every screen-stationary row below the toggled
  run — the decorative transitions that started in that same commit
  (chevron rotate, fades, hover re-target) were what made the text below
  blank on expand (2026-08-17). New row polish must not rely on a CSS
  transition; it will be inert here and animate anywhere else the
  component is reused. The one carve-out is the `[data-row-index]`
  wrapper itself, which is what keeps the pending-cut dim above fading.
  Svelte `transition:`/`in:`/`out:`/`animate:` directives are the same
  hazard through a door the CSS rule cannot reach (WAAPI/inline styles),
  so they are banned in this directory outside a verified
  outside-the-scroller allowlist. The hazard is really the scroller's DOM
  subtree, not the directory — components rendered into rows from
  elsewhere (the vendored streamdown popovers) carry directives the walk
  cannot see, so a new external row dependency needs the same check by
  hand. Guards:
  `timelineTransitionSuppression.browser.test.ts` (the CSS rule),
  `timelineAnimationDirectives.test.ts` (the directive ban). The why —
  which two motion owners exist and why everything else renders as print
  — is `docs/architecture/frontend-scroll.md` §The Print Doctrine.

The user row's body swap to a live editor (`UserMessageEditor.svelte`) is
the one deliberate exception to "keep the outer shell stable", and it is
scoped: the swap happens only through `preservePaneScrollAnchorAt`, which
holds the bubble's top edge across a height change the reader is looking
straight at. Nothing else may swap a row's body.

Because the anchor row is virtualized and CAN remount mid-edit,
everything the editor must remember lives on the `UserMessageEditSession`
that `ChatView`'s flow (`editResendFlow.svelte.ts`) owns — the local
draft store, the seed the dirty check compares against, the session's
upload id set, and the `ui` object (focus intent, caret, open discard
confirm, inline command error). None of it may become row-local `$state`:
a remount would silently reset it, and `ui` is one mutable object
precisely because the flow rebuilds the session on every stage
transition.

The editor can also outlive a failed saga. When the destructive RPC dies
with the WIRE (rather than being refused), the flow returns to its editor
holding the edit instead of voiding: the backend runs to completion
regardless of the lost answer, and the anchor row is the witness for which
way it went. Surviving the reconnect's event replay means nothing
committed, so the user just sends again; disappearing means something did,
and the ordinary anchor-removed invalidation voids the editor. The one
thing every later exit of that flow must skip is deleting the session's
uploaded attachment records — a resend that actually landed, or the
backend's merged crash-copy draft row, may reference those ids, and the
anchor is a stale witness until the resync replays what the socket lost,
so an outcome-unknown flow never reclaims (an orphaned record is
invisible; deleting a referenced one corrupts a visible message).

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
  diff-card expand/collapse overrides. An absent entry follows the
  `collapseDiffPreviews` setting default; the setter takes the state
  the reader put the card in and stores it only when it DEVIATES from
  that default, so there is no clear call and no stale-override sweep —
  flipping the setting retires the overrides it catches up with on the
  next read (`liveDiffOverride`) and flipping back restores them.
- `pane.isUserMessageExpanded(itemId)` /
  `pane.setUserMessageExpanded(itemId, expanded)` for a long user message
  whose text the reader unclamped. Every message defaults to clamped and no
  setting moves that default, so membership in the registry IS the deviation
  — collapsing forgets the id rather than storing `false`, and there is no
  override-vs-default reconciliation to do (contrast the diff cards, whose
  default `collapseDiffPreviews` can move under them).
- `pane.activityRuns` for a run's collapse override, inner scroll position,
  mount window, and pending jump focus (see "Activity Runs" below).

`CommandOutput.svelte` holds two deliberate pieces of row-local state,
`detectedDevServerURL` and `confirmedDevServerURL`, and they are NOT an
exception to the rule above. `payloadMeta.devServerUrl` (see
`internal/triage/dev_server_url.go`) is rebuilt from each 100ms flush
window while a command streams, so a startup banner is only present in
the window that carried it; the completion rebuild recomputes it over
the cumulative output and persists it. The row holds the first detection
so the `DevServerChip` cannot blink out the moment the server logs its
first request. That is last-known-value smoothing of a jittering SERVER
field, not remembered user intent — a windowing remount mid-run re-reads
whatever meta currently says and the persisted value takes over at
settle, so nothing is durably lost. Do not copy this shape for anything
a reader chose. The chip itself renders only from
`confirmedDevServerURL`: detection proves the output MENTIONED a
loopback URL, so the row asks the backend to confirm a listener
(`utils/devServerProbe.ts` → `ProbeDevServerURL`; the backend owns the
verdict TTLs). While the command runs, an unconfirmed candidate
re-probes on a bounded fast cadence and a confirmed one is re-verified
on a slower one, retracting the chip if its server dies mid-run; a
candidate moving to a different URL deliberately does not retract a
confirmed chip (the settle rebuild recomputes the candidate as the
first URL in cumulative output, so a mere mention of another loopback
URL must not blank a verified chip). A settle stops rescheduling — the
last pending tick is the final probe — and a remount re-asks the
backend, so a chip for a dead server does not come back (modulo the
backend's short live-verdict TTL).

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
  per run, not per row — do not reintroduce per-row `isRail` styling. Both
  span the CLIP only: the header sits outside the rail with its chevron
  centred on the rail's x, and the strip folds with the clip (a collapsed
  run has no edge left to click — the header is the whole run then).
- **Rows inside a run are still rows.** They keep their own components,
  leases, and margins; the run wraps them in a per-child
  `[data-run-child]` index wrapper (a jump's only handle on a non-leaf row,
  since only leaves emit `data-item-id`) and mounts a window of them.
- **A run's own state goes in `pane.activityRuns`**, keyed by the
  registry-assigned `runId` — never by a member item id, which changes at
  both window edges.
- **Collapse is resolved ONCE, by the registry, and `node.collapsed` is the whole
  answer.** `ActivityRunIdentity.collapsedFor(runId, atTail)` ranks four facts:
  a per-run answer from the reader wins outright; then tail-ness — the newest
  revealed run renders open, and rendering open is RECORDED (`openedLive`);
  then that recorded hold, which keeps a displaced run open until the gate
  releases it; then the thread's bulk state and the setting. Tail-ness, NOT
  liveness, on purpose: `live` goes false the moment the run's closing prose
  exists behind the reveal gate, so a fast run whose next section arrived
  before its first projection pass was never once seen live, recorded no hold,
  and was born collapsed (the sampled-liveness race, 2026-08-18). The tail run
  is what the reader watched stream whether or not the wire has raced ahead,
  and it is a superset of the live run by construction. Downstream,
  `collapsed` means exactly "renders without its clip": the row's template,
  the structure signature and
  the row estimate all read that one field, and none of them looks at `live`.
  Do not re-introduce a `!collapsed || live` predicate at a consumer — that is
  what made the reader's collapse of the live run inert, moving the chevron and
  nothing else, and it is why the registry now takes tail-ness as an input to
  the FALLBACK instead of consumers treating it as an override.
- **A collapse set by the reader is instant and beats tail-ness AND the hold.**
  `setCollapsed(runId, target)` takes
  the target state rather than toggling, because only the row knows the state on
  screen — a registry-side toggle would invert its own settled answer and hand
  back the state the reader is already looking at. Every `setCollapsed` write
  retires the run's `openedLive` hold: an explicit answer beats a courtesy.
  Clip presence (`clipOpen`) is recorded by `collapsedFor` itself — the
  resolution IS what the row renders — and `saveScrollSnapshot` refuses on
  THAT, never on the `collapsed` override.
- **A settled run auto-collapses OFF-SCREEN only, and instantly**
  (`timelineActivityRunAutoCollapse.ts` + `utils/activityRunAutoCollapse.ts`).
  Never in front of the reader, and never animated — the fold animation was
  built and rejected; do not bring back a "softer" in-view collapse. The gate
  releases a hold only when the run is fully outside the viewport by a margin,
  more than a viewport behind the TAIL (distance from the tail, not
  viewport-exit — a reader who scrolls up and back must find the latest runs
  as they left them), with nothing inside engaged. A failed member does NOT
  hold the run open (removed 2026-08-18) — the collapsed chip's failure
  marker already carries it.
  Releases batch through the registry's `releaseOpenedLive(runIds)`, whose
  ONE `withViewportBottomHeld` transaction wraps the whole batch — and the
  gate must NEVER run while `autoScrollInFlight()` (spring active or a
  structural arm pending): the transaction's bottom-pinned restore is a
  direct write, so a release landing mid-glide snaps the animation. That
  stand-down is the quiet scheduler's (`timelineQuietWork.ts` — the gate is
  one of its 'quiet' passes; the scheduler's recheck timer re-runs a blocked
  pass once the glide dies). The gate can only see motion that exists when
  the pass runs, so the registry's transaction passes `takeover: 'yield'`:
  a wire append landing between the release and its restore arms the
  structural spring, and the restore's `requestBottom` hands the trip to it
  instead of writing a bottom that already contains the new row (regression:
  appendAfterQuiet.browser.test.ts, the collapse-vs-append race).
- **Engagement is deviation-based, never pixel-based.** The gate's "reader
  opened something in here" check is `pane.hasUserExpansionWithin` — diff
  overrides that say expanded, subagent / wait / read groups, payload bodies
  whose default is collapsed — the same "user deviations from default"
  contract as `expansionSignature`. Two corollaries with teeth: expansion
  entries created with `loadOnMount` are skipped wholesale (`autoExpands` on
  the registry entry) because their expanded bit is the setting's doing, and
  the peek's scope is `renderedItemIdsWithin(run.children)`, not
  `memberItemIds` — identity membership stops at group parents, but a
  reader's expansion can sit on a wait child or inside an opened subagent
  card. Do not substitute `expandedPx` or any rendered-height proxy:
  `collapseDiffPreviews` defaults diffs to expanded, so a pixel guard
  silently pins every run holding an edit forever. Group expansion the gate
  must see lives in the pane registry, never in row-local `$state` —
  WaitGroup's "Show N more" writes its `wait:` key for exactly this reason
  (and so the answer survives a windowing remount).
- **Anything keyed on liveness or tail-ness reads the stamped node field**
  (`node.live` / `node.atTail`), never a per-consumer re-derivation from
  `revealedNodes` — that reintroduces the flap that rebuilt the scroll
  controller mid-stream and re-recorded holds the gate just released. The
  two fields answer different questions: `atTail` (the last REVEALED node)
  is the reader-facing claim, and every BEHAVIORAL consumer keys on it —
  collapse resolution, the inner controller's lifetime, and the
  auto-collapse gate's skip. `live` (stamped from the items, withheld nodes
  included) is the strict "nothing foreign behind the gate" claim; its
  remaining consumer is the row's `data-live` attribute, kept deliberately
  as the forensic/test seam that proves the withheld window exists (the
  `controller lifetime` unit test asserts owner `controller` while
  `data-live` is false). The controller keyed on `live` once: it died
  mid-stream the moment closing prose hit the wire, cancelling a glide the
  reader was watching (the 2026-08-19 in-run jump — see
  `docs/architecture/activity-runs.md` §The inner controller).
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
- **A snapshot is only written from a laid-out, scrollable clip, and a clamped
  mount restore stays armed until the content can take it.** Two halves of one
  defect (2026-08-22, "no faded top edge"): on a windowing eviction the clip's
  content collapses before the teardown reads it, so `scrollTop` reads 0 and
  the scroll event the clamp fires archives that artifact over the reader's
  real position; and at remount the restore write lands before the rows have
  measured, so the browser clamps it toward 0 with nothing re-asking for an
  escaped run. `saveInnerScroll` and the teardown save both refuse
  `scrollHeight <= clientHeight` geometry, and the restore observer re-applies
  `pendingRestoreTop` on content growth until it is achieved — superseded by
  any reader gesture or authored write (`positionWritten` clears it).
- **The run's top fade starts one pixel ABOVE the clip, and nothing clips
  it.** The clip's top sits on a fractional y, and the composited scroller
  clips its content to the enclosing pixel rect while the fade strip is
  antialiased at the fractional edge: the content's first pixel row showed
  through above the gradient as a bright slit that strobed under a glide
  (2026-08-22). Any overflow clip around the strip is that same antialiased
  edge and cuts the extra row back off; it needs no bound because the
  shortest run (one tool row) is taller than the strip. Both facts are
  pinned in `activityRunScroll.browser.test.ts`.
- **A notification bell with a rail member before it is ABSORBED into the
  run** (`isAbsorbedNotification`, `activityRunGrouping.ts`) rather than
  splitting it: bells land at the write head, inside the live run, and the
  split minted a fresh tail id that remounted the reader's rows from a
  one-row clip on every Monitor ping. Trailing bells stay absorbed so a
  settling run's membership never churns; an idle bell (no open run) stays
  standalone.
- **Every collapse/expand runs inside `withViewportBottomHeld`, and the hold
  lives INSIDE the registry mutators** — `setCollapsed`, `setAllCollapsed`,
  and `releaseOpenedLive` each run their write inside the transaction, so a
  caller (header, rail, the header bar's bulk toggle, the auto-collapse
  gate) just calls the mutator and must NOT wrap a hold of its own. The
  hold keeps the viewport's BOTTOM edge, so the run opens upward over rows
  the reader is already reading rather than pushing them down the page, and
  it pauses the spring so a toggle while bottom-pinned is instant instead
  of an animated ride across the delta. The one hold-free expand is
  `expandForReveal`, called only by `revealActivityRunItem`, and the missing
  hold is load-bearing: `scrollToItem` aborts its jump if the restore token
  moves during the reveal, and a hold issues one — held, every jump into a
  collapsed run would cancel at its own guard. The verb can only expand, so
  the hold-free path cannot be borrowed for a collapse.
  `docs/architecture/frontend-scroll.md` §Run Height Changes.
- **The run's header is present in BOTH states and is the per-run control.**
  It used to render only while collapsed, so expanding removed the element the
  reader had just clicked and left the invisible rail strip as the only way
  back. Do not make it conditional again — the estimate and the signature both
  assume a header that never moves, and an off-screen auto-collapse leaves the
  header exactly where the reader last saw it. The rail
  stays as a second, larger target on the same `toggle`, but **pointer-only**
  (`aria-hidden` + `tabindex="-1"`, both, since hiding a focusable element is
  its own defect): now that it duplicates a header that is always there, ARIA on
  it means a focus ring on a transparent 16px strip and the run's state announced
  twice from two buttons naming one region. The header bar's
  `activity-runs-toggle` remains the THREAD-level bulk action, rendering from
  `activityRuns.bulkCollapsed` and never from a survey of the rendered runs.
- **Nothing that changes per streaming delta belongs on the node.** The
  header's counts, failure, and running label resolve from current items through
  `components/chat/activityRunSummary.ts`; a node field would rebuild the
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
  after that. Compensation writes on a run with a controller go through
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

Diagram palettes come from `markdown/mermaidTokens.ts` — mermaid runs as
`theme: 'base'` with `themeVariables` resolved from the app's own tokens
through `utils/cssColorProbe.ts`, so a diagram is themed by the same
vocabulary as everything around it rather than by mermaid's built-in
`dark`/`default`. It is owned by `ChatMarkdown` because the Streamdown
context is created there. The resolved-config memo and the host's
`{#key}` wrapper both key on `mermaidPaletteIdentity()` — resolved mode,
sans font, and the theme system's own
`getThemePaletteIdentity()` (`uiTheme|codeTheme|revision`), which is what
makes an agent's edit to the selected theme file redraw diagrams that
already rendered. Any future palette source must widen that ONE identity,
because a second key that can disagree with it means either a stale
diagram or a remount on every tick.

Path linkification runs inside marked parsing from the server-validated
`PathRef[]` allowlist on item metadata. The generated href includes a
per-page-load nonce and is the only `agent-overflow:open` form admitted
by Streamdown's `transformUrl`. Explicit markdown-link HREFS
(`[label](/abs/file.md)`, `~/`, workspace-relative) are the deliberate
second half with a different trust model: rewritten without
render-time validation, but ONLY on surfaces that pass a
`workspacePath` (never PR/review bodies), and gated at click time by
`editor.ResolvePath` — existing regular files open from anywhere,
folder opens are refused everywhere, new files only inside the
workspace.

Copying markdown content puts TWO flavors on the clipboard, and
`utils/markdownClipboard.ts` is the only place that decides what goes
where: `text/plain` is the markdown, `text/html` is the allowlisted
rendering from `utils/markdownHtmlSerialize.ts` (lexed with the same
patched marked the renderer uses, so the flavors cannot drift from the
screen). The selection delegate (`utils/markdownCopyDelegate.ts`) writes
them through the copy event's own `DataTransfer`; a Copy button writes
them through `copyMarkdownToClipboard`, which feature-detects
`ClipboardItem` and degrades to a markdown-only `writeText` rather than
failing. Buttons opt in with `write={copyMarkdownToClipboard}` — the
markdown pipeline stays out of the primitives layer. Surfaces that
render PLAIN text (user messages, reasoning tails, `CopyFooter`
payloads) and code-block copy deliberately stay text-only: an html
flavor there would claim markup the surface never had.

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

`svelte-streamdown` is VENDORED, not installed —
`frontend/vendor/svelte-streamdown` is 3.1.2 with our divergence applied
in place (upstream is dormant; the former 13-hunk pnpm patch was pure
re-roll overhead). Parser bugs in that pipeline are fixed in the vendored
tree and recorded in its divergence ledger
([`frontend/vendor/svelte-streamdown/DIVERGENCE.md`](../../../../vendor/svelte-streamdown/DIVERGENCE.md)),
with an upstream PR when the fix is a general bug rather than a deliberate
deviation. Do not duplicate parser fixes in `markdownEnhance.ts` or the
host wrappers.
Regression coverage lives in `AssistantMessage.test.ts` and
`src/lib/markdown/`.

A raw-JSON assistant message (a schema-bound turn's answer: the body
starts with `{"`, `{}`, `[{`, `["`, `[[` or `[]`) never reaches the prose
path. `AssistantMessage.svelte` hands `ChatMarkdown` the output of
`markdown/rawJsonFence.ts` instead: a pretty-printed ```json fence whose
printer is PREFIX-STABLE (the output for any prefix of the source is a
prefix of the output for the whole source), so the code host's
incremental line rendering stays incremental while the document streams.
As prose, a 20KB single-line envelope re-paired its `_` and backtick
characters on every reveal tick and restyled up to 5KB of text the reader
had already seen (2026-08-22). Detection is that shape sniff and nothing
else — no wire flag, because Codex emits prose progress notes mid-turn in
the same schema session. Not collapsed, by ruling: it is the agent's
answer, not an artifact.

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
