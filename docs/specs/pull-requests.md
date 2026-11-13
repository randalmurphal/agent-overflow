# Pull requests

Status: design signed off 2026-10-20 (mock reviewed in the browser pane).
Phases 1 to 3 are shipped; phase 4 (Issues) is not designed and has no
tool row entry until it is.

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

- [x] An open PR's title bar shows one readiness pill (`Ready to merge`,
      `Blocked · N`, `Checking`, `Draft`) that jumps to the Merge section,
      and never an action.
- [x] The Merge section lists every blocker the forge reports with its fix
      on the row, holds Approve / Unapprove and `Merge ▾`, and is the only
      place those actions exist, identical in a thread's companion pane and
      on the page.
- [x] `Merge ▾` is never disabled: it offers the project's allowed methods
      and toggles, "Merge now" when ready and "Auto-merge when ready" when
      blocked, with the greyed one naming what it waits for. No confirm
      step.
- [x] Approvals, revoked approvals, pushes, merges, closes, reopens, draft
      and ready changes and auto-merge changes appear as rows in the
      Conversation feed on both forges.
- [x] Every forge failure the app can hit reads as one sentence naming the
      cause and the fix, with the raw text and correlation id behind
      "Details"; a failed project on the page fails alone.
- [x] The page lists open MRs of every forge-identified project with one
      state and reason per row from the forge's record of the user's
      activity, remembers its
      filters, and opens an MR in the review pane beside the list with no
      thread.
- [x] "Open with agent" starts a draft thread in a linked worktree on the
      MR's branch, with the MR linked, or hands the MR to a thread already
      on it.
- [x] Tools are pane kinds: a sidebar tools row with the active tool
      labelled and the rest as icons; ctrl+click opens a tool in a new
      pane, and a drag places it in the pane strip like a thread row.

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

### Sending and managing comments

On a backend with capability `pr-comment-actions.v1`, in the PR scope
with All commits picked, on both surfaces and both forges:

