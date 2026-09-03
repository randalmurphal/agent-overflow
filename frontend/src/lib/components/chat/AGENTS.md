# components/chat/

`MessageTimeline.svelte` owns the virtualized timeline, and row
components render stable transcript records.

## Scroll contract

Read
[`frontend-scroll.md`](../../../../../docs/architecture/frontend-scroll.md)
before editing `MessageTimeline.svelte`, `ChatView.svelte`, row expansion,
load-older behavior, or anything that touches `scrollTop`.

- Take timeline position from `listRef`, never from DOM-row math:
  `findItemIndex`, `getItemOffset`, `scrollToIndex`.
- Route programmatic scrolls through `useStickToBottom` (`forceStick`,
  `markAtBottom`, `observe(kind)`, `pauseAutoScroll`, `armRestoreSnap`).
  `scrollToIndex` needs no wrapper, since the virtualizer writes through
  the chokepoint, but it is instant-only: smooth scrolling races the
  controller's tagging, so never `scrollIntoView()` a virtualized row.
- Keep `overflow-anchor: none` on the outer scroll container.
- Keep composer-clearance padding on `scrollEl`, not `contentEl`, and keep
  `ChatView`'s composer `ResizeObserver` notifying the controller after it
  writes `--composer-height`. Observe as `'composer-geometry'`, the
  live-capable path, so active output springs through activity-rail
  height changes while idle geometry still sync-pins.
- Every sibling-overlay top fade uses the global `.scroll-top-fade` rule. Its opaque
  first pixel starts one CSS pixel above the scroll clip while its bottom stays
  at the declared fade depth. WebKit can snap a composited scroll layer and an
  ancestor-painted gradient to opposite device-pixel edges, exposing a bright
  row if they merely meet. Do not inline a `top: 0` gradient beside a scroller,
  and do not clip the rule's overdraw. A header directly above one
  keeps its border in a higher stacking level, as `ChatHeader` does.

Thread-switch restore is split on purpose: `$effect.pre` arms warm-up and
restore consent before DOM flush, then the restore `$effect` calls
`forceStick({ reason: 'restore' })` and schedules one rAF
`observe('content')` settle pass. A same-thread reload watches
`pane.switchGeneration`, not only `pane.threadId`.
Scroll-session machinery lives in `timeline*.ts` siblings and
`MessageTimeline` keeps only the thin `$effect` bodies that call into
them, so new machinery goes in a sibling. Two are not obvious from their
names: `timelineRowProjection.svelte.ts` derives the nodes (grouping, the
reveal gate, rail classification), and `timelineQuietWork.ts` is one
cadence for every deferred structural pass, with geometry-mutating work
gated on "no glide running or armed". `ChatView.svelte` is split the same
way, into `editResendFlow.svelte.ts`. On edit-and-resend SUCCESS only,
that flow lands the pane at the thread's new tail with bottom-follow
engaged, the one deliberate divergence from "a send never yanks a
scrolled-up reader", because the height the reader was parked at measured
rows the revert destroyed. Failure branches leave the scroll untouched.

## Row contract

Every row rendered inside `<TimelineVirtualizer>`:

- Lives inside a `[data-row-index]` wrapper. Only `TimelineLeaf` emits
  `data-item-id`, and structural nodes such as `SubagentGroup` do not.
- Keeps its outer shell stable after first render: no static row turning
  into a button, no late chevron, no body-height animation inside the
  scroll surface, no completion-only history row.
- Puts header ACTIONS (open-in-pane, background, stop) before the meta
  columns, as `ToolHeaderMeta`'s FIRST child. Status, duration and
  timestamp render in that order on every row, so a control after the
  `<time>` shifts that column on just the rows carrying it.
- Keeps conditionally rendered header actions height-neutral across every
  state transition, proven by a real-browser height regression.
  `opacity-0` hides paint, not layout, and icon buttons in text-bearing
  headers need explicit flex geometry (`inline-flex items-center
  justify-center`) so inherited line boxes cannot grow the active row.
- Uses `TranscriptDisclosureHeader.svelte` for disclosure headers, and
  survives windowing remount, which discards row-local state.
