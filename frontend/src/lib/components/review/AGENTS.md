# components/review/

This directory renders workspace diffs, pull or merge request detail, review
threads, CI state, and review actions. Data remains owned by the workspace and
PR stores; components own presentation and interaction.

## Identity and routing

- Resolve workspace operations from the pane's `WorkspaceRef`, including draft
  panes without a thread row.
- PR data is keyed by computer and repository identity. Route forge reads and
  mutations to that computer rather than current global focus.
- A loaded diff records the head SHA it represents in pane-local state. Derive
  staleness against the store's live head so two panes can hold different
  snapshots safely.
- Capture mutation targets before awaiting. Late replies update the captured
  PR or thread and must not navigate a pane whose target changed.

## Rendering contracts

- Keep raw patch and comment content canonical. Derive file rows, hunks,
  comments, syntax spans, and display metadata locally.
- Virtualized review lists use `LongListVirtualizer` from
  [`components/virtual/`](../virtual/AGENTS.md), so a diff or log of any
  height stays reachable. Row indices are indices of the whole list; a
  row outside the held range has no scroll offset, so reach it through
  `scrollToIndex`. Review-specific anchoring and selection stay here.
- Remember and restore reading positions as reading anchors (file, line,
  pixel delta; `utils/reviewAnchor.ts`), never as pixel offsets: a pixel
  names a different line once the held range moves.
- Preserve line identity across refreshes so open threads, selections, and
  measured rows do not move unnecessarily.
- The PR overview (`ReviewOverview`: Description and Conversation) is the
  diff list's first row (`kind: 'overview'`, key `REVIEW_OVERVIEW_ROW_KEY`),
  measured like any row and unmounted when far below, so section open
  state, section scroll offsets and the frozen feed live in the review
  store. Reading anchors name it as path `''`. `ReviewDiffBody` reports the
  row scrolling off (`onOverviewOffChange`); the title bar's peek buttons
  return through `jumpToOverview`.
- One state vocabulary per thread on every surface: classes from
  `utils/reviewThreadStyle.ts`, author names, initials and tones from
  `utils/reviewIdentity.ts` through `ReviewAvatar`. The Conversation card
  and the diff row (`ReviewPRThreadRow`) render the same card.
- A jump to a row inside a file lands under the sticky file bar: scroll it
  with `offset: -REVIEW_FILE_HEADER_BAR_PX`.
- Embedded forge HTML uses `ChatMarkdown`'s opt-in sanitized mode. Do not render
  arbitrary HTML or bypass URL transformation.
- `ReviewPane` publishes `FORGE_ATTACHMENT_SOURCE_CONTEXT`
  (`components/chat/markdown/forgeAttachmentContext.ts`) for its whole subtree;
  every `ChatMarkdown` under it renders forge-hosted attachments through
  `utils/forgeAttachments.ts`. Do not pass the PR source as a prop.
- A diff is read whole into compact storage and its text is held within the
  patch memory budget. Render evicted text as placeholders and read exact text
  through `whenResident`; see
  [review diff streaming](../../../../../docs/architecture/review-diff-streaming.md).
  Do not mount hidden diff bodies.
- Wrap the review surface in `shared/RenderBoundary.svelte`.

Review mutations reconcile from authoritative responses and show partial or
failed outcomes in the affected surface. Never optimistically erase unresolved
threads, failed comments, or stale-head warnings.

Test pure diff projection separately from component interaction. Use browser
tests for virtualized geometry, selection, sticky headers, and scroll anchoring.
