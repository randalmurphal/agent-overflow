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
  chip with the forge's color as a dot). Collapsed by default while
  every row is empty; the reader's choice lives in the review state. A
  settled or outdated review has a Re-request icon. Each row's Edit opens
  a searchable multi-select: results load on open and as the reader
  types (250 ms debounce, an older query's answer dropped), the first
  page only, "Type to narrow" when the forge has more. Teams are offered
  only on a GitHub organization (`can.reviewerTeams`) and when the search
  returns them. A GitHub reviewer whose review is in stays checked: only
  a pending request can be withdrawn. Closing the picker (Escape, Tab, a
  click outside, its trigger) applies the row's change, each person and
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
  Mark ready stays the draft blocker's fix.
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
  and when the method changes until the text is edited. Only an edited
  message is sent with Merge now and Auto-merge (`commitMessage`); a
  failed default read shows its sentence in the editor and the merge
  goes on with the forge's default. The editor hides while auto-merge is
  armed.
- Every action follows the merge actions' contract: no optimistic state;
  pending from the call until the pane has applied the snapshot whose
  sequence reaches the answer's `seq` (0 settles at once), given up with
  a new backend launch or a failing pump; one management action at a
  time; withheld with the merge actions' reason as hover text. An editor
  whose write is pending cannot be dismissed (Cancel disabled, saying it
  is saving; Escape does nothing), a held key's repeat never submits,
  and every popover and editor returns focus to the control that opened
  it. A failure shows its sentence with Details on the surface that
  caused it: an editor's on the editor, a picker's or re-request's on its
  Details row, Dismiss's and Bypass's on the changes-requested row. A
  state change that fails is a toast, since its menu is gone.
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
merge with an edited commit message; on GitLab also a refused
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
