# components/review/

The review pane: the app's ONE full-diff surface (the RHS sidebar/drawer
system it replaced is deleted). Mounts as a `review` companion pane next to
its source thread pane; open it via `openReviewCompanion` /
`pane.toggleReviewPane`, never by mounting it inside chat.

## Layout

| File | Role |
|---|---|
| `ReviewPane.svelte` | Shell: scope/branch/commit/edit selectors, tree/split/wrap toggles, error + send strip, snippet wiring. |
| `ReviewDiffBody.svelte` | The continuous virtualized surface: one `TimelineVirtualizer` over the flat row model, sticky overlay file header, keyboard (j/k files, n/p comments, c file-level comment), jump-to-file. |
| `ReviewRail.svelte` | The left rail shell: Files \| Comments tabs, resizable width persisted via appStorage `reviewTreeWidth`. Tab state is owned by `ReviewPane` (the toolbar comment tally switches to the Comments tab). |
| `ReviewFileTree.svelte` | Files tab: GitHub-style tree (`utils/reviewTree.ts`), click-to-jump, top-file highlight, per-file comment-count badges, a search box plus an extension-filter dropdown (funnel button right of the search box, multi-select `Menu` of file-type options with counts). The extension set filters the rail; the dropdown's "Apply filter to diff" checkbox extends it to the diff body (state owned by `ReviewPane`, which derives the `diffFiles` subset and maps top-file highlight indexes back to the full list). The text search stays rail-only. |
| `ReviewCommentsList.svelte` | Comments tab: every PR thread (file-anchored AND PR-level conversation — `ReviewThread.path === ''`, listed under a "Conversation" group first) + local draft grouped by file in diff order (`utils/reviewComments.ts`), actionable (unresolved/draft) first. The snippet is the row's primary text (markdown-stripped, bot badge lines skipped — see `commentSnippet`); author/line/state sit on a small meta line, full author on hover. Non-resolvable conversation comments get the neutral `comment` state (info dot, excluded from the unresolved tally). Clicking an in-diff item jumps the diff body to the row and flash-highlights it (`jumpToComment` on the store → `pendingJumpRowKey` → `ReviewDiffBody.jumpToRowKey`); items with no diff row (conversation threads, files outside the diff) expand inline instead, bodies rendered via `ChatMarkdown` (as a sibling of the row button — links must not nest in a button). PR scope shows review-verdict summaries on top. |
| `ReviewFileHeaderRow/ReviewLineBlockRow.svelte` | Row renderers. Header/line rows are EXACT-height (px-pinned from `utils/reviewRows.ts` constants). A collapsed file is its header row alone (chevron + counts) — there is no collapsed body row; the toolbar's collapse-all/expand-all toggle flips the whole surface. |
| `ReviewDraftEditor/ReviewCommentThread.svelte` | Comment rows (measured, not exact). |
| `ReviewPRHeader.svelte` | PR-scope header (title/author/verdicts/mergeability badge + description), normal flow above the diff body, not a virtual row. The CI chips are the ONLY checks surface (the old ✓/✗/● summary button is deleted). The state badge and base←head refs live in the `ReviewPane` toolbar stats area (`review-pr-meta`), not here — the toolbar's local-diff +/- is the only additions/deletions readout. |
| `ReviewPRThreadRow.svelte` | Incoming PR review-thread row (measured): comments, reply composer, send-to-agent. Reply text is store-backed. |
| `reviewScroll.ts` | The pane's single scrollTop writer + per-(thread,scope,geometry) position memory. The conflict view passes `scope:conflicts` so its position doesn't clobber the diff's. |
| `ReviewCIChips.svelte` | PR-scope pipeline chips on the header meta line: one per stage (GitLab) / workflow (GitHub) with a status dot, hover tally, and a job dropdown. Jobs with fetchable logs open the log view; external checks link out. |
| `ReviewCILogView.svelte` | CI job log view — replaces the diff body (conflict-viewer pattern, Back button). Virtualized ANSI log chunks (bottom-anchored on load, tail-capped by the backend), per-step status strip (GitHub), Refresh / Save-to-file / Send-to-chat toolbar. |