- May be dimmed by `MessageTimeline`'s `pendingCutAfter` prop while an
  edit-and-resend revert is in flight. That is a CLASS toggle and nothing
  else, because the timeline is not truncated until the backend's
  `user_message:reverted` lands. The base `transition-opacity` there is
  unconditional, since a transition needs the property present BEFORE the
  value changes.
- **Runs no CSS transitions, and may fade but never move.** The kill rule
  in `app.css` zeroes `transition-duration` inside the scroller, Svelte
  `transition:` / `in:` / `out:` / `animate:` directives are banned in
  this directory outside a verified outside-the-scroller allowlist, and a
  keyframe animation may animate `opacity` and nothing else. The one
  carve-out is the `[data-row-index]` wrapper, which is what lets the
  pending-cut dim fade. The hazard is the scroller's DOM subtree rather
  than the directory, so check a new external row dependency by hand.
  Guards: `timelineTransitionSuppression.browser.test.ts`,
  `timelineAnimationDirectives.test.ts`,
  `timelineKeyframeAnimations.test.ts`. Why: frontend-scroll.md § The
  Print Doctrine.

The user row's body swap to `UserMessageEditor.svelte` is the one
deliberate exception to "keep the outer shell stable", scoped to
`preservePaneScrollAnchorAt`, which holds the bubble's top edge across a
height change the reader is looking straight at. The anchor row is
virtualized and CAN remount mid-edit, so everything the editor remembers
lives on the `UserMessageEditSession` that `editResendFlow.svelte.ts`
owns: draft store, dirty-check seed, upload id set, and the `ui` object
(focus intent, caret, open discard confirm, inline command error). That
editor can outlive a failed saga, because when the destructive RPC dies
with the WIRE rather than being refused the flow keeps the edit and lets
the anchor row witness which way it went. Every later exit must skip
deleting the session's uploaded attachment records, since a resend that
landed or a merged crash-copy draft may reference those ids. An orphaned
record is invisible, while deleting a referenced one corrupts a message.

Remembered row state goes in a pane registry keyed by `item.id`,
`payloadId` or `groupKey`: `pane.attachmentCacheFor`,
`pane.isSubagentGroupExpanded`, `pane.activityRuns`,
`pane.diffCardExpandedOverride`, `pane.isUserMessageExpanded`, plus
`useLeasedItemExpansion` / `useLeasedPayloadExpansion`. Use the leased
forms from components: the bare
`pane.expansionStateFor*` APIs do not survive pruning disposing the
handle. An override registry stores only a DEVIATION from the default
(`collapseDiffPreviews` for diffs, always-clamped for user messages), so
there is no clear call and no stale-override sweep.
`CommandOutput.svelte`'s dev-server URL pair is the one row-local
exception, smoothing a jittering SERVER field rather than remembering user
intent. Do not copy that shape for anything a reader chose.

Payload bytes go through `utils/payloadDataCache.ts`, keyed by
`(threadId, payloadId, version)` and byte-bounded by its LRU. Registries
track expansion intent, the data cache tracks loaded bytes. Heavy work
(Mermaid, syntax spans, KaTeX, images) stays lazy and row-local.

`UserMessage.svelte` renders the row's attachments from item meta, split
by KIND (`types/attachment.ts`; policy in
[`composer/AGENTS.md`](../composer/AGENTS.md)). An image is a tile in the
grid, numbered over IMAGES so `#2` names the same thing the message
text's `[Image #2]` does — never the array index, which a file between
them would shift. A file is an inert chip: not a button, no expand, and
its bytes are never requested, because `GetAttachmentThumbnail` errors
for one and `fetchAttachmentBytes` would be refused at the download
route. An absent `kind` in meta means
image, which is what every row written before the column carried. The
edit copy seeds files alongside images, and removing a SEEDED chip must
not delete its record — the sent message still references it (the
`shouldDeleteAttachmentRecord` rule above).

## Agent cards and the agent pane

`SubagentGroup.svelte` is the one card for every launch kind and
`components/agent/AgentPane.svelte` is the one scoped thread view
(spec: [`agent-visibility.md`](../../../../../docs/specs/agent-visibility.md)).
Launch detection is `utils/subagentLaunch.ts#subagentLaunchInfo` and
nothing else; the tree is `utils/subagentGrouping.ts`.

