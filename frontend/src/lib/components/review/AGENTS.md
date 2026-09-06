# components/review/

The app's ONE full-diff surface. It mounts as a `review` companion pane
next to its source thread pane. Open it via `openReviewCompanion` /
`pane.toggleReviewPane`, never by mounting it inside chat.

Its subject is the CHECKOUT, not the thread: the workspace and branch
scopes take `ctx.workspace` (a `WorkspaceRef`), so a pane showing only a
draft placeholder reviews its checkout with no thread row in sight. The
thread id gates exactly the thread-subject affordances — the edits scope
and its `ListThreadEditDiffs` / `VerifyEditDiffs` /
`GetEditDiffContextLines` calls, review comments, send-to-agent, and
scope persistence. A `ReviewSubject` (`stores/reviewPane.svelte.ts`)
carries both, and `reviewSubjectForPane` is the only thing that builds
one.

Review state captures its backend and checkout for its lifetime. Its identity
and the companion component key both use `companionSubjectKey`, so moving a
conversation or changing its checkout disposes the previous state even when
the conversation ID stays the same. Never key either lifetime on thread ID alone.

## Ownership

- `ReviewDiffBody.svelte` is the one virtualized surface: a single
  `TimelineVirtualizer` over the flat row model, with the sticky overlay
  file header, keyboard nav and jump-to-file.
- `ReviewPane.svelte` is the shell, and owns state the rail and tree only
  render: the Files/Comments tab, and the extension filter's "Apply filter
  to diff" (it derives the `diffFiles` subset and maps top-file highlight
  indexes back to the full list). The text search stays rail-only.
  Everything under its toolbar sits in `shared/RenderBoundary.svelte`: a
  render throw (a row model that cannot build) renders the failure in
  place with a Retry, and is recorded through `reportFrontendDiagnostic`
  because the boundary keeps it from `window.onerror`. Without it the
  flush aborted mid-branch and the pane kept the previous branch's DOM —
  "Loading…" over a fully loaded store, the only trace in
  `frontend-errors.jsonl` (MR !309, 2026-09-04).
- `reviewScroll.ts` is the pane's only scrollTop writer, with
  per-(thread, scope, geometry) position memory. The conflict view passes
  `scope:conflicts` so its position does not clobber the diff's. It
  deliberately does not use `utils/scroll/`, which is spring- and
  bottom-pin-shaped. See its header comment and `frontend-scroll.md`.
- `ReviewFileHeaderRow` and `ReviewLineBlockRow` are EXACT-height. Comment
  rows (`ReviewDraftEditor`, `ReviewCommentThread`, `ReviewPRThreadRow`)
  are measured. A collapsed file is its header row alone, with no
  collapsed body row.