The PR-scope conflict viewer (`git merge-tree`, local clone required)
renders through the same `ReviewDiffBody` with `utils/conflictFile.ts`
pseudo-files: the merged blob's conflict regions become a pseudo-diff
(ours → `del`, theirs → `add`, so split view shows ours|theirs side by
side), marker lines render as visible unnumbered `marker` rows relabeled
with the base/head labels, and non-conflict runs fold to a few context
lines around each conflict — fold rows expand via
`expandConflictFold(path, foldId)` on the store (ids are stable per
file). Line numbers flow through synthetic `@@` headers. merge-tree's
informational messages are attributed to their file Go-side
(`MergeTreeResult.Notes`; redundant "CONFLICT (content)" /
"Auto-merging" lines are dropped) and render as marker rows at the top
of that file's body — the only signal for modify/delete-style
conflicts. A file with notes but no textual regions gets a structural
badge (`PatchFile.conflictLabel`, e.g. "modify/delete") in place of
the conflict-count badge, and expands even when its content is
unfetchable (the path may not exist in the merged tree). Messages
naming no conflicted path (rare) fall back to a strip above the diff
body. The surface is deliberately read-only — no comment anchors,
drafts, or PR-thread rows on conflict content. Files open EXPANDED
(content loads fan out in parallel via `GetMergeConflictFile` inside
`openConflictView`); a file whose load fails with no notes stays
collapsed.

The PR-scope CI surface (`GetPRCIJobs` / `GetPRCIJobLog` /
`SavePRCIJobLog`, normalized in `internal/git/ci.go`) loads lazily with
the PR detail and refreshes on the same `pr:updated` pump — no separate
poll. The log view is read-only and in-memory; "Save to file" writes the
full log under the app-managed `ci-logs/` dir and "Send to chat"
prefills the source pane's composer with a path reference (never
auto-sends). The log wire payload is tail-capped (2 MB) because failures
read tail-first.

Data orchestration lives in `stores/reviewPane.svelte.ts` (per-source-pane
state registry); the row model in `utils/reviewRows.ts`.

## Contracts that bite

- **Exact-height rows**: with word wrap off, line blocks render at exactly
  `REVIEW_LINE_HEIGHT_PX` per visual line and headers at
  `REVIEW_FILE_HEADER_PX` — the engine skips their ResizeObservers
  (`RowEstimate.isExact`), so ANY vertical padding/border/rem-based height
  drift misplaces every row below. Heights are px-pinned from the shared
  constants; keep them that way. The between-files separation gap is
  painted INSIDE the header row (`REVIEW_FILE_GAP_PX` band +
  `REVIEW_FILE_HEADER_BAR_PX` bar = `REVIEW_FILE_HEADER_PX`); the sticky
  overlay renders the bar alone (`overlay` prop) and appears only once
  the BAR — not the gap — passes the viewport top. The last file's slab
  closes with an exact-height `surface-end` row (`REVIEW_SURFACE_END_PX`
  hairline). Hunk-gap bands are ordinary display rows inside line blocks
  (one line tall, no borders) — same exactness rules.
- **Hunk-gap expansion**: `buildPatchDisplayRows` emits `gap` rows
  (leading/between/trailing, new-side coordinates) for the unchanged
  runs hidden between hunks; conflict pseudo-files suppress them (their
  folds already cover this). Expand clicks flow
  `ReviewLineBlockRow.onExpandGap` → `store.expandDiffContext` →
  `GetDiffContextLines` (new side only — expanded context is identical
  on both sides; per-scope source resolution lives in
  `app_diff_context.go`) → `utils/diffContextExpansion.ts` merges the
  fetched lines into the parsed file as context rows and rewrites hunk
  headers. Expansion state is per pane and CLEARED on every reload (a
  fresh patch can renumber everything); the rebuild memo is keyed by
  the expansion STATE (two panes expanding identical patch text share
  the parsed base array but never a memo slot), with globally unique
  `nextExpansionVersion()` stamps for change detection. Always derive rows
  via `filePatchDisplayRows(file)` (carries `newSideTotal`, one memo
  entry per file) and skip `row.gap` in anchor/excerpt walks.
  Fetched context lines keep a stable `PatchLine` identity across
  rebuilds and each rebuilt array records its predecessor
  (`expansionPredecessor`), so `getSpansForLine` keeps serving the
  superseded array's syntax spans for shared lines while the expanded
  file's own highlight request is in flight — expanding must never
  flash already-colored lines plain.
- **Estimate coherence**: `ReviewDiffBody` hands the engine a stable
  wrapper that reads the current `$derived` build; `viewMode`/`wordWrap`
  changes remount the virtualizer via `{#key}` (exactness is
  constructor-once). Don't "optimize" the wrapper away.
- **Scroll ownership**: `reviewScroll.ts` is the only scrollTop writer.
  It deliberately does NOT use `utils/scroll/` (no springs/bottom-pin
  here); see its header comment and `frontend-scroll.md`.