A Claude §E6 RESUME CARRIER is the one launch whose rows are not its
own. Claude resumes an idle async agent with a `SendMessage`, rebinds
the task onto that row, and then parents the whole resumed round —
tool calls, prose, nested launches, background Bash — to the ORIGINAL
launch, in every round. So:

- A carrier is a LIFECYCLE row: run state, elapsed, progress ticks and
  Stop read it. Identity (name, description, model) and the transcript
  read the ORIGINAL launch.
- The transcript root is `meta.transcript_root_id`, and every surface
  that OPENS or HYDRATES a scope resolves it through
  `utils/subagentLaunch.ts#agentScopeRootId` — the row door
  (`AgentRow.svelte`), the card (`SubagentGroup.svelte`, both
  `openAgentPane` and `ensureSubagentChildren`), and the tray
  (`ActivityRailBackgroundBody.svelte`). Scoping a pane to a carrier
  opens an empty transcript; hydrating one fetches nothing. The crumb
  LABEL stays whatever the caller was looking at.
- Cards slice per round: `groupItemsBySubagent` splits the root's bucket
  at each carrier's resume prompt row
  (`user:subagent-prompt:<carrierId>`), so the round-1 card shows round
  1 and each carrier's card shows its own round. Only the root's direct
  bucket is sliced — a nested launch inside a round is an anchor of its
  own. The store's decoration is sliced the same way: an anchor's
  `subagentDescendantCount` is its ROUND, and the root alone carries
  `subagentTranscriptDescendantCount` (all rounds), which is what the
  pane's hydration gate reads (`decoratedSubagentAggregates(...).transcriptCount`).
  A card gates on the round count; the pane, scoped to the root and
  showing every round, would stop fetching after round one otherwise.
- `stores/agentScopeView.svelte.ts` resolves the lifecycle row once, for
  the timeline's turn facet and the composer shell alike, so the run
  pill and the working chip cannot disagree. Elapsed counts from the
  RESUME, not from the original launch (user ruling).
- Known asymmetry: `stores/threadSubagentMemory.ts` folds an evicted row
  under its nearest launch ANCESTOR, which for a resumed round is the
  root, never the carrier. A carrier's card therefore has no fold
  aggregate of its own; its expand re-hydrates the root and the rows
  re-slice. Do not "fix" that by folding under the carrier — nothing is
  parented to one.

## Activity runs

The projection's last pass wraps consecutive activity rows into ONE
`activity_run` row: `ActivityRun.svelte`, an always-present summary header
over a height-capped clip that scrolls in place. Architecture, every term
below, and an implementation map:
[`activity-runs.md`](../../../../../docs/architecture/activity-runs.md).
The rules that bite here:
- Run state goes in `pane.activityRuns` keyed by the registry-assigned
  `runId`, never by a member item id, which changes at both window edges.
- Collapse is resolved once, by `ActivityRunIdentity.collapsedFor`, and
  `node.collapsed` is the whole answer. A `!collapsed || live` predicate
  at a consumer made the reader's collapse of the live run inert.
  Behavioral consumers key on the stamped `node.atTail`, never `live`,
  which survives only as the row's `data-live` forensic seam.
- Auto-collapse happens off-screen and instantly. The fold animation was
  built and rejected, so do not bring back a softer in-view collapse. The
  gate is a `timelineQuietWork.ts` pass and must never run while
  `autoScrollInFlight()`: its bottom-pinned restore is a direct write that
  snaps a live glide. Document hiding invalidates its cached geometry too:
  `timelineVisibilityGeometry.ts` keeps the gate closed through resume until
  the existing virtualizer subscription publishes a new visible,
  post-flush content-geometry sample. Do not replace that evidence with a
  timer, DOM read, extra observer, or visibility alone.
- Engagement is deviation-based through `pane.hasUserExpansionWithin`,
  never a rendered-height proxy: with `collapseDiffPreviews` off, diffs
  render expanded and a pixel guard would pin every run with an edit.
