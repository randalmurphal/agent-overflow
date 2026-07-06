# components/review/

The review pane: the app's ONE full-diff surface (the RHS sidebar/drawer
system it replaced is deleted). Mounts as a `review` companion pane next to
its source thread pane; open it via `openReviewCompanion` /
`pane.toggleReviewPane`, never by mounting it inside chat.

## Layout

| File | Role |
|---|---|
| `ReviewPane.svelte` | Shell: scope/branch/checkpoint selectors, tree/split/wrap toggles, error + send strip, snippet wiring. |
| `ReviewDiffBody.svelte` | The continuous virtualized surface: one `TimelineVirtualizer` over the flat row model, sticky overlay file header, keyboard (j/k files, n/p comments, c file-level comment), jump-to-file. |
| `ReviewFileTree.svelte` | Left-rail GitHub-style tree (`utils/reviewTree.ts`), click-to-jump, top-file highlight, a search box plus an extension-filter dropdown (funnel button right of the search box, multi-select `Menu` of file-type options with counts; filters the RAIL only, never the diff body), resizable width persisted via appStorage `reviewTreeWidth`. |
| `ReviewFileHeaderRow/ReviewCollapsedRow/ReviewLineBlockRow.svelte` | Row renderers. Header/collapsed/line rows are EXACT-height (px-pinned from `utils/reviewRows.ts` constants). |
| `ReviewDraftEditor/ReviewCommentThread.svelte` | Comment rows (measured, not exact). |
| `ReviewPRHeader.svelte` | PR-scope header (title/author/verdicts/checks/mergeability badge + description), normal flow above the diff body, not a virtual row. The state badge and base←head refs live in the `ReviewPane` toolbar stats area (`review-pr-meta`), not here — the toolbar's local-diff +/- is the only additions/deletions readout. |
| `ReviewPRThreadRow.svelte` | Incoming PR review-thread row (measured): comments, reply composer, send-to-agent. Reply text is store-backed. |
| `reviewScroll.ts` | The pane's single scrollTop writer + per-(thread,scope,geometry) position memory. The conflict view passes `scope:conflicts` so its position doesn't clobber the diff's. |

The PR-scope conflict viewer (`git merge-tree`, local clone required)
renders through the same `ReviewDiffBody` with `utils/conflictFile.ts`
pseudo-files: marker lines are `meta`-tinted, content is `context`, and
the surface is deliberately read-only — no comment anchors, drafts, or
PR-thread rows on conflict content. File content loads on expand
(`GetMergeConflictFile`), never eagerly.

Data orchestration lives in `stores/reviewPane.svelte.ts` (per-source-pane
state registry); the row model in `utils/reviewRows.ts`.

## Contracts that bite

- **Exact-height rows**: with word wrap off, line blocks render at exactly
  `REVIEW_LINE_HEIGHT_PX` per visual line, headers at
  `REVIEW_FILE_HEADER_PX`, and collapsed bodies at
  `REVIEW_COLLAPSED_ROW_PX` — the engine skips their ResizeObservers
  (`RowEstimate.isExact`), so ANY vertical padding/border/rem-based height
  drift misplaces every row below. Heights are px-pinned from the shared
  constants; keep them that way. The between-files separation gap is
  painted INSIDE the header row (`REVIEW_FILE_GAP_PX` band +
  `REVIEW_FILE_HEADER_BAR_PX` bar = `REVIEW_FILE_HEADER_PX`); the sticky
  overlay renders the bar alone (`overlay` prop) and appears only once
  the BAR — not the gap — passes the viewport top.
- **Estimate coherence**: `ReviewDiffBody` hands the engine a stable
  wrapper that reads the current `$derived` build; `viewMode`/`wordWrap`
  changes remount the virtualizer via `{#key}` (exactness is
  constructor-once). Don't "optimize" the wrapper away.
- **Scroll ownership**: `reviewScroll.ts` is the only scrollTop writer.
  It deliberately does NOT use `utils/scroll/` (no springs/bottom-pin
  here); see its header comment and `frontend-scroll.md`.
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
  never swaps the diff out from under the user.

Diff rendering reuses the chat pipeline: `utils/patchFiles.ts` parsing,
`DiffLineContent.svelte`, `dispatchInlineFileTokens` (Shiki worker pool),
`lineTintClass`. Inline chat diff affordances route here through
`components/chat/reviewTrigger.ts`.
