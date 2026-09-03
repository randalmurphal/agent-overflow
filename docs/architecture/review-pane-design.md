# Review Pane Design

Unified, virtualized diff/review surface replacing the RHS sidebar system.
It covers local agent diffs (turn / session / workspace / vs-branch) and full
PR/MR review (GitHub via `gh`, GitLab via `glab`) without leaving the app.

Status: designed 2026-07-05; all phases shipped (historical spec:
details below reflect the design as written, not the current code).
Superseded in part 2026-07-19: the checkpoint-backed **Turn**/**Session**
scopes were removed with the git-checkpoint machinery; the shipped
scopes are Workspace / Branch / PR, with a per-commit selector on the
branch and PR scopes (`app_review_diffs.go`, `internal/gitdiff/`).
Superseded in part 2026-08-08: PR polling is keyed by PR, not by
subscription. One refcounted pump per `forge:namespace/repo:number` on
both sides of the wire, with `pr:updated` addressed by that key. See
`frontend/src/lib/components/review/AGENTS.md` → "PR state is keyed by
the PR, not by the pane".

## Goal

Review any diff (an agent's turn, the whole workspace, a branch against its
base, or a live PR/MR) in one GitHub-style surface (file tree + continuous
virtualized diff + inline comments), with comments batchable to either the
linked agent or the PR itself.

## Approach

Panes get kinds and resizable splits; the review surface is a pane kind,
usually linked to a thread. The diff document is virtualized by the existing
`utils/virtual/` engine, extended with three first-class features
(mid-splice compensation, exact-height rows, group/range queries) and driven
by a new `ReviewVirtualizer` adapter. PR data flows through the existing
`internal/git` forge layer (`gh`/`glab` CLIs), extended with review-thread
read/write APIs and per-PR-key polling.

## Success Criteria

- [ ] Panes are user-resizable with persisted splits; `plan` opens as a pane;
      the RHS sidebar system is deleted.
- [ ] A 5k-line, 100-file diff scrolls smoothly (no long frames from mount
      storms) with tree navigation, sticky file headers, and stacked/split
      modes.
- [ ] All four local scopes work: Turn, Session, Workspace, vs Branch
      (base picker, default = repo default branch).
- [ ] From a thread with a detected PR, the PR scope shows description,
      verdicts, checks, and inline review threads; polling refreshes while
      the pane is open.
- [ ] A batch of line comments submits either to the linked agent (one chat
      message) or to the PR as a real review with verdict; replies to
      existing threads send immediately.
- [ ] Draft comments survive app restart; a failed submit keeps drafts and
      surfaces the error in the pane.

## Key Decisions

- **Panes get kinds; RHS dies.** `PaneHost` learns pane kinds (`thread`,
  `review`, `plan`) with draggable splits persisted via
  per-client `ui_state` (not localStorage; see 036580a2). `RhsSidebarShell`,
  `rhsPanelSlot`, and both RHS diff surfaces are deleted when the review
  pane absorbs them. Rationale: one layout system; the RHS width constraint
  was the root complaint.
- **Review pane is thread-linked, optionally standalone.** Opened from a
  thread it keeps that link (comment target = that agent). It can also open
  standalone on a bare PR (`pr://` anchors from `internal/git/forge.go`
  already model clone-less PRs).
- **One engine, two adapters.** Extend `utils/virtual/` rather than build a
  second virtualizer or bolt on overlays. The scroll-ownership contract
  (`frontend-scroll.md`) is the expensive asset; a second engine would be a
  second implementation of it that drifts. New engine capabilities, each
  pure-reducer and unit-testable:
  1. *Mid-list splice compensation* (today: head-splice only): collapse/
     expand of files and unchanged-context regions without viewport jumps.
  2. *Exact-height rows*: a row may declare a known height (lines ×
     line-height when word wrap is off) and skip measurement; measurement
     remains the path for wrapped lines and comment threads.
  3. *Group/range queries*: "which group spans offset X", "offset of key K"
     as engine APIs, powering sticky headers and tree scroll-tracking
     without DOM probing.
- **Row model.** The adapter feeds the engine a flat row list: file header /
  hunk block / inline comment thread / draft editor / context-expander /
  file footer. Sticky file headers come from per-file group containers
  inside the mount window using CSS `position: sticky`. Native browser
  stickiness, no JS scroll-chasing.
- **Diff scopes.** Header scope selector: **Turn** (checkpoint diff),
  **Session** (first checkpoint → worktree), **Workspace** (vs HEAD), all
  existing bindings in `app_checkpoint.go`, plus new **vs Branch**
  (merge-base of current branch+worktree against a chosen base, defaulting
  to the repo default branch; one new Go method beside the existing three)
  and **PR** (lights up when a PR is detected for the branch, or when the
  pane was opened on a `pr://` thread). All scopes share one frontend diff
  model via `parsePatchFiles`; PR patch text comes from the existing
  `Forge.Diff` (`gh pr diff` / `glab mr diff`).
- **Forge review APIs.** `internal/git/forge.go` gains `PRDetail`
  (metadata, body, verdicts, check summary), `ListReviewThreads`,
  `SubmitReview` (verdict + body + line comments in one call),
  `ReplyToThread`, `SetThreadResolved`. GitHub: `gh pr view --json` for
  detail; `gh api graphql` for `reviewThreads` (porcelain can't return
  line anchors) and for the `resolveReviewThread` /
  `unresolveReviewThread` mutations; `gh api` REST for review submission
  (porcelain can't attach line comments). GitLab: `glab api` MR
  discussions with position objects, and a `PUT` on the discussion itself
  carrying `?resolved=`.
  Provider specifics stay in `github.go` / `gitlab.go`; the normalized
  types are ours (the same pattern `forge.go` already uses, not the
  Claude/Codex unified-abstraction anti-pattern).
- **Polling is Go-owned and keyed by the PR.** `SubscribePRUpdates` when a
  pane enters PR scope; the pump is refcounted per
  `forge:namespace/repo:number`, so N panes on one PR share one poll. Go
  polls ~45s, diffs snapshots, `a.emit`s only on change (addressed by that
  same key); the last unsubscribe stops the pump. No background polling in
  v1.
- **Persistence stays lean.** PR snapshots live in memory per PR key.
  Only comment drafts touch SQLite: the existing `diff_review_comments`
  table extended with target + PR anchors (`commit_sha`, `side`,
  `thread_id` for replies), so a half-written review survives restart.
- **Comment flow.** Gutter "+" opens an inline draft editor (a virtualized
  row). Drafts batch across files; one submit action with target picker:
  - *Linked agent*: extends the existing `SendDiffReviewComments` saga.
  - *The PR*: `SubmitReview` with Comment / Approve / Request changes.
  Drafts clear only on confirmed success; failures surface in the pane and
  keep drafts (errors are user-facing state).
- **Agent context rule (deterministic, no toggle).** If the target thread's
  workspace is the diff source (commenting on that agent's own live work),
  keep today's lean prompt. The agent sees the diff in-turn. Otherwise
  (fresh thread, or any PR scope), the prompt includes per-comment anchored
  hunk excerpts plus the diff source reference (PR number/URL or checkpoint
  range) so the agent can fetch the rest itself via `gh`/`git`.
- **Replies send immediately.** They are conversational, not a review
  pass; batching applies to fresh line comments only. Each incoming thread
  gets a **"send to agent"** action handing the thread (file, line, bodies)
  to the linked agent.
- **Transport classification.** Every new App method shelling to
  `gh`/`glab`/`git` annotates `//ao:scope git:operate`, so only a session
  granted that scope reaches it.

## Edge Cases

- **PR head moves mid-review:** poll detects a new head SHA → non-blocking
  "PR updated, reload" banner. Drafts stay anchored to the old SHA (valid
  for the API); after reload, drafts whose lines vanished are flagged
  orphaned, never silently dropped.
- **Resolved / outdated threads:** rendered collapsed at their anchors
  (outdated grouped per file), toggleable.
- **Huge diffs:** files over a line threshold and lockfile/generated files
  render collapsed by default; the virtualizer handles the rest.
- **Context expansion (`···`):** available only when the commit exists
  locally (thread workspace or clone), served by `git show`. Forge-API file
  fetching for pure-remote expansion is an explicit follow-up, not v1.
- **`gh`/`glab` missing or unauthenticated:** the pane shows an explicit
  setup state, never a silent empty view.
- **Merge conflicts (added 2026-07-05):** detection is part of PR scope.
  `PRDetail` carries normalized mergeability (GitHub
  `mergeable`/`mergeStateStatus`, GitLab
  `has_conflicts`/`detailed_merge_status`) and the poll re-checks it; both
  providers compute it lazily, so `UNKNOWN`/`checking` is a transient
  "re-poll" state, never "no conflict" (spike FINDINGS §19). The PR header
  shows a conflicts badge. VIEWING conflict content is a follow-on (phase
  5): neither provider exposes conflict content over API (GitLab's
  `/conflicts` endpoint 404s; GitHub has only the boolean), so the viewer
  is local-only: `git merge-tree --write-tree <base> <head>` (git ≥ 2.38,
  zero worktree mutation; exit 1 + stage entries = conflicted paths) and
  `git show <tree>:<path>` renders the marker version through the review
  pane's line-block surface (read-only, no comment anchors). Clone-less
  `pr://` threads get detection only.

## Non-Goals

- **Merge from the app.** Deliberately out of v1.
- **Background polling / watched-PR badges.** Polling only while a pane is
  subscribed.
- **Pure-remote context expansion** (fetching file contents via forge API).
- **CI log viewing.** Check status is a read-only pass/fail summary.
- **Reusing `TimelineVirtualizer.svelte` for diffs.** The chat adapter
  stays chat-only; the engine core is the shared layer.

## Constraints

- Engine changes must preserve the `frontend-scroll.md` contract: the
  engine never writes `scrollTop`; compensation is reported, the scroll
  controller owns every write.
- Frontend memory stays bounded by the visible window (Core Principle 4):
  highlighting is file-level backend span metadata cached in
  `utils/diffSpanCache.svelte.ts` (originally the Shiki worker pool);
  full patch text is parsed but only windowed rows render.
- SQLite remains a cache: no PR data persistence beyond comment drafts.
- Per spike policy (`docs/references/spike-policy.md`), the
  `gh api graphql` reviewThreads shape and the review-submit REST call are
  verified in an isolated spike before porting, not guessed. Same for the
  `glab` discussions API.

## Phasing (each independently shippable)

1. **Pane foundation**: pane kinds in `PaneHost`, draggable persisted
   splits and migrate `plan` to panes.
2. **Engine features**: mid-splice compensation, exact-height rows,
   group/range queries; reducer unit tests.
3. **Review pane, local scopes**: tree + continuous scroll + stacked/split
   + comments→agent for Turn / Session / Workspace / vs Branch. Absorbs
   both RHS diff surfaces; **the RHS system is deleted in this phase.**
4. **PR scope**: forge review APIs, per-PR-key polling, PR header /
   verdicts / checks / conflicts badge, inline threads, submit-review,
   immediate replies, send-thread-to-agent.
5. **Conflict viewer**: local `git merge-tree` conflict listing + marker
   rendering in the review pane (see Merge conflicts edge case). Requires a
   local clone; detection alone ships in phase 4.

## Migration/Removal

| Old Code | New Code | Action |
|----------|----------|--------|
| `RhsSidebarShell.svelte`, `RhsSidebarResizer.svelte`, `stores/rhsPanelSlot.svelte.ts` | pane kinds in `PaneHost` | DELETE (phase 3) |
| `DiffPanelDrawer.svelte`, `diff-panel/*`, `stores/diffPanel.svelte.ts` | review pane local scopes | DELETE (phase 3) |
| `DiffSidebar.svelte`, `LazyDiffSidebar.svelte`, `DiffSidebarBody.svelte`, `DiffSidebarFile.svelte`, `utils/diffSidebarVirtualizer.svelte.ts` | review pane | DELETE (phase 3) |
| `stores/diffReviewComments.svelte.ts`, `app_diff_review_comments.go`, `internal/diffreview/` | extended with targets + PR anchors | MIGRATE |
| `utils/patchFiles.ts`, syntax spans (`utils/diffSpanCache.svelte.ts`, backend `internal/highlight`; replaced the Shiki worker pool), `diffLineTint`, `payloadExpansion` | reused by review pane | KEEP |
| `internal/git/forge.go` + `github.go` / `gitlab.go` | extended with review APIs | MIGRATE |
| `app_checkpoint.go` diff bindings | reused; + one vs-branch method | KEEP |

## Testing Strategy

- **Engine:** reducer unit tests for the new invariants. Mid-splice keeps
  the viewport anchor stable; exact-height rows are never measured; group
  queries agree with computed offsets. Same style as existing
  `utils/virtual/` tests.
- **Forge:** parsing tests against recorded `gh` / `glab` JSON fixtures
  (from the spike); error-path tests for missing/unauthenticated CLIs.
- **Prompt builder:** table tests for the lean-vs-rich context rule in
  `internal/diffreview`.
- **Frontend:** vitest for tree↔scroll mapping, scope switching, draft
  lifecycle (persist, orphan flagging, clear-on-success only).
- **Integration:** pane split persistence across restart via `ui_state`;
  end-to-end local-scope review → agent message content.