- Every collapse and expand runs inside `withViewportBottomHeld`, and the
  hold lives INSIDE the registry mutators, so callers just call the
  mutator. The one hold-free expand is `expandForReveal`, whose missing
  hold is load-bearing because `scrollToItem` aborts its jump if the
  restore token moves. Jumps go through `revealActivityRunItem`, which
  expands, relocates and focuses together.
- Whether a run follows its tail is STORED (`followingBottom`), never
  measured, and only a reader gesture clears it. Route every inner
  position write through `positionWritten`, never `syncPosition` alone, or
  the run pages backwards through its history, and compensate through
  `applyEngineCompensation({ kind: 'head-splice' })`: a bare `scrollTop =`
  reads as a reader gesture and escapes bottom-follow.
- A prop a run's effects branch on goes through a `$derived` primitive
  first, because `run.collapsed`, `live` and `runId` are plain reads of a
  prop the projection replaces on every streamed row. Nothing that changes
  per streaming delta belongs on the node at all: counts, failure and the
  running label resolve from current items (`activityRunSummary.ts`).
- The header renders in BOTH states and is the per-run control. The rail
  is a second, pointer-only target on the same toggle (`aria-hidden` AND
  `tabindex="-1"`).
- The overlay bar is a SIBLING of the clip, and every gesture states its
  intent through `onUserScrollStart` / `onUserScrollEnd`, or it moves the
  clip and is yanked back by the next chunk. The clip's scrollbar consumes
  zero width and takes no gutter, or the rows leave the rail. That
  geometry is browser-only (`activityRunClip.browser.test.ts`,
  `activityRunScroll.browser.test.ts`).

## Companion panes

Plan and review surfaces mount as companion panes through
`components/panes/CompanionPane.svelte`, opened by the companion and
review store helpers rather than as a sidebar inside `ChatView`. Their
bodies receive `ctx: PanelContext`, not `pane: ThreadPane`, so they cannot
subscribe to every chat tick. Inline diff affordances route into
`components/review/` through `reviewTrigger.ts` / `openReviewCompanion`.

## Markdown rendering

`ChatMarkdown.svelte` mounts the renderer from
[`lib/markdown/`](../../markdown/AGENTS.md) — first-party source, imported
through its `index.ts` barrel and nothing deeper — with host wrappers
under `markdown/` for Code, Mermaid and Math. Fix parser bugs in that
tree, never duplicating the fix in `markdownEnhance.ts` or a host
wrapper. Its area guide owns the parser map, the host seams, the
path-relative URL security boundary and the test map.

Mermaid and Math stamp their source on `data-mermaid-source` and
`data-math-source`, keeping markdown copy and diagram actions working.
Code uses a source-free `data-code-source=""` marker, because
`<code>.textContent` owns the source: never recover it from the marker.

A footnote body is the one thing NOT recoverable from the DOM: a `[^1]:
body` definition renders nothing, and per-block lexing leaves the ref
token's back-reference empty. The chip publishes
`data-footnote-label`, `markdown/footnoteDefinitions.ts` resolves it
against the source each `.markdown-body` registers, and
`FootnotePopoverHost.svelte` — one app-level instance, delegated
click and hover listeners, one `primitives/Popover.svelte` — shows the
body. Hover previews, click pins. The lookup runs at open time and
never during render.

The direct assistant prose reveal
(`markdown/streamingAssistantLiteralOwner.ts`) is the SINGLE owner of the
active literal host's visible text, a correctness boundary: the host
renders empty and hands the element over. The
invariant is **extend-only**. A reveal delta extends the visible string,
an authoritative parser update either extends it or replaces it in ONE
`replaceChildren` mutation, and a fallback relinquishes the RUN without
deleting visible bytes. Never reintroduce a delete-then-recreate path and
never fix a rollback with timing
(`ChatMarkdown.directRevealMonotonic.browser.test.ts`). The owner cuts
Text nodes only at `Intl.Segmenter` word boundaries, because Blink shapes
adjacent Text nodes as separate runs and can expose a detached combining
mark or broken emoji join.

Path linkification runs inside markdown parsing from the server-validated
`PathRef[]` allowlist on item metadata, and the generated href carries a
per-page-load nonce that is the only `agent-overflow:open` form
`transformUrl` admits. Explicit markdown-link hrefs are the second half,
with a different trust model: rewritten without render-time validation,
but ONLY on surfaces that pass a `workspacePath` (never PR or review
bodies), and gated at click time by `editor.ResolvePath`.