- Send now or hold. A new comment's editor offers "Comment now" and "Add
  draft", a reply's "Reply now" and "Add draft". The primary is the "now"
  action while the pane holds no drafts for the PR and "Add draft" once it
  holds one; Ctrl/Cmd+Enter runs the primary and Ctrl/Cmd+Shift+Enter the
  other, and the placeholder names only the primary's chord. A held reply
  is a draft (`side: reply`, the thread and its first comment's id): it
  lists at its thread's foot with its own Edit and Delete, counts in the
  send strip, and goes with the batch, to the PR as a reply on its thread
  or to an agent quoting the thread (`DiffReviewPRContextEntry.thread`).
  Reply on a thread that holds some edits the newest. A held reply whose
  thread is gone lists in the Conversation and is left out of the review.
  Local scopes, the single-commit view and older backends keep a single
  "Add comment" (a draft) and "Reply" (now).
- Edit and delete a sent comment where the forge allows it
  (`viewerCanEdit`, `viewerCanDelete`), from icons at the right end of its
  header, revealed on hover and always shown on compact, never moving the
  header's text. Delete asks in place (`Delete?  Yes  Cancel`). "edited"
  shows when the forge says when the body was edited (GitHub
  `lastEditedAt`; GitLab notes do not say).
- A range by Shift+click on another line's "+" while a comment's editor
  is open: the spanned rows are tinted, the editor sits after the last
  one, its header reads `Lines 12–18` with a control back to one line.
  A range stays in one file, on one half of the diff (an unchanged line
  is on both; a deleted line to an added one is refused), on lines the
  loaded diff has (not an expansion's) and inside one hunk; a refusal
  says which on the editor and leaves the editor on its line. A thread
  the forge reports with a start line shows the same label and tint. One
  written on the forge from a removed line through an unchanged or added
  one reads `-4 to +6` and tints the rows from the removed line through
  the last one.
- One "Comment…" field at the end of the Conversation feed posts a
  comment on the PR itself, now; it shows in the feed even after the feed
  froze. The answer names the created comment (`PRActionResult.commentId`,
  as `ReviewComment.id` reads it), and only that comment joins the frozen
  feed; one the answer does not name stays behind "N new".

A write posted now (`PostPRLineComment`, `ReplyToPRThread`,
`PostPRComment`, `EditPRComment`, `DeletePRComment`) answers
`PRActionResult`; its editor stays pending until the pane applies the
frame at the answer's seq, the backend restarts or the pump fails, and
closes at once on seq 0. A failure keeps the text and shows the forge's
sentence on the editor. Back and the pane's close are not blocked while a
write is pending. A write that fails after its pane's review state was
disposed has no editor left, so it gets one toast that stays until
dismissed, with a `Copy text` action that copies the comment's whole text
and leaves the toast up: "Your comment on <where> was not sent.
<sentence>", or for a write that may have landed "Your comment on
<where> was not confirmed. <sentence>": one the forge did not confirm
(`forge_unconfirmed`, whose sentence says what to check) or one whose
answer was lost to a dropped connection or a timeout (which reads the
same way in an open editor). When the clipboard refuses the copy, the
toast is replaced by one that shows the text itself, selectable. <where> is the file and line or range,
the thread's place, or "the pull request"; an edit reads "Your edit to
the comment on <where>". Nothing is kept: the PR's review opened again
has fresh editors. A write that succeeds after its pane went changes
nothing, and one whose pane is still open fails in its editor, with no
toast. Posting now is withheld, with the reason as the
control's hover text, under the merge actions' gates and when
`git:operate` is not granted. Open editors' text lives in the review
state, so it survives its row unmounting, and find skips an open edit.

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
| Comment answer lost | "FORGE may have posted this comment. Check the thread before sending it again." (kind `unconfirmed`: a comment or reply posted now whose response did not say; the pane's next frame shows what the forge holds) |
| Comment elsewhere | "That comment is not on PROJECT#N, so it was not changed." (kind `not_found`: an edit or delete naming a GitHub comment of another PR) |
| Reply thread gone | "The thread this reply answers is no longer on PROJECT!N, so the reply was not posted." (kind `not_found`) |
| Rate limited, network, 5xx | existing sentences |

A write failure is a toast with the sentence, "Details", and a "Reload"
action where the fix is a reload. A read failure on the page shows in the
failed project's group header; the other projects keep working.

### The page (phase 2)

A pane kind `pulls`. Left: a list column with tabs (Merge requests,
Pipelines), a toolbar, and grouped rows; right: the review pane on the
selected MR with the project's root as its workspace and no thread, so
stepping through the list never loses it. The list column's width is
remembered and clamped to leave the detail a usable width in the pane as
it is now; its handle also resizes from the keyboard. Compact: the list
is the page, a tap opens the pane full width, and the page header's Back
is the one Back: it closes a CI log, the conflict view or the open
file-tree rail in the detail, then the detail.

The pane is pinned to the MR (`reviewSubjectForPR`): its scope is the PR
alone, and the MR, its computer and the `git:operate` check are the ones
the page selected. Everything the PR scope does beside a thread works,
except what needs one: no Edits scope, no Send to chat (CI log, sections),
no linked-agent target. The page keeps the selected MR's review state and
the one before it; older ones are released with their subscription. Line
comment drafts have no thread row to live under, so the MR owns them
(stored under its key on its computer) until a review posts them or they
are sent to a thread; they survive stepping through the list and a
restart. The page records nothing about which MRs the user opened.

A row reads top down: the project, `!number`, the author's display name
("you" for your own) and the age; the title, wrapping to two lines; then
the facts, wrapping and never cut: the state label in its tone, approvals
`n/m`, the unresolved count and, on your own MRs only, the CI dot. The
label says why the row is where it is: `Review requested`, `+N commits
since your review`, `N replies since your review` or `Author replied`,
`Approved by you` or `Reviewed by you`. States, in attention order:
`Your turn` (a review requested from you and not
given; or, after your latest review, comment or approval, the author
pushed, or replied: GitHub the author's latest comment or review, GitLab
the MR's update time more than `gitlabTouchSlack` (10 s) past your
activity), `Pipeline failed` (yours), `Ready to merge` (yours, no
blockers), `Changes requested` (yours), `Waiting on author` (you
requested changes, nothing since), `Approved by you` or `Reviewed by you`
(your approval stands, or you reviewed without approving, nothing since),
`Draft`, `Open`. The first three are Needs you, listed before the rest.
Your activity is the forge's own record: GitHub `viewerLatestReview`,
GitLab your notes (conversation, diff and discussion notes) and approvals,
read as two events feeds GitLab filters to comments and to approvals.
The feeds are a lower bound: each reads at most five pages of 100 events
from the last 30 days. A feed that ends within its pages covers the 30
days; one cut at the fifth page covers back to the oldest event it
returned. The window's floor is the later of the two. Your activity is
the later of the activity found and the floor, since a cut feed can hide
activity newer than what the other feed found: a reviewer state of
reviewed, approved or changes requested with no activity in the window,
or only activity older than the floor, takes the floor as your activity.
A head commit after it is a push since your review, an update after it
(past the slack) a reply, and neither keeps `Approved by you` or
`Reviewed by you`. The toolbar's three
menu buttons, the filter (`Needs you` with its count, the rows that
filter lists for the picked projects whichever filter and sort show, `Mine`,
`Reviewing`, `All open`), the projects and the sort (`Updated` newest
first by default, `Created`, `Title`, `Author`, `Attention`; the same
key again turns the order around) persist in appStorage per computer;
under 380px the buttons keep their icons and drop their labels. A sort
you pick opens the list from the top; rows that move under a list at its
top leave it at its top, and under a list scrolled into its rows keep the
row at the viewport top in view.

Each project is read with a fixed set of concurrent requests and never a
read per row. GitHub: one `PullsSearch` per 100 PRs, whose nodes carry the
review state, approvals given and required, unresolved threads (unknown
for a PR with more than 100) the rollup and the head commit's time.
GitLab: the merge request list (rows), three GraphQL reads of the MRs you
review (`PullsReviewing`, reviewer states, approvals, head pipeline;
`PullsReviewingActivity`, discussions and the head commit's time;
`PullsReviewingApprovals`, the MR's own approval rules, so the required
count follows its target branch and any override) and the same three of
the MRs you authored, and, once per host per refresh, your comment and
approval events. Only a
failed list fails the project; another failed request leaves its fields
unknown and shows on the project beside its rows. A GitLab MR you neither
review nor authored carries no review detail and reads `Draft` or `Open`.

Each project's read is stored (`pr_list_projects`), so the page opens on
the stored list, marked stale while it is older than the PR-update
interval, and each project replaces its rows as its read lands. The
backend reads once after boot every forge project whose stored read is
missing or older than the interval, so the page opens on a current list
rather than a stale one. The page refreshes on open, on its own refresh
button and every PR-update interval while visible; hidden, it reads
nothing.

"Open with agent" (the page header's button on the selected MR, and each
row's menu on that row's MR, which stays unselected): "New
thread on BRANCH" creates a linked worktree on the MR's source branch,
which must exist on origin; a fork's head is fetched into the local
branch `pr/N` instead. The thread opens with a draft seeded "Reviewing
!N: TITLE URL" and records the PR (`thread_pull_requests`) so the badge
and the merge cleanup see it, every step rolled back if a later one
fails;
"Threads on this MR" lists threads already tracking it, and a failed read
of them says so with a Retry and keeps "New thread" off until it
succeeds; "Send review comments to a thread…" is the existing send flow.
A failed refresh shows under the page header with "Details".

### Tools row (phase 2)

The sidebar's Workflows row becomes a tools row above Settings: the
active tool shows its icon and label, the others show their icon with
hover text (`git-pull-request-arrow`, `workflow`, `circle-dot`). Only
Workflows carries a badge, the workflow runs that need you, also in its
hover text; Pull requests shows no count in the sidebar, the page's
`Needs you` count being the one place it is read. The label follows the
focused pane when it is a tool, else the tool used last. A click opens
the tool in the focused pane like a thread; ctrl+click opens a new pane.
Off compact a tool's button drags into the pane strip like a thread row,
with the same drop slots and preview; the drop places the tool as its
own pane at that slot, replacing nothing, or moves it there when it is
already open (one pane per tool; its own slot shows no preview). The
dropped tool is focused, revealed and counts as used last. Workflows
becomes the `workflows` pane kind with the same shell (tabs, remembered
filters, list, detail beside it) and loses its modal.

### Pipelines tab (phase 3)

Rows are pipelines of the selected projects on any ref: id, ref (branch,
MR, tag), head commit, stage dots for the rows on screen (or that their
read failed), age. A project whose pipelines or origin could not be read
shows its failure above the rows, as on the Merge requests tab. Filters:
project, ref kind, status, default all; the ref and status pickers are
one choice each and close on the pick. A row opens the stages / jobs /
log view beside the list; on compact the page header's Back is the one
Back, for a pipeline as for an MR, and closes an open log first;
the CI model is keyed by `(repository, pipeline id)`, the MR's head
pipeline being one case. GitLab `projects/P/pipelines`, GitHub
`actions/runs`. A GitLab pipeline's stages hold its builds, bridges and
external commit statuses (an external pipeline holds only those); a
bridge or status links out, as a GitHub external check does. The review pane's CI line stays the head pipeline only.
The tab's pipeline view is the review pane's CI view, so it offers the
same CI actions (below).
The tab reads its list on open, on its refresh button and every
PR-update interval while visible. Hidden, the interval, a reconnect and
a regained grant read neither the list nor the stages; the one read they
owe runs when the window shows.

### CI actions

The review pane's CI and the Pipelines tab act on a pipeline through one
implementation (`stores/ciActions.svelte.ts`, `ciSubjectActions.svelte.ts`):
retry a job, cancel a job and run a manual job (GitLab), retry the failed
jobs and cancel the pipeline, and download artifacts. The forge requests,
the capability reads and the refusals are in
[forge transport](../architecture/forge-transport.md), "CI actions".

- Which controls show is the CI read's flags, never a status string. A
  flag of `allowed` shows the control; absent, `""` (does not apply) and
  `denied` (this login may not) show nothing, so no control is dead. A
  GitHub check that is not an Actions job has no flags and keeps "Open in
  browser". A GitHub run created more than 30 days ago offers no retry
  where its read carries its creation (the Pipelines tab); a PR's checks
  do not, and GitHub's refusal reads "… is more than 30 days old, so it
  can no longer be retried."
- A read behind the flags that failed (GitHub's repository permission or
  a run's artifacts) never costs the pipeline: its jobs and the other
  flags stay, the flags it would have set are absent, and the pipeline
  carries one sentence (`actionsUnavailable`), shown as a muted,
  non-interactive row in the CI header's menu, whose trigger shows for it
  alone. The sentence names only what the failed read took away: artifact
  downloads for the artifacts read, retry and cancel for the permission
  read, all three when both failed. The pipeline is read again when the
  failed read is due again, finished or not, and the sentence clears with
  the read that succeeds.
- Job actions are icon buttons in the log view's header beside Refresh,
  and on a job's row in a stage's list, revealed on hover (always shown on
  compact). Pipeline actions (Retry failed jobs, Cancel pipeline, and on
  GitHub Download artifacts, where artifacts are per run) are one menu on
  the CI header, present only when one applies. GitLab artifacts are a
  job's archive and download from the job.
- No optimistic state. An action answers with its pump's CI sequence after
  the poll it asked for; the pressed control stands pending (a stepped
  spinner) until the applied CI sequence reaches it, the call fails or the
  backend's launch changes. The pending action is held under the runs
  (GitHub) or the pipeline (GitLab) it reaches, so it shows on every
  surface whose pipeline shows one of them (a PR's checks and the
  Pipelines tab's view of the same run alike), and while it is pending
  the other actions there wait, their hover text saying why. It settles,
  for every surface at once, by the surface that started it, and is
  dropped when that surface goes. The backend
  holds the runs (GitHub) or the pipeline (GitLab) an action reaches
  while it runs, so a second action reaching any of them, from a PR's
  checks or the Pipelines tab, is refused with "Another action on this
  pipeline is still running."
- A failure is a toast with the backend's one sentence and Details. A
  write whose answer was lost may have been applied: it is never sent
  again, its sentence says it may have been applied, and the pipeline is
  read again as after a success, so the pane shows what the forge did.
- A log open on a job a retry replaced follows the retry when the open
  job is gone from the read, the read is the job's own pipeline (GitLab:
  the same pipeline id; GitHub: the same workflow run), and exactly one
  job in the same stage with the same name has a higher id. Otherwise the
  log stays on the job it opened, so a new commit's pipeline, or another
  job that shares the name, never takes it over. The view says so in a quiet line
  under its header ("The job was retried; this is the new attempt's
  log.") until another job opens.
- A download runs through `SaveCIArtifacts` and shows its control pending
  while it runs. The result shows where the reader is: on the open log's
  "Saved to …" line when it is that job's archive, otherwise as a "Saved
  to …" toast; a single file names the file, several their directory. A
  download that stopped part way is a failure toast naming what was saved.
  A download that finds the artifacts gone clears the flag that offered
  it: the pipeline is read again at once.

### Managing a PR

The review pane manages the PR it shows, on both forges, through
`stores/prManage.svelte.ts` (the editors, the Details pickers and the
pending action) over the bound methods `EditPR`, `SetPRReviewers`,
`SetPRAssignees`, `SetPRLabels`, `SetPRState`, `RerequestPRReview`,
`DismissPRReview` (GitHub), `BypassPRRequestedChanges` (GitLab),
`SearchPRCandidates` and `GetPRMergeMessage`. The forge requests, the
permission reads and the refusals are in
[forge transport](../architecture/forge-transport.md#managing-a-pr).

- Which controls show is `PRDetail.manage.can`, one flag per control,
  each from the forge's own answer and each decided alone (a row's Edit
  by its own flag). A control the viewer may not use is absent. A detail
  without `manage` (an older backend) shows no Details section and none
  of these controls; that absence is the compatibility gate. A GitLab
  host decides per control as well: one whose schema lacks a set
  mutation's `operationMode` offers no Edit on that row, one without
  re-request or the requested-changes override no Re-request or Bypass,
  while the other controls stay. A failed management read sets
  `manage.unavailable`: every flag is false, the lists are empty, and the
  Details section, collapsed, shows the sentence instead of its rows; the
  rest of the PR loads as usual, and the controls return with the next
  snapshot whose read answers.
- Edit affordances are icons revealed on hover (always shown on compact)
  whose space is held, so nothing moves when they appear or go pending.
- Title: the pencil at the end of the title line turns it into one
  input of the line's own height; Enter saves, Escape cancels. A failure
  floats under the input, so the title bar keeps its fixed lines.
- Description: the pencil on the Description section's header opens the
  raw body with Write and Preview (`ChatMarkdown`'s sanitized embedded
  mode), Save and Cancel; Ctrl or Cmd+Enter saves. A save names the body
  the editor opened with (`expectedDescription`); one changed since is
  refused with "The description changed since you opened it. Copy your
  text, reload the description and edit again." and the editor stays
  open. An empty description shows the section when the viewer may edit
  it. Task list items in the rendered description are not tickable: the
  renderer has no seam that maps a rendered checkbox to its source.
- Details, between Merge and Description: Reviewers (each with the
  verdict vocabulary: approved, changes requested, commented, pending;
  an outdated GitHub review is marked), Assignees and Labels (a neutral
  chip with the forge's color as a dot). Collapsed by default, always:
  its header carries a muted one-line summary ("3 reviewers · 1
  assignee · 2 labels", a zero part left out, nothing when all are zero,
  "unavailable" when the read failed); opening it is remembered in the
  review state as the other overview sections are. A settled or outdated
  review has a Re-request icon. Each row's Edit opens a searchable
  multi-select: results load on open and as the reader types (250 ms
  debounce); a query change drops the older results at once (the
  checked entries stay listed) and an older query's answer is dropped;
  the first page only, "Type to narrow" when the forge has more. Labels
  are one option per forge id, so a rename while the picker is open
  changes nothing. Teams are offered
  only on a GitHub organization (`can.reviewerTeams`) and when the search
  returns them. A GitHub reviewer whose review is in stays checked: only
  a pending request can be withdrawn. Closing the picker by any route
  (Escape, Tab, a click outside, its trigger, the pane closing) applies
  the row's change to the PR it opened on. The overview scrolling out and
  back neither applies nor loses an open picker. Each person and
  label passed as the backend gave it (a person addressed by `login`, a
  label by `id` on both forges); nothing is written when nothing changed.
  The backend removes first, then adds, one write per side. A failed
  change shows its sentence on the row, until the row's picker opens
  again or its next write starts. When the remove applied and the add
  did not, the sentence says both ("Removed X …; adding Y was refused:
  …"), or, when the forge's answer was lost, that the add may have
  applied and the pane shows what the forge did. The refusals name the
  cause: GitHub's 10-assignee limit, a person GitLab did not add, a label
  that is not the project's, a host that cannot change the row. The row
  shows the next snapshot either way.
- The title bar's `⋯` at the end of the state line holds Convert to
  draft and Close on an open PR, Reopen on a closed one; a merged PR has
  no menu. No confirm step: each is undone by the item that replaces it.
  Mark ready stays the draft blocker's fix. A held Enter or Space never
  activates a menu item (`MenuItem` ignores a repeated keydown), so the
  press that opened the menu cannot pick the item focus moved to.
- The changes-requested blocker row: GitHub offers Dismiss per review in
  `blocker.changeRequests`, opening a reason field (required; Enter
  dismisses); GitLab offers Bypass when `blocker.bypassable`. A bypass
  stands as the settled row "Requested changes bypassed". A failed
  Dismiss closes its reason field, keeps the reason for that review's
  next Dismiss, and shows its sentence on the row, as a failed Bypass
  does, until the next Dismiss opens or Bypass starts. Once Dismiss or
  Bypass settles, focus returns to its button, or to the Merge header
  when the answer took the button away.
- `Merge ▾` gains `Edit commit message…` for a method that makes a
  commit (merge, squash): a title and body editor in the Merge section,
  prefilled with the forge's default for the method, read when it opens
  and when the method changes until the text is edited. The review state
  holds one selected method beside the message, so the overview's
  remount keeps both: the project's default (the first offered),
  re-resolved on every snapshot, until the reader picks a method or edits
  the message. The editor has no
  method of its own; it shows the selected one, which Merge now and
  Auto-merge send. The message (`commitMessage`) is sent exactly when its
  text is edited and the selected method makes a commit; a failed default
  read shows its sentence in the editor and the merge goes on with the
  forge's default. The title takes
  focus once the default has loaded, unless the reader moved focus
  meanwhile. The editor hides while auto-merge is armed. A selected
  method a snapshot stops offering falls back to the project's default
  through the same path as a pick, keeping edited text: the editor shows the new
  method, the text still marked edited, and a quiet line ("Squash is no
  longer allowed here. This message will be used for the merge
  commit."). When the new method makes no commit the line reads "...
  Rebase makes no commit, so this message will not be used.", the editor
  stays open with the text, and the merge sends no message. A method
  that comes back does not move the selection back; the line goes once
  the reader picks a method or the message is no longer edited. The
  editor always shows what the merge sends.
- Every action follows the merge actions' contract: no optimistic state;
  pending from the call until the pane has applied the snapshot whose
  sequence reaches the answer's `seq` (0 settles at once), given up with
  a new backend launch or a failing pump, and the wait is dropped then;
  a wait of a PR the pane left stays until the pane is back on it and
  its sequence is known. One management action at a
  time per PR, keyed by the PR it started on, so the controls of the PR
  on screen read only its own; withheld with the merge actions' reason
  as hover text. An editor
  whose write is pending cannot be dismissed (Cancel disabled, saying it
  is saving; Escape does nothing), a held key's repeat never submits,
  and every popover and editor returns focus to the control that opened
  it. A failure shows its sentence with Details on the surface that
  caused it: an editor's on the editor, a picker's or re-request's on its
  Details row, Dismiss's and Bypass's on the changes-requested row. A
  state change that fails is a toast, since its menu is gone. The
  surface is chosen when the answer arrives: the row while the pane's
  review state is alive, a toast once it was disposed (the pane closed),
  nothing when the pane's PR changed, for good: a write discarded by a
  PR change toasts nothing when the pane closes later. Each operation
  claims its surface,
  so an older answer never replaces a newer operation's sentence there.
  A completion settles its own PR's draft and error even when that PR is
  briefly not on screen, and never clears a newer operation's state.
- A title or description save that fails after the pane's review state
  was disposed (Back or the pane's close while it is pending) is one
  toast, held until dismissed, through the comment writes' lost-write
  rule (`stores/lostWrite.ts`): "Your description edit was not saved.
  <sentence>", or, for `forge_unconfirmed` and a lost answer, "Your
  description edit was not confirmed. <sentence>" ("Your title edit"
  for the title). `Copy text` copies the text that was sent. A failure
  while the state is alive stays in the editor.
- Each editor, the picker and the commit message belong to the PR (its
  computer and number) they opened on and write only to it. When the
  pane's PR becomes another (a branch change in the same workspace),
  all of it is discarded: open editors and the picker close without
  writing, the commit message and its method go, and a late answer for
  the PR that was is ignored.
- The close, reopen, draft, ready, review requested and review dismissed
  events these cause are rows in the Conversation feed.

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

### Reading the diff

Commit runs. In the PR scope the commit picker picks one commit (a click)
or a run (Shift+click on a second row, or a row's `From here`, which runs
to the newest commit). Commits are numbered from the oldest: the trigger
reads `All commits`, `N/T <short sha>` for one commit or `N–M/T` for a run,
never a subject; its steppers are Older (left) and Newer (right). A run
of the whole list is All commits, never sent. The backend lists at most
the newest 300 commits with the PR's real count: ordinals and `T` count
from the PR's first commit, the menu says once that only the newest N of
T are listed, the oldest listed commit keeps `From here`, and a run of
the whole truncated list stays a run. When the count is unknown there are
no ordinals: a commit reads as its short hash and a run as
`<sha>–<sha>`, at the same trigger width. The run is pane state like a
picked commit: kept across a reload while its commits are listed, and
when one is gone (a rebase, an amend) the pane shows All commits with a
banner saying so. A run's card leads the diff in the commit card's place:
its count, its ordinals (`· to the head` when it ends there), its first
and last commits, and `All commits` to leave. Each commit group in the
Conversation feed has `Changes from here`, the run from the group's
first commit to the newest, landing on the diff.

Comments in a run. A run that ends before the head comments as a single
commit does (to the agent). A run of two or more commits that ends at the
head is read against the whole PR's diff: a new-side line inside one of
the whole PR's hunks takes the PR's comments (`Comment now`, PR drafts,
saved in the whole diff's line numbers); any other new-side line takes an
agent-only draft whose `+` says why on hover ("This line is not part of
the pull request's diff, so a comment here goes to the agent only."); a
removed line takes none. The whole diff's line index is built when the
run opens (from the whole diff on screen, else read once) and dropped when
it closes; until it is ready, or if its read failed (the card says so,
with Retry), every line is agent-only. Nothing is guessed.

Binary files. A binary file whose path is an image (png, jpg, jpeg, gif,
webp, avif, bmp, ico, svg) shows its sides as images: two columns in split
view, old above new in unified, one for an add or a delete, the new side
alone for an edit. Each is captioned with its decoded pixel size and its
byte size; a side over the cap shows its size and that it is too large.
The row's height is reserved before any byte arrives, so nothing moves.
Bytes are read only while the row is near the viewport and released when
it leaves (the media cache's budget). Any other binary file, or an image
the backend cannot serve, shows `Binary files differ` with both sizes and,
when the backend says why, its sentence. A refused read says why on the
row: Retry when the backend was busy, `Reload diff` when the file changed
since the diff was read, and only the sentence when the diff's files
cannot be read at all. The reads name the handle the diff's open
answered, and the bytes are cached under it, so no other diff or pane
shares them. The server forgets a handle when it evicts it or the
connection that opened it ends. A diff of immutable revisions (the PR, a
picked commit or run, a branch commit) is then opened once more for a new
handle, before the next read when the connection ended; a diff that reads
the working tree is not, and its rows offer `Reload diff` without
reading. A diff no longer on screen is never reopened. Rows published
while the diff still streams read under its open from the start. A text SVG keeps its text diff, with one `Rendered` toggle
in its header that shows it as an image (always an `<img>`, never
inline).

Line links. In the PR scope with All commits shown, a line's number
gutter has a context menu (a long press on compact) with `Copy link to
line`: the line's place on the forge's own diff page, in the anchor that
page puts in the address bar (neither forge documents one). GitHub:
`<pr>/files#diff-<sha256 of the path>R<n>` for an added or unchanged line,
`L<n>` for a removed one. GitLab: `<mr>/diffs#<sha1 of the path>_<old>_<new>`,
where an added or removed line takes the other side's running number. A
renamed file is named by its new path. A picked commit or run, and the
workspace, branch and edits diffs, offer no link.

## Non-goals

- Merge queues and merge trains: the forge's own page.
- Milestones, changing the target branch.
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
of the section header. `pulls-page.spec.ts` and its compact twin
(`pulls-page-flow.ts`) drive the page against both forges: the groups
and the states, the forge reads (each project's fixed request set and no
read per row), the GraphQL read that moves a GitLab row to `Pipeline
failed`, filters remembered across a reload, the pinned review, the
user's own comment reading `Reviewed by you` and the author's reply
after it `Your turn`, a refused project failing alone, "Open with agent" opening a
thread on a worktree of the branch, and the Pipelines tab's filters
remembered across a reload, stages and a GitLab and a GitHub job log.
`review-manage.spec.ts` and its compact twin (`review-manage-flow.ts`)
drive the management controls on both forges: a refused title edit and
its retry, the stale-description refusal, each Details picker's change
and the requests it sent, a reviewers change whose add was refused
after its remove applied, re-request, close, reopen, draft and ready, a
refused then retried dismiss (GitHub) and bypass (GitLab) with the
sentence on the blocker row and focus on the Merge header after, and a
merge with an edited commit message, a held Enter on the `⋯` that
activates nothing, Ctrl+W closing the pane writing an open picker's
change (desktop), a description save refused after Back (compact) or
the pane's close (desktop) as one held toast whose Copy text holds the
description, and an edited squash message that falls back to Merge
and then to Rebase as the snapshot drops each method, with the notice
and a merge that sends no message; on GitLab also a refused
management read (Details shows the sentence, no control anywhere, the
controls back once it answers) and a host without `operationMode` (no
row Edit, the other controls kept). `review-ci-actions.spec.ts` drives the CI actions on a GitLab MR and a
GitHub PR (a refused retry, a retry followed to passed, cancel, a manual
job, artifacts saved to disk, a refused artifacts read that keeps the
jobs and says why in the menu), and `ci-actions-flow.ts` the same from
the Pipelines tab on desktop and compact.
`pullsList.browser.test.ts` covers the lists' virtualization: the
mounted rows stay bounded over thousands, a list at its top stays there
when its rows move, a chosen sort opens from the top, and the Pipelines
list reports the rows on screen as it scrolls, changes and hides. `tool-panes.spec.ts`, `workflows-pane.spec.ts` and their
compact twins cover the tools row and the pane kinds.
`review-comment-actions.spec.ts` drives the comment actions on both
forges: a line comment, a reply and a PR comment posted now after one
refusal that keeps the text, a draft and a held reply submitted as one
review, an edit, a reply's delete and a thread's only comment's delete,
a range over unchanged and added lines, and a held reply sent to the
agent quoting its thread.
`review-diff-reading.spec.ts` and its compact twin
(`review-diff-reading-flow.ts`) drive reading the diff on both forges: a
run by Shift+click and by `From here`, its card and leaving it, a head
run's `Comment now` and agent-only line, `Changes from here`, image rows
in unified and split view, an over-cap and a non-image binary file, the
SVG toggle and a line's copied link.
