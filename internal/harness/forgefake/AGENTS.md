# Fake forge

`forgefake` answers the `gh pr create`, `glab mr create` and repository
`ssh -G` invocations an isolated boot makes, and the requests of its forge
API transport (every other GitHub and GitLab call), from a fixture a test
seeds. The CLI path is:

1. `internal/git` runs every forge CLI through `Core.runSpec`. Under
   `--harness` and `--soak`, `WithIsolatedForgeCLIs` (`forge_cli.go`)
   executes `cmd/ao-mockforge` in its place and sets `AO_FORGE_CLI` to the
   impersonated name. With no fake configured the call fails before
   anything runs; it never falls through to `PATH`.
2. `ao-mockforge` forwards argv, cwd and stdin to the harness over the
   control channel (`internal/harness/control`, `POST /forge`) and prints
   the answer.
3. `Engine.Handle` routes the call, answers from the seeded fixture and
   records it. `internal/harnessrpc` exposes `HarnessForgeSeed`,
   `HarnessForgeInvocations`, `HarnessForgeOffline`,
   `HarnessForgeRateLimit`, `HarnessForgeRefuse`, `HarnessForgeRelease`,
   `HarnessForgeAdvanceJob` and the `harness:forge` event. `HarnessReset`
   clears the fixture, the log, the offline state, the rate limits and the
   refusals, and releases every held answer.

