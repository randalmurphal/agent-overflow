# Review Pane Design

Unified, virtualized diff/review surface replacing the RHS sidebar system.
It covers local agent diffs (turn / session / workspace / vs-branch) and full
PR/MR review (GitHub and GitLab through their APIs with the token of the
`gh` or `glab` login) without leaving the app.

Status: designed 2026-07-05; all phases shipped (historical spec:
details below reflect the design as written, not the current code).
Superseded in part 2026-07-19: the checkpoint-backed **Turn**/**Session**
scopes were removed with the git-checkpoint machinery; the shipped
scopes are Workspace / Branch / PR, with a per-commit selector on the
branch and PR scopes (`app_review_diffs.go`, `internal/gitdiff/`).
Superseded in part 2026-08-08: PR polling is keyed by PR, not by
subscription. One refcounted pump per PR key (`PRReference.Key`:
`forge:namespace/repo:number` on the forge's public host,
`forge@host:namespace/repo:number` elsewhere) on both sides of the wire,
with `pr:updated` addressed by that key. See
`frontend/src/lib/components/review/AGENTS.md` → "PR state is keyed by
the PR, not by the pane".
Superseded in part 2026-09-27: standalone PR review (`pr://` threads
started from a PR URL) and the forge CLI patch (`gh pr diff` /
`glab mr diff`) were removed. The PR scope needs a local clone whose
branch has the PR; it fetches the PR head and diffs it against the
merge base (`OpenPRDiff`). Diffs of every scope are read in chunks with no
size limit, and the frontend holds patch text within a memory budget
([review diff streaming](review-diff-streaming.md)), replacing the
constraint below that full patch text is parsed and held.

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
`internal/git` forge layer (the forge API transport for GitHub and
GitLab), extended with review-thread read/write APIs and per-PR-key
polling.

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
  `appStorage` (server-side `ui_state` in 036580a2; this frontend's own
  localStorage since 26fd27dca, on the pinned transport port). `RhsSidebarShell`,
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
  model via `parsePatchFiles`.
- **Forge review APIs.** `internal/git/forge.go` gains `PRDetail`
  (metadata, body, verdicts, check summary) and review threads, read by
  part through `ReadPR`, plus `SubmitReview` (verdict + body + line
  comments in one call), `ReplyToThread` and `SetThreadResolved`. GitHub:
  one GraphQL `PRTick` request per tick reads the detail, the
  `reviewThreads` with their line anchors and the head commit's check
  rollup ([reads per tick](forge-transport.md#reads-per-tick)); one
  `SetThreadResolved` mutation carries `resolveReviewThread` and
  `unresolveReviewThread` behind `@include` / `@skip`; review submission,
  file comments and replies are REST POSTs. GitLab: one merge request
  read per tick, with its approvals, discussions with position objects and
  head pipeline jobs as the parts ask; a `PUT` on the discussion itself
  with a JSON `resolved` body.
  Provider specifics stay in `github.go` / `gitlab.go`; the normalized
  types are ours (the same pattern `forge.go` already uses, not the
  Claude/Codex unified-abstraction anti-pattern).
- **Polling is Go-owned and keyed by the PR.** `SubscribePRUpdates` when a
  pane enters PR scope; the pump is refcounted per PR key
  (`PRReference.Key`), so N panes on one PR share one poll. Go
  polls ~45s, diffs snapshots, `a.emit`s only on change (addressed by that
  same key); the last unsubscribe stops the pump. A failing fetch, the
  first one included, never fails the subscribe: the pump carries the
  failure (a caller-safe summary with a log id, and its kind) and retries
  on a doubling delay from 5s up to the interval, and the pane loads its
  diff when the recovery frame brings the first snapshot. A rate limit
  instead holds every poll until its resume time, and one transient
  failure over a snapshot on screen is not shown unless the retry fails
  too; each kind has its own surface ([failure
  presentation](forge-transport.md#failure-presentation)). The pane reads its PR from
  the workspace's git status; the fast first status marks its PR lookup
  pending (`openPrLookupPending`), and the pane waits on that too rather
  than reading empty PR fields as "no PR". No background polling in v1.
- **CI rides the same pump** (`internal/app/app_forge_ci.go`). The pump
  reads the head pipeline on start and re-reads it only while something
  can change: every 10s while a job is queued or running, every 5s while
  a followed job is live on a forge that streams its log (GitLab) or a
  followed job's final log is still being fetched, never while every job
  is terminal (the 45s snapshot re-arms it
  when the head SHA or the check summary moves). Pipeline frames go out on
  `pr:ci_updated` only on change, under the PR's sequence, and the
  subscribe result carries the current pipeline for a joiner. GitHub reads
  every job from the rollup (a `PRTick` that asks for the checks alone) and steps, which only
  the open log view shows, from one REST jobs list per run holding a
  followed job whose steps can still change; GitLab reads the MR view and
  the jobs list. `SetPRCILogFollows` names the jobs a subscription watches:
  the pump fetches each now and streams UTF-16 prefix deltas on
  `pr:ci_log` while the job runs, revalidating by ETag. GitLab serves a
  running job's trace; GitHub serves a log once its blob exists, in
  practice once the job completed, though the jobs API can still call the
  job running then
  ([measurements](../references/forge-api-measurements.md#logs)). A log
  the forge answers 404 for is a wait, not an error: asked at the job's
  cadence while it runs (every 10s on GitHub, which answers 404 for a
  running job's whole run), and once it completed every 5s for six tries,
  then every 45s while the job stays followed. The log keeps what the log view groups lines by: a
  GitHub log's `##[group]` lines, beside the steps' `startedAt` and
  `completedAt` from the jobs list (each log line starts with its own
  RFC 3339 time), and a GitLab trace's section markers, each on a line
  of its own (`cleanGitLabTrace`). The pause, dedup, caller-safe error and
  connection-cleanup rules of the
  snapshot pump apply to all of it; `RefreshPRCI` and a re-sent follow
  are the manual refreshes and run while paused. The frontend sends the
  union of the jobs its panes show per PR (`prReviewCIFollows.svelte.ts`).
  The open log view opens at the tail and follows growth through the
  shared stick-to-bottom controller, wired over `LongListVirtualizer` as
  chat wires it; a reader who scrolls away keeps their place.
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
  pass; batching applies to fresh line comments only. A thread's actions
  sit at its foot as on the forges: a reply field that opens the composer,
  **Copy** (the location and every comment as text, for pasting into an
  agent composer) and **Resolve** / **Unresolve**.
- **Transport classification.** Every new App method that reaches a forge
  or runs `glab`/`git` annotates `//ao:scope git:operate`, so only a
  session granted that scope reaches it.

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
- **`gh`/`glab` missing or unauthenticated** (no login to read the
  forge token from): the failure's kind is `setup` and the pane shows its
  message, which names the login to fix, in its own banner, never a
  silent empty view and never "Retrying".
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
- Per spike policy (`docs/references/spike-policy.md`), the GraphQL
  `reviewThreads` shape and the review-submit REST call are verified in an
  isolated spike before porting, not guessed. Same for the GitLab
  discussions API.

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
- **Forge:** parsing tests against recorded GitHub GraphQL answers
  (`internal/git/testdata/github-pr-tick-*.json`) and GitLab REST JSON
  fixtures; error-path tests for a missing or signed-out login.
- **Prompt builder:** table tests for the lean-vs-rich context rule in
  `internal/diffreview`.
- **Frontend:** vitest for tree↔scroll mapping, scope switching, draft
  lifecycle (persist, orphan flagging, clear-on-success only).
- **Integration:** pane split persistence across restart via `appStorage`;
  end-to-end local-scope review → agent message content.
