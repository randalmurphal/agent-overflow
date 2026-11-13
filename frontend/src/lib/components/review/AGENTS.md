# components/review/

This directory renders workspace diffs, pull or merge request detail, review
threads, CI state, and review actions. Data remains owned by the workspace and
PR stores; components own presentation and interaction.

## Identity and routing

- Resolve workspace operations from the pane's `WorkspaceRef`, including draft
  panes without a thread row.
- PR data is keyed by computer and repository identity. Route forge reads and
  mutations to that computer rather than current global focus.
- A loaded diff records the PR revision (head and recorded base) it represents in
  pane-local state. Derive staleness against the store's live head so two
  panes can hold different snapshots safely. PR reads take that revision,
  never a branch name the backend resolves on its own. The conflict read
  is the one exception: the recorded base does not follow the branch, so
  `GetPRMergeConflicts` reads the base branch's tip from origin itself, and every
  open of the conflict view reads again.
- Capture mutation targets before awaiting. Late replies update the captured
  PR or thread and must not navigate a pane whose target changed.
- A review is a companion of a thread pane (`ctx`, `reviewSubjectForPane`)
  or pinned to one PR with no thread (`pr`, `reviewSubjectForPR`, the Pull
  requests page). A pinned review's workspace is the project root, its
  scope stays `pr`, and its PR, computer and `git:operate` check are the
  ones it was opened with, never the owner pane's git status, thread or
  composer. Its gates read the state (`pinned`, `commentsGranted`,
  `canDraftComments`), not `ctx`: no scope selector, Edits, Send to chat
  or agent target. Its drafts have no thread row, so they are the PR's
  (`stores/prDiffReviewComments.svelte.ts`, persisted under the PR on its
  computer) and post as a PR review or are sent to a thread. The owner keys `ReviewPane` on the PR; `reviewStateForPR` (`stores/reviewPanePinned.svelte.ts`) keeps
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
- The PR overview (`ReviewOverview`: Merge, Details, Description and Conversation)
  is the diff list's first row (`kind: 'overview'`, key
  `REVIEW_OVERVIEW_ROW_KEY`) for every PR, measured like any row and
  unmounted when far below, so section open state, section scroll offsets
  and the Conversation feed's order, arrivals and reading anchor live in
  the review store (`stores/reviewConversationFeed.svelte.ts`). The feed
  never gates an arrival: it joins at its place, and `ReviewConversation`
  holds the reader's top card to the pixel by compensating `scrollTop`
  (the section body has `overflow-anchor: none`, as WebKit has none); the
  contract is in
  [pull requests](../../../../../docs/specs/pull-requests.md#the-conversation-feed). The Merge section
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
  counts, collapse-all, the view-options menu (whitespace, wrap, split,
  and in the PR scope Hide resolved threads, `stores/reviewHideResolved.svelte.ts`,
  whose `shownPRThreads` the diff rows and find read),
  reload and close (Back on a pinned review); no
  comment tally (the tree badges, the Conversation header and the title
  bar's stepper carry counts). Every selector is `ReviewToolbarMenu`, a
  bordered trigger over the shared `Menu`; a trigger never truncates on
  its own, a long value is shortened in the middle with the whole value as
  hover text. The commit picker (`ReviewCommitPicker`) reads `All commits`,
  `N/T <shortSha>` or a run's `N–M/T`, numbered from the PR's first
  commit by the backend's count even when only the newest are listed
  (`utils/commitRange.ts`), and never the subject; the subject, author, age
  and body live in the menu rows and in the commit card (`ReviewCommitCard`)
  or a run's card (`ReviewRangeCard`), which takes the overview's place as
  the diff list's lead row (`lead: 'commit'`, key
  `REVIEW_COMMIT_CARD_ROW_KEY`) while a commit or run is picked, so the
  fixed chrome never changes height for it. A run that ends at the head
  reads each line's comment home from the whole PR's diff index
  (`stores/reviewPRLineIndex.svelte.ts`, `stores/reviewRangeComments.ts`);
  the line's `+` carries the answer (`lineComment`). The
  file-tree toggle lives in the rail (`ReviewRail`: the tree's header when
  open, a strip when closed; on compact the open rail covers the diff and
  a pick closes it). Back steps out of the CI log, the conflict view and
  the compact rail before the review: `ReviewPane` registers that step
  under its host pane (`stores/paneBackSteps.ts`), which the companion
  rung of `utils/stepBack.ts` and the Pull requests page run first. The
  conversation refresh is on the Conversation section's header. The PR title bar (`ReviewPRTitleBar`) is four
  fixed-minimum-height lines: title, author and state, refs and verdicts,
  CI with its pipeline actions menu and refresh inside the wrapping chip
  row. The CI actions contract is in
  [pull requests](../../../../../docs/specs/pull-requests.md#ci-actions).
  The management controls (title and description editors, Details and
  its pickers, the title bar's `⋯`, Dismiss, Bypass and the commit
  message editor) hold their text, the merge method and the pending
  action in `stores/prManage.svelte.ts`, captured for the PR they opened
  on and discarded when the pane's PR changes; Details is collapsed by
  default and an open picker applies on any close, the pane's included.
  The contract is in
  [pull requests](../../../../../docs/specs/pull-requests.md#managing-a-pr).
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
- Find paints only text inside a `data-find-key` element, keyed as
  `utils/reviewFindScan.ts` and `utils/ciLogFind.ts` key their hits. A PR
  thread comment is keyed by its thread and `reviewCommentKey` (its forge
  id), never its position in the thread. A row
  that renders searched text carries the matching key; text the scanner
  skips (the +/- gutter, the log's clip note, any chrome inside a keyed
  element) carries `data-find-skip`. A split view keys a context line on
  its new side only. A draft or a sent comment open in its editor is not
  counted, mounted or not: find reads the open edits from the review
  state.
- An open comment editor (a new comment, a draft's edit, a thread reply,
  a sent comment's edit, the Conversation's PR comment field) is a
  virtualized row, so its unsaved text and open state live in the
  review state (`draftBodyFor`, `draftEditBodyFor`, `replyBodyFor`,
  `replyOpen`, `sentComments`) until it is saved or cancelled. All of
  them render `ReviewCommentEditorShell`, and a sent comment's header
  actions are one `ReviewCommentActions` taking a list of actions. Closing a reply (Cancel,
  Escape, Back) keeps its text for the next open; only a sent reply
  clears it. A reload of the same subject keeps every open editor. A new
  comment's editor stays on its line only by exact identity
  (`utils/editorPlacement.ts`); otherwise it is detached: in place in
  its file after a suggested row, with "Use this line" and "Move", and
  `createComment` refuses it. Only an editor whose file is not in the
  loaded diff shows in the pane-level group, to copy or discard; a
  collapsed or filtered file hides its editors with it. The contract is
  in [review pane design](../../../../../docs/architecture/review-pane-design.md#open-new-comment-editors).
  Every editor's Preview is `ReviewPreviewToggle` in its button row and
  `ReviewEditorPreview` in the box's place at the box's height, its
  state in `editorPreviews` under the editor's key. A sent comment's body
  renders through `ReviewCommentMarkdown` (one document; the fences at
  the lines the backend reports, never parsed here, claimed as
  `ReviewSuggestionDiff`, the reaction chips under it), and its actions are Add reaction, Copy
  link, Edit, Delete in that order: hover icons on desktop, one `⋯` menu
  (and the long press) on compact. A
  reaction or Apply never shows before the forge's answer
  ([pull requests](../../../../../docs/specs/pull-requests.md#suggestions-reactions-and-preview)).
  Rows an expansion supplied carry no version proof; verified expansions
  are a backend change and out of scope here.
- A binary file, or a text SVG shown `Rendered`, is one media row
  (`kind: 'media'`, `ReviewImageDiff`) at a height reserved from the image
  area's bound (`utils/reviewImageRows.ts`). Its sizes and bytes come from
  the pane's `ReviewDiffMedia` (`stores/reviewDiffMedia.svelte.ts`) while
  the row is mounted; every backend read of a diff's files, and what a
  refusal offers (Retry or reloading the diff), is decided in
  `stores/reviewDiffFiles.ts` and nowhere else. The conflict view passes
  no media host and keeps git's line.
- The number gutter's context menu (`ReviewLineMenu`, the PR's whole
  diff only) copies the line's anchor on the forge's diff page from
  `utils/reviewLineLink.ts`; elsewhere the native menu stays.
- Wrap the review surface in `shared/RenderBoundary.svelte`.

Review mutations reconcile from authoritative responses and show partial or
failed outcomes in the affected surface. Never optimistically erase unresolved
threads, failed comments, or stale-head warnings. What a PR review submit
did comes only from the backend (`settleSubmit`): its answer, or after a
lost answer the outcome of its attempt id; never from the state of drafts
anyone can send. No submit starts while a lost one's outcome is unknown.

Test pure diff projection separately from component interaction. Use browser
tests for virtualized geometry, selection, sticky headers, and scroll anchoring.
