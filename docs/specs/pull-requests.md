# Pull requests

Status: design signed off 2026-10-20 (mock reviewed in the browser pane).
Phase 1 (the Merge section, approve, merge, forge events, error kinds) is
in progress; phases 2 to 4 are designed here and not started.

## Goal

Do the rest of what the user opens GitHub or GitLab for, in the app, for
every project on this computer, the same way on both forges: find the
merge requests that need attention, open any of them without a thread,
start an agent on one, see what blocks a merge and clear it, approve and
merge, watch a pipeline, and read a plain sentence when a forge call
fails. The review pane already reads, comments, replies and resolves; this
spec adds the list, the merge actions and the event history around it.

## Approach

Everything reads and writes through the existing forge transport with the
user's `gh` or `glab` login (`docs/architecture/forge-transport.md`); no
new CLI calls, no stored token. One normalized vocabulary per concept so
the UI never branches on the forge: merge readiness is a state plus a
list of blockers, merge options are what the project allows, forge
activity is a list of events. The review pane gains a Merge section that
owns every merge action; the page lists MRs across projects and opens
the same review pane beside the list; tools (Workflows, Pull requests,
later Issues) are pane kinds behind one sidebar row.

## Phases

1. **Merge section** (review pane, every scope that shows a PR): readiness
   pill and section, Approve / Unapprove, `Merge ▾` with auto-merge, forge
   events in the Conversation feed, the error vocabulary.
2. **The Pull requests page**: tool panes and the sidebar tools row
   (Workflows leaves its modal), the MR list with attention states and
   remembered filters, "Open with agent".
3. **Pipelines tab** on the page: project and ref filters, the existing
   stages / jobs / log view keyed by pipeline.
4. **Issues** (GitHub issues, Jira): another tool pane in the same shell;
   not designed here.

## Success criteria

- [ ] An open PR's title bar shows one readiness pill (`Ready to merge`,
      `Blocked · N`, `Checking`, `Draft`) that jumps to the Merge section,
      and never an action.
- [ ] The Merge section lists every blocker the forge reports with its fix
      on the row, holds Approve / Unapprove and `Merge ▾`, and is the only
      place those actions exist, identical in a thread's companion pane and
      on the page.
- [ ] `Merge ▾` is never disabled: it offers the project's allowed methods
      and toggles, "Merge now" when ready and "Auto-merge when ready" when
      blocked, with the greyed one naming what it waits for. No confirm
      step.
- [ ] Approvals, revoked approvals, pushes, merges, closes, reopens, draft
      and ready changes and auto-merge changes appear as rows in the
      Conversation feed on both forges.
- [ ] Every forge failure the app can hit reads as one sentence naming the
      cause and the fix, with the raw text and correlation id behind
      "Details"; a failed project on the page fails alone.
- [ ] The page lists open MRs of every forge-identified project with one
      state per row and a "since you looked" fragment, remembers its
      filters, and opens an MR in the review pane beside the list with no
      thread.
- [ ] "Open with agent" starts a draft thread in a linked worktree on the
      MR's branch, with the MR linked, or hands the MR to a thread already
      on it.
- [ ] Tools are pane kinds: a sidebar tools row with the active tool
      labelled and the rest as icons with badges; ctrl+click opens a tool
      in a new pane.

## Key decisions

### One home for merge actions

The Merge section is the overview row's first collapsible, above
Description. Its header carries the readiness pill, Approve or Unapprove,
and `Merge ▾`; its body is one row per blocker. The title bar's refs line
carries the same pill and nothing else; clicking it runs
`jumpToOverview('merge')`. The header's actions work while the section is
collapsed (the Conversation header's refresh is the precedent). No second
Merge button anywhere, in any state.

### Readiness is a state and a list

`PRDetail.Readiness` is `{State, Blockers}`. `State` is `ready`, `blocked`,
`checking`, `draft`, `merged` or `closed`. `Blockers` is the forge's list
in its order, each `{Kind, Text, Detail, URL}` with `Kind` from one
vocabulary: `approvals`, `checks`, `threads`, `conflicts`, `rebase`,
`draft`, `rules`, `changes_requested`, `other`. The UI renders the kind's
fix beside the text: `approvals` offers Approve when the viewer can,
`checks` opens the pipeline, `threads` jumps to the first unresolved,
`rebase` runs the forge's rebase (phase 1 offers the button; GitLab
`PUT .../rebase`, GitHub `updatePullRequestBranch`), `draft` marks ready,
`conflicts` opens the conflict view, `rules` and `other` show the text.
A ready PR lists its passed checks as settled rows (approved, pipeline
passed, no conflicts) so the section is never empty.