- `ReviewPRHeader.svelte` renders in normal flow above the diff body, not
  as a virtual row. Its CI chips are the ONLY checks surface, and the
  state badge and base/head refs live in the `ReviewPane` toolbar stats
  area rather than here. It hosts two `ReviewCollapsibleSection`s:
  Description (local open state) and Conversation
  (`ReviewConversation` + `ReviewConversationThread` +
  `ReviewConversationCommits`). The Conversation body is ONE
  chronological feed, newest first, interleaving thread cards, verdict
  rows and commit-push rows (GitLab's overview timeline, compact); a
  thread card's first comment always renders IN FULL — never truncated,
  never behind a click — and what folds is only the REPLIES, default
  folded on settled (resolved/outdated) threads behind an "N replies"
  toggle. Its open state lives on the review store because the feed
  order FREEZES while open — captured at open, remote arrivals wait
  behind an "N new" chip, and a remote resolve never moves a card or
  folds replies the reader has open (`conversationFeed` /
  `captureConversationOrder` in `stores/reviewPane.svelte.ts`). Both
  section bodies render through `ReviewResizableBody`: a default
  max-height cap until the user drags the bottom handle, then a
  remembered pixel height shared per section across panes
  (`stores/reviewSectionSizes.svelte.ts`).
- Forge-authored bodies (PR description, review thread comments, verdict
  summaries) render `ChatMarkdown` with `embeddedHtml` — the sanitized
  forge-HTML surface (`markdown/AGENTS.md` § Security boundary) — and
  comments whose `visibleBody` is empty (marker-only bot replies) render
  nothing. `ReviewThreadComments.svelte` is the one comment-list +
  reply-composer body, shared by the inline strip (`ReviewPRThreadRow`)
  and the conversation card so the two cannot drift; icon actions come
  from `ReviewIconButton` (hovertext buttons, never text buttons).
- `ReviewCILogView.svelte` REPLACES the diff body (the conflict-viewer
  pattern) rather than mounting beside it.
- Comment bodies render through `ChatMarkdown` as a SIBLING of the row
  button, because links must not nest inside a button.
- Data orchestration is `stores/reviewPane.svelte.ts` (per-source-pane
  view state) over `stores/prReviewStore.svelte.ts` (the shared per-PR
  entity). The row model is `utils/reviewRows.ts`.

The PR-scope conflict viewer (`git merge-tree`, local clone required)
renders through the same `ReviewDiffBody` with `utils/conflictFile.ts`
pseudo-files: the merged blob's conflict regions become a pseudo-diff
(ours → `del`, theirs → `add`, so split view shows ours|theirs side by
side), marker lines render as visible unnumbered `marker` rows relabeled
with the base/head labels, and non-conflict runs fold to a few context
lines around each conflict. Fold rows expand via
`expandConflictFold(path, foldId)` on the store (ids are stable per
file). Line numbers flow through synthetic `@@` headers. merge-tree's
informational messages are attributed to their file Go-side
(`MergeTreeResult.Notes`; redundant "CONFLICT (content)" /
"Auto-merging" lines are dropped) and render as marker rows at the top
of that file's body, the only signal for modify/delete-style
conflicts. A file with notes but no textual regions gets a structural
badge (`PatchFile.conflictLabel`, e.g. "modify/delete") in place of
the conflict-count badge, and expands even when its content is
unfetchable (the path may not exist in the merged tree). Messages
naming no conflicted path (rare) fall back to a strip above the diff
body. The surface is deliberately read-only: no comment anchors,
drafts, or PR-thread rows on conflict content. Files open EXPANDED
(content loads fan out in parallel via `GetMergeConflictFile` inside the
store's `openPRConflicts`); a file whose load fails with no notes stays
collapsed. The tree and its file contents belong to the PR, not the pane,
and it is RECOMPUTED when the head or base ref moves under it. A tree OID
names one (base, head) pair, and reading files against a superseded one
renders the previous merge's content forever.

The PR-scope CI surface (`GetPRCIJobs` / `GetPRCIJobLog` /
`SavePRCIJobLog`, normalized in `internal/git/ci.go`) loads lazily with
the PR detail and refreshes on the same `pr:updated` pump, with no
separate poll. The log view is read-only and in-memory; "Save to file"
writes the full log under the app-managed `ci-logs/` dir and "Send to
chat" prefills the source pane's composer with a path reference (never
auto-sends). The log wire payload is tail-capped (2 MB) because failures
read tail-first.

## Contracts that bite

- **Exact-height rows**: with word wrap off, line blocks render at exactly
  `REVIEW_LINE_HEIGHT_PX` per visual line and headers at
  `REVIEW_FILE_HEADER_PX`. The engine skips their ResizeObservers
  (`RowEstimate.isExact`), so ANY vertical padding/border/rem-based height
  drift misplaces every row below. Heights are px-pinned from the shared
  constants; keep them that way. The between-files separation gap is
  painted INSIDE the header row (`REVIEW_FILE_GAP_PX` band +
  `REVIEW_FILE_HEADER_BAR_PX` bar = `REVIEW_FILE_HEADER_PX`); the sticky
  overlay renders the bar alone (`overlay` prop) and appears only once
  the BAR (not the gap) passes the viewport top. Files render as inset
  card slabs (`mx-2` + `border-x` on every row of a file, horizontal
  only — never a vertical border on an exact row); each slab's rounded
  bottom cap is painted by the NEXT header's gap band, and the last
  file's by the exact-height `surface-end` row (`REVIEW_SURFACE_END_PX`).
  Hunk-gap bands are ordinary display rows inside line blocks (one line
  tall, no borders). Same exactness rules.
- **Hunk-gap expansion**: `buildPatchDisplayRows` emits `gap` rows
  (leading/between/trailing, new-side coordinates) for the unchanged
  runs hidden between hunks; conflict pseudo-files suppress them (their
  folds already cover this). Expand clicks flow
  `ReviewLineBlockRow.onExpandGap` → `store.expandDiffContext` →
  `GetDiffContextLines(workspace, …)` for the checkout scopes and
  `GetEditDiffContextLines(threadId, …)` for edits (new side only, since
  expanded context is identical on both sides; per-scope source
  resolution lives in `app_review_diffs.go`) → `utils/diffContextExpansion.ts` merges the
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
  file's own highlight request is in flight. Expanding must never
  flash already-colored lines plain.
- **Estimate coherence**: `ReviewDiffBody` hands the engine a stable
  wrapper that reads the current `$derived` build; `viewMode`/`wordWrap`
  changes remount the virtualizer via `{#key}` (exactness is
  constructor-once). Don't "optimize" the wrapper away.
- **Reading anchor, content never moves the reader**: refreshes are
  opt-in (refresh button, PR stale banner; there is NO auto-reload on
  workspace change, only a stale dot on the refresh button fed by the
  source pane's gitwatch slot), and when content DOES change under the
  same scroll key (reload, gap expansion, PR poll replacing thread
  rows) `ReviewDiffBody` restores the top-of-viewport anchor (file,
  line, pixel delta; math in `utils/reviewAnchor.ts`) instead of
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
- **Hide whitespace (`-w`)** is a diff SOURCE option, not a render
  option: flipping it re-requests the patch
  (`setIgnoreWhitespace` → `reload({ selectionOnly: true })`), because
  git emits a different patch and every derivation from it (parsed
  files, px-pinned row geometry, highlight spans) has to be rebuilt.
  Per pane, default off, deliberately not persisted. Available only
  where `internal/gitdiff` produces the patch
  (`supportsIgnoreWhitespace`): workspace, branch, and any selected
  commit including in pr scope. NOT the PR whole-diff (it can come
  from the forge API, which has no `-w`), not edits, not the conflict
  or CI-log views. The toolbar and `loadPatch` read the same
  predicate, so the button can't offer a mode the load path won't
  deliver.
  Comment anchors survive the toggle: `-w` narrows hunks but emits
  true file line numbers, so a `(path, line)` anchor means the same
  physical line in both patches (guarded Go-side by
  `TestIgnoreWhitespaceKeepsCanonicalLineNumbers`). Comment creation
  therefore stays enabled.
- **Comment sourceKey** is a content hash of the patch
  (`utils/diffSourceKey.ts`); drafts persist in SQLite via the
  `diff_review_comments` bindings and batch-send through
  `SendDiffReviewComments`. EXCEPTION: pr scope uses the stable key
  `pr:{forge}:{namespace}/{repo}:{number}` so drafts survive head pushes;
  each draft's `commitSha` records what it was anchored to, and orphan
  detection (drafts whose line left the diff) excludes them from PR
  submission without deleting them.
  Orphan detection runs wherever the sourceKey OUTLIVES the patch it
  was written against (`sourceKeyOutlivesPatch`): pr scope always, and
  a selected commit while `-w` is on. The SHA key is stable but the
  rendered patch is not, so a draft on a whitespace-only line carries
  over with nowhere to land and would otherwise be invisible in the
  diff body yet still counted and still sent. Everywhere else the
  content-hashed key changes with the patch, so no draft can carry
  over: flipping `-w` in workspace/branch scope re-keys drafts, which
  HIDES them (reversibly, never deleted, never re-anchored) rather
  than landing them on a diff they were not written against.
- **PR diff source**: `GetPRDiff(workspace, pr, baseRef)` prefers a
  locally-computed three-dot diff (`git diff --merge-base origin/<base>
  <fetched-head-oid>`) when there is a clone. A ZERO `WorkspaceRef` (both
  fields empty) is the wire spelling of "no local clone". gh/glab's PR-diff
  endpoints refuse diffs over 20k lines (HTTP 406), which large PRs blow
  past. The forge API is the fallback for pr-anchor threads with no local
  checkout. The PR load sequences the entity hold BEFORE the diff (not
  parallel) because the base ref only lands with the PR detail. It awaits
  `attachPR(...).ready()`, which resolves from the first observation and
  REJECTS if the subscribe fails or the last holder leaves, so a load can
  never hang on a PR nobody is watching.
- **PR state is keyed by the PR, not by the pane**
  (`stores/prReviewStore.svelte.ts`). Detail, review threads, the live
  head, the CI pipeline and the merge-conflict tree describe the pull
  request; two panes on one PR observe ONE snapshot, one poll pump and
  one merge-tree run. A pane holds the key (`attachPR`, refcounted) and
  keeps only view concerns: the diff it loaded, the head it loaded that
  diff AT, collapse/expansion, drafts, the CI log view.
  `prStale` is DERIVED from those two heads, so a push observed by one
  pane can never mark another pane's freshly-loaded diff stale, and it
  still never swaps the diff out from under the user (banner only).
  Resolve/unresolve is optimistic THROUGH the entity: `setPRThreadResolved`
  records an override in `prReviewStore` (`overriddenPRThreads`) that
  every pane's `prThreads` projects through, outranking poll snapshots
  fetched before the mutation landed until one AGREES (the backend
  read-back-verifies, so the next poll does). A failed RPC reverts the
  override and surfaces per-thread (`resolveErrorFor`).
  Backend side, `SubscribePRUpdates` is refcounted per PR key too
  (`prUpdateKey` in `app_forge_review.go`, the same string
  `utils/prReference.ts#prKey` builds): one ticker, one change-detection
  state, however many subscribers. `pr:updated` therefore carries a
  `prKey` and no subscription id, and `stores/eventsPRReview.ts` routes
  by it. A fetch failure on the pump rides the same event as `error` and
  surfaces as `prUpdateError` (separate from the pane's `error`, which
  owns the diff). A hidden document (minimized window, background tab)
  votes every live pump down via `SetPRUpdatesActive`; the votes COMPOSE
  backend-side, so one visible client keeps the shared pump running, and
  resume catch-up-polls only when a tick was missed. The store's
  module-level `visibilitychange` listener owns the flip, one call per
  PR, not per pane.

- **One PatchFile per path is the parser's job for a type change.** git
  reports a regular-file ↔ symlink flip as ONE `T` status but emits it
  as TWO adjacent same-path `diff --git` sections (old form deleted,
  new form created), and every path-keyed consumer — the file tree,
  collapse/comment maps, `reviewRows`' header keys, chat's
  `ToolCallCard` file stack — dies on the duplicate key. `parsePatch`
  folds that pair into one `modified` file (one preamble, both hunks,
  `suppressGaps` because nothing is hidden between them), and
  `extractPatchFile` hands back both sections as that file's patch.
  This repo's own `CLAUDE.md` → `AGENTS.md` symlink convention makes the
  shape routine, in every scope. Only the (deleted, added) pair folds;
  the edits-scope multi-section shape below never matches it. Same
  parser, same class: `\ No newline at end of file` is `meta` (git's
  annotation on the line above), matching `internal/highlight/patch.go`;
  read as context it numbered itself and shifted every row after it,
  so a comment on a file's last line anchored one line off.
- **Edits scope** renders persisted tool-call diff payloads (the
  historical change itself, correct after commits/rebases), never a
  git recomputation. `ListThreadEditDiffs` lists metadata only; the
  selected diff loads via `GetPayloadData` (single edit) or
  `GetTurnEditsDiff` (a turn's payloads concatenated in item order,
  a sequential story, NOT a net diff: a file edited twice keeps both
  sections). Same-path sections merge into ONE PatchFile
  (`mergePatchFilesByPath`, because the surface keys rows/tree/collapse
  by path, and duplicate paths crash the keyed each). Each section's
  line numbers describe the file at ITS edit's moment, so disjoint
  sections are renumbered into one coherent final-file-ordered
  section (later-above edits shift earlier hunks by their net delta).
  That coherence is what lets a merged file verify, prime, and
  gap-expand below, and keeps the gutter's number sizing honest.
  A file CREATED in the turn composes instead: later hunks apply to
  the creation content (old side byte-verified per splice) and the
  merge emits one clean added-file section of end-of-turn content.
  Overlapping sections on pre-existing files can't be renumbered and
  fall back to edit-order concatenation with `suppressGaps` set (a
  failed composition falls back the same way). The store consumes the
  merge through `mergePatchFilesByPathCached`, keyed on the stable
  parse-cache array: the `files` derived re-runs per expansion click,
  and a fresh merged lines array per run would break the span cache's
  predecessor-chain fallback, which flashed the whole file plain on
  every expansion. sourceKeys:
  `edit:<payloadId>` / `edit-turn:<turnIndex>`. Historical fidelity is
  verification-gated and snapshot-first: the persist tap captures each
  edit's new-side file content into `edit_file_snapshots` (gzipped,
  per payload+path, written only when the just-edited workspace file
  provably matched the patch), and `app_review_diffs.go` resolves the
  edit selection (`editPayloadId` / `editTurnIndex`, whole-turn = last
  snapshot of the path in item order) against snapshots before falling
  back to the current workspace file for pre-snapshot history. Either
  source serves ONLY when every new-side patch line still matches it,
  byte-exactly or modulo Claude's structuredPatch tab mangling
  (leading tabs ship as two spaces per tab;
  `highlight.PatchContentMatch` tolerates exactly that transform and
  `GetDiffContextLines` tab-expands served lines to match); the
  expansion and priming requests carry the patch as `verifyPatch`.
  Gap arrows are POSITIVELY gated at load time: after an edits load
  the store fires `VerifyEditDiffs` (a batch run of the same
  resolution) and only verified paths get gap rows
  (`editExpandablePaths`). An arrow that can't serve never renders.
  Absolute-path edits (agent memory files, scratchpads) are never even
  sent; drifted pre-snapshot files and remote clients (the RPC is
  ungranted) simply never verify. A click-time refusal (rare
  load-to-click race) still retires the path quietly
  (`unexpandableEditPaths`, no error banner), and unverified files'
  spans fall back to unprimed. Span quality is monotonic:
  persist-time seeds (primed with the just-edited file, attached to
  `GetPayloadData`/`GetTurnEditsDiff` and pushed on
  `highlight:diff_seed` to ALL clients) upgrade unprimed cache entries
  in place and are never downgraded. Sends are agent-only
  (scope ≠ 'pr'). The inline chat affordance
  (`reviewTrigger.ts`) passes `editItemId` so the pane opens pinned to
  that tool call (`pendingEditItemID`, consumed on the next load);
  stale selections resolve to the default, the latest turn.

Diff rendering reuses the chat pipeline: `utils/patchFiles.ts` parsing,
`DiffLineContent.svelte`, `utils/diffSpanCache.svelte.ts` (backend
tree-sitter span requests; the review pane passes `spanContext` so
hunks are parse-primed with real file content per scope — routed by the
same subject split as the gap-expansion RPCs,
`HighlightPatchWithContext(workspace, …)` for the checkout scopes and
`HighlightEditPatchWithContext(threadId, …)` for edits, with a primed
entry keyed on exactly the subject its RPC resolved through; the
`subjectId` a diff body carries — `review.identity`, the row id, or a
draft placeholder's synthetic one — is the OWNER for cache eviction and
scroll memory, never an RPC subject, and materialization hands it to the
real row through `adoptDiffSpanOwner` rather than evicting, since the
subject-keyed entries stay valid across the swap),
`lineTintClass`. Inline chat diff affordances route here through
`components/chat/reviewTrigger.ts`.
