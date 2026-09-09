# components/chat/

`MessageTimeline.svelte` owns the virtualized transcript. Row components render
stable transcript records; `ChatView.svelte` composes the timeline, header,
composer, terminal placement, and companion panes.

## Scroll ownership

Read [`frontend-scroll.md`](../../../../../docs/architecture/frontend-scroll.md)
before changing the timeline, expansion, paging, restore, or any `scrollTop`
behavior.

- Read virtual position through the virtualizer handle: `findItemIndex`,
  `getItemOffset`, and `scrollToIndex`. Do not derive it from mounted rows.
- Route programmatic timeline motion through `useStickToBottom`. The virtualizer
  feeds its targets through the same scroll writer.
- Keep `overflow-anchor: none` on the outer scroller. Composer-clearance padding
  belongs on that scroller, and composer resize is observed as
  `composer-geometry`.
- Reader escape comes from input events, not geometry. Re-engagement requires
  explicit intent or the controller's documented bottom-restick rule.
- Scroll motion integrates CSS distance over elapsed time, then samples the
  browser's measured write grid. Preserve fractional progress and do not count
  frames or infer the grid from DPR.
- Retire completed or escaped motion tokens. Callbacks from an older token must
  never alter a newer motion.
- Use the global `.scroll-top-fade` beside timeline scrollers; preserve its
  overdraw and stacking contract.

Keep scroll state machines in `timeline*.ts` modules and thin reactive effects
in `MessageTimeline`. Generic measurement and windowing belong to
[`components/virtual/`](../virtual/AGENTS.md).

## Rows and transcript identity

- Discriminate records by `kind`, and preserve `(turnIndex, itemIndex)` ordering.
- Row identity must survive streaming updates. Keep local presentation state in
  `rowState.ts` or another entity-keyed owner rather than DOM position.
- Heavy output, diffs, thinking, and subagent bodies load on expansion. Keep
  collapsed rows cheap and accessible.
- A settled row must stop timers, observers, and live-only rendering work.
- Model-authored repeated list keys use `utils/uniqueEachKeys.ts`; unexpected
  duplicate entity keys remain reportable defects.
- New row families need settled, streaming, error, expansion, and remount
  coverage when those states apply.

Activity runs are a single transcript row with one summary/header owner. Their
collapse and height changes must preserve reader position and transfer tail
follow intent through the timeline controller. Do not add row-local scroll
writes or independent animation loops.

## Edit and resend

Read `editResendFlow.svelte.ts`, `userMessageActions.ts`, and
`preserveScrollAnchor.ts` before changing past-message editing.

- `UserMessageEditSession` owns the local draft, seed, uploads, and row UI for
  the entire flow so virtualizer remounts cannot discard an edit.
- Use `preservePaneScrollAnchorAt` for the editor/body swap and other row-height
  changes. Pass the element whose top edge the reader is following.
- `pendingCutAfter` dims only rows the in-flight revert would remove. Do not
  truncate the local projection before the backend event arrives.
- Only a successful destructive replacement calls `stickToLatest`. A refusal or
  failed send preserves the reader's scroll position while returning the text to
  an actionable editor or composer.
- The editor may delete only attachments uploaded by that edit session. Once a
  send outcome is unknown, later exits retain those records because a delivered
  message or recovered draft may reference them.

## Companions and agent surfaces

Review, plan, browser, and agent panes are companions selected through the pane
layout. Opening one must preserve the thread pane and target the current pane
explicitly. Keep lazy component promises stable so unrelated updates do not
remount a companion.

Agent and subagent cards render provider records; they do not recreate provider
lifecycle state. Resolve controls from the owning thread and computer, including
for threads hidden from the sidebar.

## Markdown

`ChatMarkdown.svelte` hosts the first-party renderer in
[`lib/markdown/`](../../markdown/AGENTS.md). Parser and renderer bugs are fixed
there. Chat owns host integrations: path and preview links, code and diagram
islands, footnote popovers, selection preservation, and streamed reveal.

- Agent chat does not enable embedded HTML. Forge-authored review content may
  opt into the renderer's sanitized embedded-HTML mode.
- Path and preview behavior is implemented as parser extensions so both compact
  static and component render paths make the same decision.
- The streaming literal has one imperative DOM owner. Ownership transfers must
  preserve extend-only visible text and selection until genuine divergence.
- Keep completed Markdown blocks stable. Do not rebuild sealed DOM when only the
  volatile tail changes.

Use differential tests for both renderer paths and browser tests for DOM
identity, selection, geometry, or actual scroll behavior.