A `localhost:<port>` link is rewritten the same way and for a related
reason: it names a listener on the machine the agent runs on, so read
anywhere else it points at the reader's own machine. `ChatMarkdown` takes a
`threadId` and builds `utils/previewLinkExtension.ts` from
`stores/devServers#previewLinkTargetFor`, which answers null on the owner's
own screen and until that machine has sent a list. Registered AHEAD of the
path-link extension, which claims every `[…](…)` link it is offered.

The anchor keeps the agent's href and carries `data-preview-state`
(`open` / `not-shared` / `no-address`), `data-preview-port`,
`data-preview-path`, `data-preview-thread`, `data-preview-machine` and
`data-preview-via`; the `not-shared` state may be followed by a
`[data-preview-allow]` button. `utils/externalLinks.ts`'s delegate is the
only reader: it swallows the click in EVERY state — following it would
load whatever answers on that port here — and only `open` mints. Both
renderers spell the anchor from `markdown/render/previewLink.ts`, and the
class is `.preview-link` with the `via <machine>` marker as a CSS
`::after`, never a second element.

The two actions the delegate calls are taken by REGISTRATION
(`installPreviewLinkActions`, armed and disarmed by `initDevServers`),
because the store opens a minted URL through `handleExternalURL` and an
import back would close a ring around two modules that both run at boot.

`DevServerChip` follows the same split. On the owner's own screen it is
gated by the loopback probe; anywhere else that probe is asking the wrong
computer, so the loop does not start and `previewChipFor` reads the
machine's own list instead. A browser tool row (`meta.mcp.server ===
BROWSER_TOOLS_SERVER`, `utils/browserTools.ts`) names its machine in
`GenericToolCallRow`'s header actions for the same reason: off that
machine, the row is the only sign the page exists.

Copying puts TWO flavors on the clipboard and `utils/markdownClipboard.ts`
is the only place that decides what goes where: `text/plain` is the
markdown, `text/html` the allowlisted rendering from
`utils/markdownHtmlSerialize.ts`, lexed with the same patched marked the
renderer uses so the flavors cannot drift from the screen. Buttons opt in
with `write={copyMarkdownToClipboard}`, which keeps the pipeline out of
primitives. Plain-text surfaces and code-block copy stay text-only.

Code-block spans come from the backend (`HighlightCode` via
`markdown/codeSpanCache.ts`), with remote clients also warming the cache
from hash-verified `highlight:seed` pushes
(`markdown/liveCodeSeeds.svelte.ts`). Settled history rows skip the RPC:
`AssistantMessage.svelte` and `DiffFileStack.svelte` ingest persisted,
version-stamped span blobs through `utils/persistedSpans.ts` synchronously
at component init, before the hosts take their first cache reads. A stale
blob is dropped and recomputed, and the ingest is not memoized, because
thread switches evict the caches.

A raw-JSON assistant message never reaches the prose path.
`AssistantMessage.svelte` hands `ChatMarkdown` the output of
`markdown/rawJsonFence.ts`: a pretty-printed json fence whose printer is
PREFIX-STABLE, so incremental line rendering stays incremental while the
document streams. As prose, a 20KB single-line envelope restyled 5KB of
already-read text per reveal tick (2026-08-22). Detection is a shape
sniff, because Codex emits prose progress notes in the same session.

## Test notes

happy-dom reports zero geometry, so `MessageTimeline.svelte` enables the
virtualizer's `renderAll` seam only under happy-dom runs (`MODE === 'test'`
AND the `window.happyDOM` marker). The Chromium project keeps real
windowing, and counts row unmounts.
Layout invariants happy-dom cannot see (margin containment, flush,
oscillation geometry) run in the real-Chromium `browser` vitest project,
in files named `*.browser.test.ts`, and a cascade-coupled one imports the
production `app.css`. `pnpm test` runs the unit project only, so the gate
needs no browser binary, and `pnpm test:browser` needs
`pnpm exec playwright install chromium`. MessageTimeline-level behavior
belongs in `scroll.test.ts`, row behavior in row tests.