- **Reading anchor — content never moves the reader**: refreshes are
  opt-in (refresh button, PR stale banner; there is NO auto-reload on
  workspace change — only a stale dot on the refresh button fed by the
  source pane's gitwatch slot), and when content DOES change under the
  same scroll key (reload, gap expansion, PR poll replacing thread
  rows) `ReviewDiffBody` restores the top-of-viewport anchor — (file,
  line, pixel delta), math in `utils/reviewAnchor.ts` — instead of
  keeping the raw scrollTop. scrollTop 0 is unanchored by design (the
  top stays the top). User-driven rebuilds (collapse toggles, editor
  opens) re-anchor nothing: they're detected by files/prThreads
  identity, which those don't change. User collapse choices are
  overrides (`collapseOverrides` in the store) that survive reloads;
  defaults only apply to untouched paths and overrides reset on scope
  switch. PR scope additionally has two partial refreshes that leave
  the diff alone: `refreshPRThreads` (detail + review threads; a moved
  head raises the stale banner) and `loadCIJobs` (CI chips button).
- **Row state must survive windowing**: rows unmount ~1800px offscreen.
  Draft-editor text lives in the store (`draftBodyFor`/`setDraftBody`),
  focus is a one-shot store request (`consumeDraftEditorFocus`). Never
  hold user input in row-local `$state`.
- **Comment sourceKey** is a content hash of the patch
  (`utils/diffSourceKey.ts`); drafts persist in SQLite via the
  `diff_review_comments` bindings and batch-send through
  `SendDiffReviewComments`. EXCEPTION: pr scope uses the stable key
  `pr:{forge}:{namespace}/{repo}:{number}` so drafts survive head pushes;
  each draft's `commitSha` records what it was anchored to, and orphan
  detection (drafts whose line left the diff) excludes them from PR
  submission without deleting them.
- **PR diff source**: `GetPRDiff(threadId, pr, baseRef)` prefers a
  locally-computed three-dot diff (`git diff --merge-base origin/<base>
  <fetched-head-oid>`) when the thread has a clone — gh/glab's PR-diff
  endpoints refuse diffs over 20k lines (HTTP 406), which large PRs blow
  past. The forge API is the fallback for pr-anchor threads with no local
  checkout. `loadPRPatch` sequences the subscription BEFORE the diff (not
  parallel) because the base ref only lands with the PR detail.
- **PR subscription lifecycle**: entering pr scope opens a Go-side poll
  pump (`SubscribePRUpdates`); the pane state OWNS that subscription.
  Every exit path must unsubscribe exactly once — scope switch, pane
  dispose, a superseded/late-resolving reload, and
  `reviewStateForPane` replacing a thread-mismatched state all do. If
  you add a new path that can drop a state or abandon a load, close its
  subscription or the pump polls `gh`/`glab` until the connection dies.
  `pr:updated` events route by subscription id
  (`stores/eventsPRReview.ts`); a moved head sets `prStale` (banner) and
  never swaps the diff out from under the user. A hidden document
  (minimized window, background tab) pauses every live pump via
  `SetPRUpdatesActive` — the store's module-level `visibilitychange`
  listener owns that flip, and resume catch-up-polls only when a tick
  was missed. Pausing suspends fetches without releasing ownership;
  the unsubscribe-exactly-once rule above is unaffected.

- **Edits scope** renders persisted tool-call diff payloads — the
  historical change itself, correct after commits/rebases — never a
  git recomputation. `ListThreadEditDiffs` lists metadata only; the
  selected diff loads via `GetPayloadData` (single edit) or
  `GetTurnEditsDiff` (a turn's payloads concatenated in item order —
  sequential story, NOT a net diff: a file edited twice keeps both
  sections, merged into ONE PatchFile as consecutive hunks
  (`mergePatchFilesByPath` — the surface keys rows/tree/collapse by
  path, and duplicate paths crash the keyed each)). The selector is
  turn-grouped (`optgroup` per turn, whole-turn
  option first, labels from the turn's first user prompt). sourceKeys:
  `edit:<payloadId>` / `edit-turn:<turnIndex>`. Hunk-gap rows are
  suppressed (`PatchFile.suppressGaps` — only the hunks were
  persisted; `app_diff_context.go` refuses the scope as backstop), and
  span priming degrades to unprimed highlighting the same way. Sends
  are agent-only (scope ≠ 'pr'). The inline chat affordance
  (`reviewTrigger.ts`) passes `editItemId` so the pane opens pinned to
  that tool call (`pendingEditItemID`, consumed on the next load);
  stale selections resolve to the default — the latest turn.

Diff rendering reuses the chat pipeline: `utils/patchFiles.ts` parsing,
`DiffLineContent.svelte`, `utils/diffSpanCache.svelte.ts` (backend
tree-sitter span requests; the review pane passes `spanContext` so
hunks are parse-primed with real file content per scope),
`lineTintClass`. Inline chat diff affordances route here through
`components/chat/reviewTrigger.ts`.