Sources. GitLab: `detailed_merge_status` (one blocker per value;
`mergeable` is ready, `checking`, `unchecked`, `preparing` and
`approvals_syncing` are checking), `has_conflicts`, the approvals read's
`approvals_required`, `approvals_left`, `user_has_approved`,
`user_can_approve`, and the MR's `user.can_merge`,
`merge_when_pipeline_succeeds`, `blocking_discussions_resolved`. GitHub:
`mergeStateStatus` (`CLEAN` and `HAS_HOOKS` are ready, `UNKNOWN` is
checking, `DRAFT` is draft, `DIRTY` conflicts, `BEHIND` rebase, `BLOCKED`
and `UNSTABLE` expand into `reviewDecision`, the failed or pending
required checks of the rollup, and unresolved threads when the repository
requires conversation resolution; a remainder is `rules`), `mergeable`,
`autoMergeRequest`, `viewerCanEnableAutoMerge`, `viewerCanDisableAutoMerge`,
`viewerCanMergeAsAdmin`, and the repository's `mergeCommitAllowed`,
`squashMergeAllowed`, `rebaseMergeAllowed`, `deleteBranchOnMerge`,
`autoMergeAllowed`, `viewerPermission`.

### Merge options come from the project

`PRDetail.Merge` is `{Methods, DeleteBranchOffered, DeleteBranchDefault,
AutoMergeAvailable, AutoMerge, ViewerCanMerge, ViewerCanApprove,
ViewerApproved, CannotApproveReason, CannotMergeReason}`. `Methods` lists
what the project allows (`merge`, `squash`, `rebase`; GitLab's `ff` is
`rebase`), the project default first. `AutoMerge` is the armed request
when one exists (`{Method, ByLogin, ByName, At, WaitsFor, CanCancel}`).
The menu shows exactly these. GitLab project settings are read once per
repository and revalidated through the ETag store, never per tick;
GitHub's repository settings ride `PRTick` with the detail at no extra
request, its default being `viewerDefaultMergeMethod` (the viewer's last
used method or the repository's default) and its methods narrowed by the
base's rulesets (`allowedMergeMethods`).

### Actions

- Approve: GitLab `POST .../approve` with the head `sha`; GitHub a review
  with the approve verdict (the existing `SubmitReview`). Unapprove:
  GitLab `POST .../unapprove`; GitHub `dismissPullRequestReview` on the
  viewer's own standing approval, which the forge adapter resolves from
  `latestOpinionatedReviews` (`viewerDidAuthor`) for the pull request
  named: the caller names no review, so nothing of another login's or
  another pull request's can be dismissed. A viewer who cannot approve
  (GitHub: the author; GitLab: `user_can_approve` false) sees no button,
  and the approvals blocker row says why.
- Merge now: GitLab `PUT .../merge` with `squash`,
  `should_remove_source_branch` and the head `sha`; GitHub
  `mergePullRequest` with `mergeMethod` and `expectedHeadOid`, then the
  branch delete when asked and the repository does not do it itself. The
  head SHA is always sent: a push the pane has not seen yet is refused by
  the forge (409), not merged blind.
- Auto-merge when ready: GitLab `PUT .../merge` with `auto_merge` (17.11+)
  and `merge_when_pipeline_succeeds` (before) in one request; the server's
  answer decides which checks it waits for, and the armed row shows the
  forge's own wording;
  GitHub `enablePullRequestAutoMerge`. Cancel: GitLab
  `POST .../cancel_merge_when_pipeline_succeeds` (`cancel_auto_merge` on
  19.5+), GitHub `disablePullRequestAutoMerge`. An armed auto-merge shows
  as a settled row "Auto-merge set by X · method" with Cancel, and the
  button reads "Auto-merge set".
- No optimistic state: a successful action asks the PR's pump to poll at
  once and the pane follows the forge's answer. The action answers with
  the pump's sequence after that poll (`PRActionResult.Seq`); the button
  shows its pending state until the pane's applied sequence reaches it
  (the frame that carries the forge's answer) or the call fails. A poll
  that found the snapshot unchanged stamped nothing, so the pane settles
  at once: the label it shows is the forge's answer. A thread refresh,
  which replaces the snapshot without a frame, does not settle it, and no
  second action runs while one is pending.
- Every action is withheld, with its reason as the controls' hover text,
  until this PR's diff is on screen (loading, or a failed load), while
  the diff predates the live head (the stale banner) and while the pump
  is failing; the head-pinned actions send the head the diff was loaded
  at, never the live one.
- Merging from a thread's companion pane is the merge the worktree cleanup
  ruling (2026-10-13) reacts to: the thread moves to Base and the pane
  stays on the merged PR's snapshot until closed.

### Forge events in the feed

`PRRead.Events` carries `ReviewEvent{Kind, ActorLogin, ActorName, At,
Text, Ref}` with `Kind` from `approved`, `unapproved`, `review_dismissed`,
`merged`, `closed`, `reopened`, `marked_draft`, `marked_ready`,
`auto_merge_set`, `auto_merge_canceled`, `force_pushed`, `rebased`,
`review_requested`, `base_changed`. GitLab reads them from the system
notes already in the discussions read (no extra request); GitHub from
`timelineItems` inside `PRTick` under `$wantEvents`. Pushes stay the
commit groups the feed already builds from git. Events are feed items of
kind `event`, in time order with the rest, frozen and revealed like
threads. Label, milestone and assignee changes are not events.

### Error vocabulary

Forge failures keep their kinds (`transient`, `rate_limited`, `setup`,
`forge`) and gain `forbidden`: a 403 that is not a rate limit, or a 401
the credential refresh did not cure, classified by what the forge said.
Every message is one sentence naming the cause and the fix; the raw text
and correlation id stay behind "Details". The sentences:

| Case | Sentence |
|---|---|
| CLI missing | existing `SetupError` text |
| Not logged in | "Your `glab` login for HOST has no token. Run `glab auth login --hostname HOST`." |
| Token expired | "Your GitLab token for HOST expired on DATE. Run `glab auth login --hostname HOST`." (the expiry is learned while the token works: GitLab `GET personal_access_tokens/self` once per personal token, GitHub the `GitHub-Authentication-Token-Expiration` header; see forge transport "Credentials") |
| Token lacks scope | "Your HOST token lacks the `api` scope, so NAME cannot ACTION." (GitLab `insufficient_scope`; GitHub `X-Accepted-OAuth-Scopes` none of which `X-OAuth-Scopes` grants. A fine-grained token's "Resource not accessible by personal access token" reads "GitHub blocks this: REASON" on an action and "No access" on a read) |
| No access | "No access to PROJECT with this login." |
| Project or PR gone | "PROJECT!N was not found on HOST, or this login cannot see it." (REST 404, GitHub GraphQL `NOT_FOUND`) |
| Head moved | "The branch moved after you loaded this MR. Reload and try again." (409) |
| Not mergeable | "FORGE refused the merge: REASON." (GitLab 405/422 body, GitHub 405/422 message) |
| Rules block | "FORGE blocks this merge: REASON." (GitHub 403 with a protection message, GitLab 401 on merge) |
| Cannot approve | "You cannot approve your own merge request." / "You are not an eligible approver." |
| Rate limited, network, 5xx | existing sentences |

A write failure is a toast with the sentence, "Details", and a "Reload"
action where the fix is a reload. A read failure on the page shows in the
failed project's group header; the other projects keep working.

### The page (phase 2)

A pane kind `pulls`. Left: a list column with tabs (Merge requests,
Pipelines), a filter row, and grouped rows; right: the review pane on the
selected MR with the project's root as its workspace and no thread, so
stepping through the list never loses it. Compact: the list is the page,
a tap opens the pane full width, Back returns.

The pane is pinned to the MR (`reviewSubjectForPR`): its scope is the PR
alone, and the MR, its computer and the `git:operate` check are the ones
the page selected. Everything the PR scope does beside a thread works,
except what needs one: no Edits scope, no Send to chat (CI log, sections),
no linked-agent target. The page keeps the selected MR's review state and
the one before it; older ones are released with their subscription. Line
comment drafts have no thread row to live under, so the frontend holds
them per MR until a review posts them; they survive stepping through the
list but not a restart. The PR store's `onPRSnapshotApplied` observer
(key, live head, window visibility) is the page's signal to mark an MR
viewed in `pr_views`.

Rows carry number, title, project, author, age, CI dot, approvals `n/m`,
unresolved count, one state pill and a "since you looked" fragment.
States, in sort order across projects: `Your turn` (review requested and
no review from you, or the author pushed or replied after your last
review), `Pipeline failed` (yours), `Ready to merge` (yours, no blockers),
`Changes requested` (yours), `Waiting on author` (you requested changes,
nothing since), `Draft`, `Open`. "Since you looked" compares the head SHA
and the comment count against the last time the app opened the MR
(`pr_views` store table: PR key, opened at, head SHA seen, comments seen);
a row with nothing new dims. Filters (`Needs you`, `Mine`, `Review
requested`, `All open`, project) persist in appStorage per tab.

One list read per project per refresh: GitLab
`projects/P/merge_requests?state=opened&per_page=100` (carries sha,
`user_notes_count`, reviewers, `detailed_merge_status`, `head_pipeline`),
GitHub a `search(type: ISSUE, query: "is:pr is:open repo:O/R")` with
`reviewDecision`, `reviewRequests`, `headRefOid` and the rollup state.
Approvals and unresolved counts are read per MR only for rows in the
"needs you" set or on screen, ETag-revalidated. The page refreshes on
open, on its own refresh button and every PR-update interval while
visible; hidden, it reads nothing.

"Open with agent" (toolbar button on the page's review pane, and each
row's menu): "New thread on BRANCH" creates a linked worktree on the MR's
source branch (`Core.CreateWorktree` after fetching it), opens a draft in
it seeded "Reviewing !N: TITLE URL", and records the PR for the thread
(`thread_pull_requests`) so the badge and the merge cleanup see it;
"Threads on this MR" lists threads already tracking it; "Send review
comments to a thread…" is the existing send flow.

### Tools row (phase 2)

The sidebar's Workflows row becomes a tools row above Settings: the
active tool shows its icon and label with its badge, the others show
their icon and badge with hover text (`git-pull-request-arrow`,
`workflow`, `circle-dot`). The badge is the tool's "needs you" count:
MRs in `Your turn` + `Pipeline failed` + `Ready to merge`, running
workflows, assigned issues. The label follows the focused pane when it is
a tool, else the tool used last. A click opens the tool in the focused
pane like a thread; ctrl+click opens a new pane. Workflows becomes the
`workflows` pane kind with the same shell (tabs, remembered filters, list,
detail beside it) and loses its modal.

### Pipelines tab (phase 3)

Rows are pipelines of the selected projects on any ref: id, ref (branch,
MR, tag), head commit, stage dots, age. Filters: project, ref kind, status,
default all. A row opens the stages / jobs / log view beside the list;
the CI model is keyed by `(repository, pipeline id)`, the MR's head
pipeline being one case. GitLab `projects/P/pipelines`, GitHub
`actions/runs`. The review pane's CI line stays the head pipeline only.

## State matrix (phase 1)

Each row is a component test; the ones marked e2e also run against the
fake forge on desktop and compact.

| State | Pill | Section header | Body |
|---|---|---|---|
| Open, blocked (e2e) | `Blocked · N` amber | Approve or Unapprove · `Auto-merge when ready ▾` | one row per blocker with its fix, then settled rows |
| Open, ready (e2e) | `Ready to merge` green | Unapprove · `Merge ▾` (Merge now primary) | settled rows only |
| Checking | `Checking…` grey | actions present, Merge now greyed | "The forge is checking whether this can merge." row |
| Draft | `Draft` grey | Approve · `Auto-merge when ready ▾` | "Draft" row with Mark ready |
| Auto-merge armed (e2e) | as blocked or ready | `Auto-merge set` button, menu offers Cancel | "Auto-merge set by X · method" row with Cancel |
| Merged (e2e) | `Merged` accent | no actions | "Merged by X · time" row |
| Closed | `Closed` red | no actions | "Closed without merging · time" row |
| Viewer cannot merge | as above | no Merge button | a row with `CannotMergeReason` |
| Viewer is the author | as above | no Approve | the approvals blocker row carries no Approve; without one, a row with `CannotApproveReason` |
| Action in flight | unchanged | the pressed button spins; every merge control disables until the pump's answer is on screen: the action answers with the pump's seq after its poll and the pane waits for its watermark to reach it (a poll that found nothing changed settles at once) | unchanged |
| Action failed (e2e: 403, 409) | unchanged | buttons re-enable | toast with sentence, Details, Reload when 409 |
| Stale snapshot under a banner (the diff predates the live head, or the pump is failing), or the PR's diff not loaded (loading, or its load failed) | last known | every action disabled, its hover text the reason; head-pinned actions always send the head the DIFF was loaded at | last known rows |
| Commit picked (no overview row) | pill still shown; click opens the section for the next overview mount | n/a | n/a |
| Empty diff, or every file hidden by the tree filter | as above | the overview still leads the (empty) list, Merge section included; the rail stays while there are files to unhide | as above |
| PR detail failed to load | no pill | no section | n/a |
| Compact (e2e, screenshot) | pill wraps with the refs line | header wraps: pill, then actions on their own row | rows wrap, fix buttons stay right |

## Edge cases

- GitLab Free has no approval rules: `approvals_required` is 0 and
  `approved` is true with any approval; the approvals row then reads
  "N approvals" as a settled row, never a blocker.
- GitLab before 17.11 has no `auto_merge` and ignores it as an undeclared
  parameter (it never answers 400 for it), so auto-merge sends
  `auto_merge` and `merge_when_pipeline_succeeds` together; 17.11+ reads
  either. Cancel tries `cancel_auto_merge` and, on a 404 (before 19.5),
  `cancel_merge_when_pipeline_succeeds`, remembered per host.
- GitHub `latestReviews` lists each author's latest review, so a comment
  after an approval hides it; `latestOpinionatedReviews` lists each
  author's latest approval or change request, and the viewer's approval
  to dismiss is the one with `viewerDidAuthor`. A dismissed review stays
  in the timeline as `review_dismissed`.
- A merge the forge accepts but the pump reads as still open for one tick
  (GitLab merges asynchronously) shows the pending button until the frame
  with `merged` lands; the toast says "Merging…" only if the forge
  answered 200 with `merged: false`.
- An MR whose project the token can read but not write: readiness shows,
  `ViewerCanMerge` is false, actions grey with the reason; no toast until
  something is pressed.
- A fork PR (head in another repository) shows readiness and merge like
  any other; the branch delete toggle is absent since the branch is not
  the project's.

## Non-goals

- Merge queues and merge trains: the forge's own page.
- Editing title, description, reviewers, labels, milestones.
- Creating MRs from the page (the chat header's create flow stays).
- A full activity timeline: only the event kinds listed.
- Notifications for attention-state changes.

## Testing

Go: fixture-driven parser tests per forge for readiness, options and
events; `forgefake` routes for approve, unapprove, merge, auto-merge,
cancel, dismiss, project settings, timeline items and system notes, each
transitioning the fixture, plus a refusal knob for 403 and 409. Frontend:
one component test per state-matrix row, feed tests for event items, the
toast sentences. e2e: `review-merge.spec.ts` and its compact twin drive
approve → unapprove → auto-merge → cancel → merge → merged on a GitLab MR
and a GitHub PR, plus the 403 and 409 refusals, with a compact screenshot
of the section header.
