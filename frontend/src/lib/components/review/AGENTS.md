# components/review/

This directory renders workspace diffs, pull or merge request detail, review
threads, CI state, and review actions. Data remains owned by the workspace and
PR stores; components own presentation and interaction.

## Identity and routing

- Resolve workspace operations from the pane's `WorkspaceRef`, including draft
  panes without a thread row.
- PR data is keyed by computer and repository identity. Route forge reads and
  mutations to that computer rather than current global focus.
- A loaded diff records the PR revision (head and base tip) it represents in
  pane-local state. Derive staleness against the store's live head so two
  panes can hold different snapshots safely. PR reads take that revision,
  never a branch name the backend resolves on its own.
- Capture mutation targets before awaiting. Late replies update the captured
  PR or thread and must not navigate a pane whose target changed.
- A review is a companion of a thread pane (`ctx`, `reviewSubjectForPane`)
  or pinned to one PR with no thread (`pr`, `reviewSubjectForPR`, the Pull
  requests page). A pinned review's workspace is the project root, its
  scope stays `pr`, and its PR, computer and `git:operate` check are the
  ones it was opened with, never the owner pane's git status, thread or
  composer. Its gates read the state (`pinned`, `commentsGranted`,
  `canDraftComments`), not `ctx`: no scope selector, Edits, Send to chat
  or agent target. Its drafts have no thread row, so they are held per PR
  in `stores/reviewPRDrafts.svelte.ts` (not persisted) and post as a PR
  review. The owner keys `ReviewPane` on the PR; `reviewStateForPR` keeps
  the selected PR's state and the one before it per owner pane, and the
  owner calls `disposePRReviewStates` when its page goes.

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
- The PR overview (`ReviewOverview`: Merge, Description and Conversation)
  is the diff list's first row (`kind: 'overview'`, key
  `REVIEW_OVERVIEW_ROW_KEY`) for every PR, measured like any row and
  unmounted when far below, so section open state, section scroll offsets
  and the frozen feed live in the review store. The Merge section
  (`ReviewMergeSection`) is the one home of approve, merge and auto-merge
  on every surface: the title bar's refs line carries only the readiness
  pill, which jumps to it, and both read `utils/reviewReadiness.ts` so
  they never disagree. Its actions go through the store
  (`approvePR`, `mergePR`, …), which holds `mergeAction` pending until the
  PR's applied seq reaches the one the action answered with (the pump's
  seq after its poll) and toasts a failure as the
  backend's one sentence (code `forge_<kind>`; `forge_stale_head` offers
  Reload). Never render merged, approved or armed state from a call's
  success; the forge's answer arrives on the snapshot. Reading anchors
  name it as path `''`. `ReviewDiffBody` reports the row scrolling off (`onOverviewOffChange`); the title bar's peek buttons
  return through `jumpToOverview`. With a commit picked the lead row is
  the commit card instead, which has no peek and no conversation to jump
  to: `overviewOff` stays false and thread rows offer no conversation jump.
- One state vocabulary per thread on every surface: classes from
  `utils/reviewThreadStyle.ts` (`reviewThreadState` is unresolved,
  resolved or none; `reviewThreadOutdated` is a marker layered on it,
  never a state, so counts, the stepper and Resolve treat an outdated
  thread by its resolve state), author names, initials and tones from
  `utils/reviewIdentity.ts` through `ReviewAvatar`. The Conversation card
  and the diff row (`ReviewPRThreadRow`) render the same card.
- The toolbar (`ReviewToolbar`) holds the scope selectors, the diff's
  counts, collapse-all, the view-options menu (whitespace, wrap, split),
  reload and close (Back on a pinned review); no
  comment tally (the tree badges, the Conversation header and the title
  bar's stepper carry counts). Every selector is `ReviewToolbarMenu`, a
  bordered trigger over the shared `Menu`; a trigger never truncates on
  its own, a long value is shortened in the middle with the whole value as
  hover text. The commit picker (`ReviewCommitPicker`) reads `All commits`
  or `N/M <shortSha>` and never the subject; the subject, author, age and
  body live in the menu rows and in the commit card (`ReviewCommitCard`),
  which takes the overview's place as the diff list's lead row
  (`lead: 'commit'`, key `REVIEW_COMMIT_CARD_ROW_KEY`) while a commit is
  picked, so the fixed chrome never changes height for it. The
  file-tree toggle lives in the rail (`ReviewRail`: the tree's header when
  open, a strip when closed; on compact the open rail covers the diff and
  a pick closes it). The conversation refresh is on the Conversation
  section's header. The PR title bar (`ReviewPRTitleBar`) is four
  fixed-minimum-height lines: title, author and state, refs and verdicts,
  CI with its refresh inside the wrapping chip row.
- A jump to a row inside a file lands under the sticky file bar: scroll it
  with `offset: -REVIEW_FILE_HEADER_BAR_PX`.
- A jump to a thread pulses the destination card once and leaves nothing
  lit: the jump's owner (`ReviewDiffBody` for rows, `ReviewConversation`
  for cards) holds the target and a generation for `REVIEW_JUMP_PULSE_MS`
  and the card runs `utils/reviewJumpPulse.ts`. Do not render a standing
  highlight from the stepper's `unresolvedCursor`.
- The unresolved stepper on the title bar and the Conversation header is
  `ReviewUnresolvedStepper`, one segmented control under one border.
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