The HTTP path: `harnessrpc.StartForgeAPI` serves `Engine.ServeHTTP` on its
own loopback listener and hands its base URL and fixed token to the boot
(`app.SetForgeAPI`), whose `forgeapi.Service` is built with
`Options.Isolated` (see
[forge transport](../../../docs/architecture/forge-transport.md#isolation)).
`ServeHTTP` maps a REST request onto the `api` route it stands for
(`http.go`) and a GraphQL request onto the operation its `operationName`
names (`github_graphql.go`; GitLab's in `gitlab_lists.go`), and answers
from the same fixture. Offline
drops the listener's open connections and records each request that
arrives as route `offline`, dropping it without a reply.

The engine lives in the harness process, not the binary, so seeding and
inspection go through the harness wire with no fixture files.

## Answer rules

- A call no route claims, or one carrying a flag, `--json` field, query
  parameter or header its handler does not implement, is unhandled: exit 1
  with the full argv on stderr (CLI), or 404 with the method and path in
  `detail` (HTTP), recorded with `unhandled: true`. So is a GraphQL
  request for an operation the fake does not implement, or whose
  document declares, or whose request supplies, other variables than the
  operation's entry in `githubGraphQL` or `gitlabGraphQL`. An HTTP request with a token
  other than the fixed one is an unhandled 401; one with no token at all
  (the boot's dev-server scan probing its own listeners) is a 401 that is
  not recorded. Specs assert none occurred
  (`e2e/tests/forge-helpers.ts`, `expectEveryForgeCallHandled`).
- A well-formed call for something the fixture lacks (an unknown PR, a
  missing upload) answers the forge's own not-found error: GitHub GraphQL
  answers 200 with a `NOT_FOUND` error beside null data, REST answers 404.
  That is a handled call.
- A GraphQL handler reads its operation's variables, never the query
  text, and answers only the parts the document's `@include` and `@skip`
  variables select. Its nodes carry the shapes the real API returns for
  the app's selections: every connection has `pageInfo`, an absent time
  is `null`, and an actor without a display name has no `name` key.
- A handler implements everything its route admits. Do not add a route
  that accepts a mutation and answers success without changing state. A
  mutation's request body is decoded strictly: a parameter its handler
  does not implement is unhandled. A mutation the forge would refuse
  (a moved head, a blocked merge, a viewer without the role) answers the
  forge's own refusal and changes nothing.
- A refusal (`SetRefusal`, `HarnessForgeRefuse`) makes one route answer
  a given status, headers and JSON body in place of its handler, for
  `times` requests or until cleared (status 0): 401, 403, 405, 409 or
  422 on any route, or 200 with an `errors` body on a GraphQL operation,
  which is how GitHub refuses a mutation. The route is a REST route's
  name (`glab api approve`), `gh graphql <operation>` or
  `glab graphql <operation>`. It is answered
  after the route matched and before its handler runs, so nothing
  changes, and is recorded under the route. With `hold` the request is
  recorded at once and answered only on `ReleaseRefusal`
  (`HarnessForgeRelease`, `{route, drop}`), so a spec can act while it is
  in flight; with `drop` the held request is closed with no reply, its
  answer lost. A 401 on a mutation is
  retried once after the transport's token refresh and then probed with
  `GET user`, so drive one with `times` 0.
- `routes.go` is the whole CLI and REST route table, `githubGraphQL` and
  `gitlabGraphQL` the whole GraphQL ones. A route's flag list is closed; a REST route's flags
  are the request properties `httpCall` synthesizes (`hostname`,
  `method`).
- A handler answers status and headers through its response (`status`,
  `header`). A route that lists the `if-none-match` flag is conditional:
  its 200s carry an ETag (a hash of the body) and a request naming the
  current one is a bodiless 304 under the same route. Only routes whose
  real endpoints send an ETag the app revalidates list it
  ([measurements](../../../docs/references/forge-api-measurements.md#conditional-requests));
  on any other route `If-None-Match` is an unimplemented flag. `Range` is
  likewise a flag only the two job log routes list, each answering it as
  its forge does (below); on any other route it is unhandled. A log
  (`body`) is written after the engine's lock is released.
- A rate limit (`SetRateLimit`) is per forge and pool: GitHub `core`
  (REST) and `graphql`, GitLab `throttle_authenticated_api`. With
  `remaining` 0 every API request to the pool is answered as the forge
  refuses it (GitHub REST 403, GitHub GraphQL 200 with a `RATE_LIMITED`
  error, GitLab 429 with `Retry-After`), each carrying the quota headers
  and recorded under route `rate limited`; otherwise requests are
  answered as usual with the pool's quota headers (`limit` 5000,
  `remaining` as set, not counting down). The limit lifts in the second
  of its `reset`. Attachments are outside the quota.

## Adding an endpoint

1. Add the fixture field the answer needs to `fixture.go`, with defaults
   and validation in `normalize`.
2. Add one handler in `github.go`, `github_graphql.go` or `gitlab.go` and
   one row in `routes.go` (a `commands` entry, or an `api` route with its
   method, pattern and allowed flags) or in `githubGraphQL` (the
   operation's kind, name and variables).
3. Test the routing and refusal in `engine_test.go`, and drive the app's
   own code against the engine in `cmd/ao-mockforge/binary_test.go`, whose
   rig runs the CLIs through the real binary and the forge API transport
   through the engine's HTTP mounts. The second test is what catches drift
   between the app's request and the handler.
4. Update the table below.

## Invocations

Handled, covering the reads the review pane, the Pull requests page, git
status and CI make, the thread resolve toggle and the Create PR/MR
dialog. Every row but the CLI ones is served over HTTP only: GitHub REST at `/github/rest/E`, GraphQL at
`/github/graphql` (a POST of `{"query", "operationName", "variables"}`),
an attachment URL at `/github/absolute/<host>/<path>`, GitLab REST at
`/gitlab/api/v4/E`, GitLab GraphQL at `/gitlab/api/graphql` (recorded as
`glab graphql <operation>`). The request's Host header is the invocation's
recorded `host` (an attachment's is the host in its path); a GraphQL
invocation also records its `operation` and `variables`, and a REST
write its JSON `body` (at most 64 KiB), so a spec can assert what the
fake received. P is the
project's path, escaped as one segment (`grp%2Fsub%2Ftool`).

| Forge | Invocation (route) | App caller |
|---|---|---|
| GitHub | `GET repos/O/R` (`gh repository identity`) | repository identity; ReadPipeline's CI permission (`permissions` from the repository's `merge.permission`) |
| GitLab | `GET projects/P` (`glab repository identity`, conditional) | repository identity; the merge settings and the viewer's access the Merge section reads |
| ssh | `-G -o CanonicalizeHostname=no -o PermitLocalCommand=no [-l U] H` | local SSH host resolution, from fixture `sshHosts` |
| GitHub | `gh pr create --title T --body B [--base B] [--draft]` | GitCreatePR |
| GitLab | `glab mr create --title T --description D --yes --no-editor [--target-branch B] [--draft]` | GitCreatePR |
| GitHub | GraphQL `PRTick(owner, name, number, wantDetail, wantThreads, wantEvents, wantChecks)` (`gh graphql PRTick`) | ReadPR: detail and viewer with the repository's merge settings and the merge box (readiness, opinions, thread states, base protection, auto-merge), threads and comments, the newest 100 timeline items, head commit rollup (CI, each check's `isRequired`); `viewerPermission` on every read |
| GitHub | GraphQL `PRThreadsPage`, `PRCommentsPage`, `PRThreadStatesPage(owner, name, number, after)` | ReadPR: review threads, comments and thread states past the first 100 |
| GitHub | GraphQL `PRTimelinePage(owner, name, number, before)` | ReadPR events: older timeline pages, read backwards |
| GitHub | GraphQL `ThreadCommentsPage(threadID, after)`, `RollupContextsPage(commitID, number, after)` | ReadPR: one thread's comments, the rollup's contexts past 100 |
| GitHub | GraphQL `PRMergeTarget(owner, name, number)`, `ViewerApproval(owner, name, number)` | merge actions: the pull's node id and head; Unapprove: the viewer's standing approval |
| GitHub | GraphQL `MergePR`, `EnableAutoMerge(pullRequestID, method, expectedHeadOid)` | Merge now (merged, MergedEvent and ClosedEvent, head deleted when the repository does) and Auto-merge (armed, the method's enabled event); a head other than `expectedHeadOid` answers "Head branch was modified. Review and try the merge again." |
| GitHub | GraphQL `DisableAutoMerge`, `MarkPRReady(pullRequestID)`, `UpdatePRBranch(pullRequestID, expectedHeadOid)`, `DismissReview(reviewID, message)` | Cancel auto-merge, Mark ready, Update branch (the head moves to a new commit), Unapprove (the review reads DISMISSED, a ReviewDismissedEvent) |
| GitHub | `POST repos/O/R/pulls/N/reviews` with `{event, body?, commit_id?, comments?}` (`gh api pull reviews`) | Approve and SubmitReview: the viewer's review, a timeline item, and a thread of the viewer's per line comment (`{path, body, line, side, start_line?, start_side?}`, its line not checked against the diff); the author's approval and a comment without a side are GitHub's 422 |
| GitHub | `POST repos/O/R/pulls/N/comments` with `{body, commit_id, path, line?, side?, start_line?, start_side?, subject_type?}` (`gh api pull comments`) | PostPRLineComment: a thread of the viewer's on a line, a range (one side) or a file (`subject_type: "file"`); a `commit_id` other than the head is a 422 (the fake models no older commit) |
| GitHub | `POST repos/O/R/pulls/N/comments/ID/replies`, `POST repos/O/R/issues/N/comments` with `{body}` (`gh api pull comment reply`, `gh api issue comments`) | ReplyToPRThread and SubmitReview's held replies (404 unless ID is a thread's first comment), PostPRComment (a conversation comment) |
| GitHub | GraphQL `CommentOwner(id)` | EditPRComment and DeletePRComment: the comment's type and the pull request and repository it is on; an id no repository on the host has is a `NOT_FOUND` |
| GitHub | GraphQL `UpdateReviewComment`, `UpdateIssueComment(id, body)`, `DeleteReviewComment`, `DeleteIssueComment(id)` | EditPRComment and DeletePRComment by node id (`PRRC_ao<id>`, `IC_ao<id>`): the author or a WRITE, MAINTAIN or ADMIN viewer may edit (sets `editedAt`) and delete, else `FORBIDDEN`; a thread whose only comment goes is deleted with it |
| GitHub | `DELETE repos/O/R/git/refs/heads/B` (`gh api delete branch`) | the head branch delete after a merge: 204, then 422 "Reference does not exist" |
| GitHub | GraphQL `SetThreadResolved(threadID, resolved)` | SetThreadResolved (flips the seeded thread) |
| GitHub | GraphQL `PullsSearch(query, after)` | project PR list: `search` over `is:pr is:open repo:O/R` (any other query is unhandled), the repository's open pulls newest first in pages of 100, beside the viewer, each with its last 10 `comments` and submitted `reviews`, its opinionated reviews (state, `viewerDidAuthor`), the first 100 review threads' `isResolved` with `pageInfo`, and the base's `refUpdateRule` (its `requiredApprovingReviewCount` alone) and `rules` as PRTick answers them; a repository not on the host answers GitHub's search refusal (null `search` beside an error) |
| GitHub | GraphQL `OpenPRsByHead(owner, name, head, after)` | open PR lookup (checkout origin): pages of 100 with each PR's node id, `baseRefName` and `isCrossRepository` (a pull with `headRepo`) |
| GitHub | GraphQL `PRStates(ids)` | merged-PR cleanup: state and `mergedAt` of tracked PRs by node id; an unknown id is a `NOT_FOUND` error at its `nodes` index |
| GitHub | GraphQL `MergedPRs(owner, name, first, after)` | merged heads (checkout origin), in fixture order |
| GitHub | `GET repos/O/R/actions/runs?[per_page=N][&page=N][&branch=B][&status=S]` (`gh api runs`, conditional) | pipelines list: every run of the repository, highest id first, `status` a run status or conclusion; `Link` rel="next" while more pages exist |
| GitHub | `GET repos/O/R/actions/runs/ID` (`gh api run`, conditional) | ReadPipeline: one run |
| GitHub | `GET repos/O/R/actions/runs/ID/jobs?per_page=N[&page=N]` (`gh api run jobs`, conditional) | CI steps of a followed job's run, ReadPipeline's jobs; `Link` rel="next" while more pages exist |
| GitHub | `GET repos/O/R/actions/jobs/ID/logs` (`gh api job logs`, conditional) | CI job log (the current `log` of a started job; 404 while the job is pending and while it sets `logWithheld`, as the real endpoint does until the job's log blob exists, which for a running job is its whole run; a running job with a log models the jobs API still reporting a completed job running); answered directly where the real endpoint redirects to its log blob, with the blob's ETag and Range behavior: a start or closed range is a 206 with `Content-Range`, a suffix range is ignored (200, whole body), a start at or past the end is a 416 (HTTP range semantics; not measured on the forges) |
| GitHub | `POST repos/O/R/actions/jobs/ID/rerun` (`gh api job rerun`) | retry a job: 201, the job replaced by a pending attempt under a new id; 403 while its run is live, for a viewer below write, or "created over a month ago" for a run created more than 30 days ago |
| GitHub | `POST repos/O/R/actions/runs/ID/rerun-failed-jobs` (`gh api run rerun failed`) | retry the failed jobs: 201, each failed or canceled job replaced; 403 while the run is live, when it has none, or for a run created more than 30 days ago |
| GitHub | `POST repos/O/R/actions/runs/ID/cancel` (`gh api run cancel`) | cancel a run: 202, its live jobs canceled; 409 once it completed |
| GitHub | `GET repos/O/R/actions/runs/ID/artifacts?[per_page=N][&page=N]` (`gh api run artifacts`) | the CI read's artifacts flag, ListCIArtifacts: the run's `artifacts`, expired ones included, each with its `expires_at`; `Link` rel="next" while more pages exist |
| GitHub | `GET repos/O/R/actions/artifacts/ID/zip` (`gh api artifact zip`) | artifact download, answered directly where the real endpoint redirects to its blob; 410 once `expired` |
| GitHub | `GET <attachment URL>` (`gh api attachment`) | forge attachments |
| GitLab | `GET projects/P/merge_requests/N` (`glab api merge request`, conditional) | ReadPR, once per read: detail, head SHA for threads, head pipeline for CI, the merge fields with `detailed_merge_status` derived from the fixture |
| GitLab | `GET projects/P/merge_requests/N/approvals` (`glab api approvals`) | ReadPR detail: approvals required and left, the viewer's standing |
| GitLab | GraphQL `PullsReviewing`, `PullsAuthored(fullPath, viewer, after)` | project PR list: the open merge requests `viewer` reviews or authored, newest update first in pages of 100, each with its reviewers' `reviewState`, `approvedBy` and `headPipeline.status`; a project not on the host is a null `project` |
| GitLab | GraphQL `PullsReviewingActivity`, `PullsAuthoredActivity(fullPath, viewer, after)` | project PR list: the same merge requests with resolvable and resolved discussion counts and the head commit's `committedDate` |
| GitLab | GraphQL `PullsReviewingApprovals`, `PullsAuthoredApprovals(fullPath, viewer, after)` | project PR list: the same merge requests' `approvalState.rules`, one `ANY_APPROVER` rule requiring the pull's `readiness.requiredApprovals` |
| GitLab | `GET users/ID/events?per_page=N[&page=N][&after=YYYY-MM-DD][&action=commented\|approved]` (`glab api user events`) | project PR list: the viewer's (id 1) `commented on` notes on merge requests (target type `Note` for a conversation comment, `DiffNote` for a thread comment) and `approved` events across the host's projects, newest first, created after the date, `action` keeping one kind as GitLab filters it; another id is a 404, another action unhandled; `X-Next-Page` while more pages exist, a repository's `eventsHidden` events dropped from a page after it is selected |
| GitLab | `GET projects/P/merge_requests/N/discussions?per_page=N[&page=N]` (`glab api discussions`) | ReadPR threads and, from the system notes, events; `X-Next-Page` while more pages exist |
| GitLab | `POST .../approve` with `{sha}`, `POST .../unapprove` (`glab api approve`, `glab api unapprove`) | Approve (409 on a moved head, 401 for a viewer GitLab takes no approval from) and Unapprove (404 with none), each with its system note |
| GitLab | `PUT .../merge` with `{sha, squash, should_remove_source_branch, auto_merge, merge_when_pipeline_succeeds}` (`glab api merge`) | Merge now (405 while blocked, 401 without the role, 409 on a moved head) and Auto-merge (either key or both: arms a blocked merge request, with its system note) |
| GitLab | `POST .../draft_notes` with `{note, position?, in_reply_to_discussion_id?, resolve_discussion?}`, `GET .../draft_notes`, `DELETE .../draft_notes/ID`, `POST .../draft_notes/bulk_publish` (`glab api create draft note`, `glab api list draft notes`, `glab api delete draft note`, `glab api publish draft notes`) | SubmitReview: a viewer's draft (a summary, positioned on a line, a range or a file as a discussion's position is, or `in_reply_to_discussion_id` naming a discussion with `resolve_discussion`, else GitLab's 400) answers its id; the list is the viewer's pending drafts in one page (how a publish whose answer was lost is settled); a delete removes the viewer's draft with that id (404 otherwise); publish makes each of the viewer's pending drafts, seeded `draftNotes` included, a thread, a conversation comment or a note of the discussion it replies to (which `resolve_discussion` resolves and its absence reopens; a reply whose discussion went is dropped and the publish still succeeds, as on GitLab), and leaves another author's pending |
| GitLab | `GET projects/P/merge_requests/N/diffs?per_page=N[&page=N]` (`glab api merge request diffs`) | PostPRLineComment and SubmitReview: the pull's `diff` by file (`old_path`, `new_path`, the hunks, `new_file`, `renamed_file`, `deleted_file`); `X-Next-Page` while more pages exist |
| GitLab | `POST .../discussions` with `{body, position}`, `POST .../notes` with `{body}` (`glab api create discussion`, `glab api create note`) | PostPRLineComment (a position whose shas are not the merge request's, or whose file, by both paths, or line the pull's `diff` lacks, is a 400; a `line_range` point whose `line_code` is not sha1 of the new path and the diff's two cursors at its line, or whose `type` is not GitLab's, is unhandled; a range may start on the other half, a removed line through an unchanged one, and reads back with its `line_range` as posted), PostPRComment |
| GitLab | `GET .../discussions/D` (`glab api discussion`) | a held reply's draft refused with a 400: one discussion, 404 when gone |
| GitLab | `POST .../discussions/D/notes` with `{body}`, `PUT .../discussions/D/notes/N` with `{body}`, `DELETE .../discussions/D/notes/N` (`glab api create discussion note`, `glab api update discussion note`, `glab api delete discussion note`) | ReplyToPRThread (a thread, or a conversation comment's discussion, whose `replies` it joins; 404 otherwise), EditPRComment and DeletePRComment (the author's, or any as maintainer or owner, else 403); a discussion whose last note goes is deleted, one whose first note goes keeps its id |
| GitLab | `POST .../cancel_auto_merge`, `PUT .../rebase` | Cancel auto-merge (409 with none armed), Update branch (202, the head moves) |
| GitLab | `PUT projects/P/merge_requests/N` with any of `{title, description, state_event}` (`glab api update merge request`) | edit (a title's draft prefix sets the draft state), close and reopen (`close`, `reopen`, with their state events, a reopen recorded as `opened`); 403 for a viewer who may not update, 400 for an empty title and for an update naming nothing (`{}` or `null`), as GitLab answers |
| GitHub, GitLab | `GET user` (`gh api user`, `glab api user`) | the fixture's viewer (GitLab's with account id 1): GitLab's detail and project list reads (once per host per 10 minutes), and the transport's probe after a 401 on a mutation on both forges |
| GitLab | `GET projects/P/merge_requests?state=opened\|merged&...` (`glab api merge request list`, conditional) | open MR lookup, merged heads (checkout origin), project MR list; the full view, with `project_id` and `source_project_id` (a pull with `headRepo` answers its fork's id) and the list fields (author, reviewers, draft, times, `user_notes_count`, conflicts, blocking discussions, `detailed_merge_status`), no pipeline |
| GitLab | `GET projects/P/merge_requests?iids[]=N...&state=all&per_page=100` (`glab api merge request list`) | merged-PR cleanup: state and `merged_at` of tracked MRs |
| GitLab | `GET projects/P/pipelines?[per_page=N][&page=N][&ref=R][&status=S][&scope=tags\|branches]` (`glab api pipelines`) | pipelines list: every pipeline of the project, highest id first; a scope answers each tag's latest tag pipeline or each branch's latest branch pipeline (a branch may share a tag's name), then the ref and status filters apply; `X-Next-Page` while more pages exist |
| GitLab | `GET projects/P/pipelines/ID` (`glab api pipeline`) | ReadPipeline: one pipeline; ListPipelines: its `tag` flag, for a pipeline on a tag's name outside the tag scope |
| GitLab | GraphQL `PipelineJobs(fullPath, id, after)` (`glab graphql PipelineJobs`) | ReadPR CI, ReadPipeline's rows: the pipeline's stages in fixture order and its current rows (no retried job) newest first in pages of 100, with the CI action fields (`retryable`, `cancelable`, `playable`, `canPlayJob`, `userPermissions` from the viewer's role and the project's `cancelRole`, the archive), each a build, a bridge or an external commit status by its `kind` (gid, `webPath`, `detailsPath`, `downstreamPipeline` as gitlab.com answers them); a project not on the host is a null `project`, a pipeline it lacks a null `pipeline` |
| GitLab | `GET projects/P/jobs/ID/trace` (`glab api job trace`, conditional) | CI job log (the current `log` of a started job, running included; 404 while the job sets `logWithheld`). A completed job's trace honors a start or closed range (206) and refuses a suffix range (400); a running job's trace ignores `Range` (200, whole body) and a running `synthetic` trace grows on every request |
| GitLab | `POST projects/P/jobs/ID/retry` (`glab api job retry`) | retry a job: 201 with the new pending job; 403 "Job is not retryable" for a live, replaced or non-build job |
| GitLab | `POST projects/P/jobs/ID/cancel`, `POST projects/P/jobs/ID/play` (`glab api job cancel`, `glab api job play`) | cancel a live job (201; a finished one is unchanged; 403 below the project's `cancelRole`), run a manual job (200, pending; 400 "Unplayable Job" otherwise) |
| GitLab | `POST projects/P/pipelines/ID/retry`, `POST projects/P/pipelines/ID/cancel` (`glab api pipeline retry`, `glab api pipeline cancel`) | retry the pipeline's failed and canceled jobs (201), cancel its live jobs (200; 403 below the project's `cancelRole`) |
| GitLab | `GET projects/P/jobs/ID/artifacts` (`glab api job artifacts`) | artifact download: the job's `artifacts` archive; 404 without one |
| GitLab | `GET projects/P/uploads/SECRET/NAME` (`glab api upload`) | forge attachments |
| GitHub | `POST`, `DELETE repos/O/R/pulls/N/requested_reviewers` with `{reviewers?, team_reviewers?}` (`gh api request reviewers`, `gh api remove reviewers`) | reviewers: each added user or team pending with its ReviewRequestedEvent (a re-request included); 403 below triage unless the viewer authored the pull, 422 for the author, a non-member or an unknown team, changing nothing |
| GitHub | `POST`, `DELETE repos/O/R/issues/N/assignees` with `{assignees}` (`gh api add assignees`, `gh api remove assignees`) | assignees; as GitHub does, a non-member, one past 10 assignees, and any change by a viewer who may not assign, is ignored without an error |
| GitHub | GraphQL `PRAssignees(owner, name, number)` | the pull's assignees, which the app counts against GitHub's 10 before a change |
| GitHub | GraphQL `PREditTarget(owner, name, number)`, `EditPR(pullRequestID, title?, body?)` | edit: the node id and body the precondition compares, then the change |
| GitHub | GraphQL `PRLabelTarget(owner, name, number, ids)`, `AddLabels`, `RemoveLabels(labelableID, labelIDs)` | labels: the repository and pull ids and each id's label with its repository (an unknown id is a `NOT_FOUND` error at its `nodes` index); a label of another repository is GitHub's `NOT_FOUND` |
| GitHub | GraphQL `ClosePR`, `ReopenPR`, `ConvertPRToDraft(pullRequestID)` | close, reopen, convert to draft, each with its timeline event; refused for a viewer who may not close, reopen or update respectively, or a pull already in that state |
| GitHub | GraphQL `SearchAssignable(owner, name, query)`, `SearchTeams(org, query)`, `SearchLabels(owner, name, query)` | pickers: the repository's owner (`__typename`, `login`) and `members`, an `organization`'s `teams`, or `labels`, 20 per page with `hasNextPage`; `teamsRefused` answers the teams search as GitHub does a token without `read:org` (null `data` beside an `INSUFFICIENT_SCOPES` error); a login that is no organization is `NOT_FOUND` |
| GitHub | GraphQL `PRMergeMessage(owner, name, number, method)`, `MergePRMessage`, `EnableAutoMergeMessage` (as `MergePR`, `EnableAutoMerge` with `commitHeadline`, `commitBody`) | the default commit message (the fake's own wording, after GitHub's defaults), and a merge or auto-merge with an edited one |
| GitLab | GraphQL `PRManageSchema` | the schema probes `rereview`, `updateInput`, `setReviewers`, `setAssignees` and `setLabels`, answered from the host's first GitLab repository as its `serverLacks` leaves them |
| GitLab | GraphQL `PRManage`, `PRManageChecks(fullPath, iid)` | the Details read: `userPermissions`, reviewers with `reviewState`, assignees, labels with their global ids; `PRManageChecks` adds `mergeabilityChecks` (`REQUESTED_CHANGES` FAILED, SUCCESS once overridden, else INACTIVE) and is refused on a server that lacks the override |
| GitLab | GraphQL `PRSetReviewers`, `PRSetAssignees(projectPath, iid, usernames, mode)`, `PRSetLabels(projectPath, iid, labelIds, mode)`, `PRSetDraft(projectPath, iid, draft)` | the set mutations, `mode` `APPEND` or `REMOVE`, each answering the merge request's list or draft state: as GitLab does, a non-member and an unknown label id are skipped without an error (an added reviewer gets its note); denied (null field beside GitLab's permission message) below developer; refused before execution on a server that lacks `operationMode`. The draft mutation moves the title's prefix, with its note |
| GitLab | GraphQL `PRRereview(projectPath, iid, userId)`, `PRBypassChanges(projectPath, iid)` | re-request (the reviewer reads unreviewed, their change request cleared, a note) and bypass (`changesOverridden`, so `detailed_merge_status` leaves `requested_changes`); refused on a server that lacks them, re-request for a viewer who may not update or a user who is no reviewer, bypass below developer |
| GitLab | GraphQL `PRMergeMessage(fullPath, iid)` | the default merge and squash commit messages (GitLab's default templates) |
| GitLab | `GET projects/P/users?search=&per_page=N[&page=N]` (`glab api project users`), GraphQL `SearchProjectLabels(fullPath, search?)` | pickers: `members` matching search with `X-Next-Page` while more pages exist; the project's labels with their global ids, 20 with `hasNextPage` |
| GitLab | `GET projects/P/merge_requests/N/resource_state_events?per_page=N[&page=N]` (`glab api state events`) | ReadPR events: the pull's `stateEvents` (`closed`, `opened` or `reopened` for a reopen, `merged`) |

Unhandled (fail with their request): GitLab's discussion resolve PUT
and `cancel_merge_when_pipeline_succeeds`. A lost or cut-short answer is not
a fake behavior: a test wraps the engine in a handler that runs the route
and then drops or truncates the response.

## Fixture notes

- A request answers only for a repository whose seeded `host` equals its
  Host header as given, port included; a GraphQL node (a thread or commit
  id) resolves only among repositories on that host. Another host answers
  the forge's not-found.
- `gh pr create` and `glab mr create` resolve the repository from the
  call's cwd (`git config remote.origin.url`). The seeded `host` and
  `project` must match that origin. An HTTP request names its project.
- A GitLab path may name the project by its numeric `id` instead of its
  path; both resolve.
- A create opens the pull for the cwd's checked-out branch and commit
  (read with git) into `main` unless the call names a base, numbered one
  past the repo's highest. It does not check that the branch was pushed.
- GitHub's viewer is the fixture's `viewer`, answered on every PRTick that
  wants the detail; a reseed changes it on the next read.
- A pull's merge box is its `readiness` (approvals and checks the base
  requires, conversation resolution, behind, an armed auto-merge, a GitLab
  approval override), its repository's `merge` settings and the viewer's
  role, and its reviews, CI, threads and `mergeable`. Omitted, nothing is
  required and the viewer can write, so an open pull with no conflicts is
  ready on both forges. GitHub's `mergeStateStatus` and GitLab's
  `detailed_merge_status` are derived from them in each forge's order,
  never seeded. A GitLab draft is its title prefix, as on GitLab.
- `events` are the timeline items (GitHub) or system notes (GitLab) a
  pull carries besides its reviews; every action appends its own. A
  GitHub review is its own timeline item. A seeded GitLab approval has no
  note: seed an `approved` event beside it for one. An armed auto-merge
  stays armed until merged or canceled: the fake does not merge it when
  the pull becomes ready.
- `pipelines` are a repository's pipelines beside its pulls' `ci`; the
  lists answer both, highest id first, and a pipeline id and a job id
  are each unique in their repository (a seed that repeats one is
  refused). A pipeline's `createdAt` defaults to the seed time. A pull's `ci` defaults to a merge request pipeline of its
  head (GitLab ref `refs/merge-requests/N/head`). A pipeline's `refKind`
  is answered as each forge can: GitHub only through the run's event
  (`source`, defaulting to push, release or pull_request by kind), GitLab
  through the merge request ref, the tag scope (each tag's latest tag
  pipeline) and the single pipeline's `tag`.
- `reviewers` are the logins asked for a review (GitHub
  `reviewRequests`, GitLab `reviewers`); a GitHub review from one ends its
  request. `changeRequests` are the GitLab reviewers who requested
  changes (their reviewer state, and `detailed_merge_status`
  `requested_changes`); an approval replaces the viewer's. A GitLab
  reviewer's `reviewState` is `approved` while their approval stands,
  `requested_changes` when in `changeRequests`, `reviewed` once they
  commented (a thread comment or a note), else `unreviewed`; the
  viewer's comments and approvals are also its events feed. `createdAt`,
  `updatedAt`, `committedAt` and `commits` feed the project list rows.
  A GitLab repository's `eventsHidden` makes its viewer events ones
  GitLab no longer shows the viewer: the events feed selects them into a
  page and drops them after, so a page comes back short and still names
  the next, as GitLab's does.
- The management fixture: a repository's `members` (each with its GitLab
  user id, generated when omitted) are whom reviews can be requested from
  and who can be assigned; `teams`, `organization` and `teamsRefused` are
  a GitHub organization's; `labels` are the repository's (GitHub ids
  generated: GitHub `LA_ao<n>`, GitLab `gid://gitlab/ProjectLabel/<n>`;
  a name may hold a comma). A pull's `teamReviewers` ("owner/slug"),
  `assignees` and `labels` name them. A pull's `viewer` overrides what the
  viewer may do (`update`, `close`, `reopen`, `label`, `assign`, `triage`,
  `admin`), each its own knob, otherwise derived from the role and
  authorship. A GitLab merge request's `stateEvents` are its
  close, reopen and merge events (a seeded closed or merged one without
  one gets one) and `changesOverridden` a standing bypass. A GitLab
  repository's `serverLacks` (`rereview`, `overrideRequestedChanges`,
  `operationMode`) makes it an older server without them.
- Who may edit or delete a comment follows from its `author` and the
  viewer's role: on GitHub the author or WRITE and above edits and
  deletes (answered as `viewerCanUpdate` and `viewerCanDelete`); on
  GitLab, whose notes carry no permission, the author or a maintainer or
  owner edits and deletes. Seed a comment of another author's, with a
  GitHub role below WRITE or GitLab's default role, for one the viewer
  may not edit. A
  comment's `editedAt` is GitHub's `lastEditedAt` and GitLab's
  `updated_at`. A GitLab conversation comment's `replies` are the rest of
  its discussion.
- A GitLab position resolves against the pull's `diff`, a unified `git
  diff` (`diff --git`, `rename from`/`rename to`, `new file mode`,
  `deleted file mode` and hunks); a pull with none takes no positioned
  comment. A thread's `oldPath` is a renamed file's path before the
  change, answered as `old_path`. The e2e `publishPullRequest` seeds a
  GitLab pull's `diff` from the pushed change.
- Generated ids start at 1,000,000 and stay unique across reseeds and
  resets.
- A GitLab job's `kind` makes it a `bridge` (its `downstream` pipeline
  path, none until set) or an `external` commit status (its `targetUrl`,
  stage `external` by default) instead of a build; neither has a log, and
  its trace is the forge's 404. A GitHub run holds builds only.
- A job's `status` may be `manual` on GitLab (a job waiting to be
  played). A retry, on both forges, marks the job `retried` and appends a
  pending attempt of it under a new id; the reads answer only the jobs no
  retry replaced. Every CI action refuses a viewer below write (GitHub)
  or developer (GitLab) with the forge's 403; the refusal messages are
  the fake's. A GitLab project's `merge.cancelRole` (developer by
  default, maintainer or no_one) gates cancel alone, so `cancelBuild` and
  `updateBuild` can differ. GitHub refuses a re-run of a run created more
  than 30 days ago. `artifacts` are a GitHub run's (`pipeline.artifacts`,
  each named, `expired` and `expiresAt` optional) or a GitLab build's
  archive (`job.artifacts`), each `text` or `base64`; the other forge's
  form is refused at seed.
  `AdvanceJob` (`HarnessForgeAdvanceJob`) moves a current job to a later
  status, starting and completing it as its runner would, and the
  pipeline's status follows its current jobs.
- A job's `log` is served as the forge would serve it, and the app's log
  view splits it into rows from what it carries. A GitHub log splits by
  its steps: give each line its own time prefix
  (`2026-01-01T00:00:04.0000000Z text`) and each started step its
  `startedAt`, or the lines stay in one row for the job. A GitLab log is
  the raw trace: write section markers as the runner does
  (`section_start:<unix>:<name>\r\x1b[0K<header>`), and the app's trace
  cleaner turns them into sections.
- A job's `synthetic` log (`SyntheticLog`, `synthetic.go`) is generated
  line by line on each request instead of stored, so a spec can serve a
  log of many megabytes: sections of a fixed number of fixed-width lines,
  each GitHub section stamped with the time of the job's matching started
  step and opened by its `##[group]<name>` line, each GitLab section
  wrapped in the runner's markers. A running GitLab job serves
  `runningLines` lines on its first trace request and `growLines` more on
  each later one (`HarnessReset` restarts the count); a completed job
  serves all of it. Its ETag names the spec and the extent served.

Run `go test ./internal/harness/forgefake ./cmd/ao-mockforge` after a
change here, and the Playwright specs that seed a forge
(`rg -l seedForge e2e/tests`).
