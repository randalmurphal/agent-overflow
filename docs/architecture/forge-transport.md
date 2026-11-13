# Forge API transport

`internal/forgeapi` talks to GitHub and GitLab over HTTPS with the token of
the user's `gh` or `glab` login. `internal/git` keeps forge semantics and
response parsing; `internal/appupdate` reads the GitLab release feed through
the same client. This document fixes the shapes those packages share. API
contracts live as comments on the exported types.

Measured facts behind the design are in
[forge API measurements](../references/forge-api-measurements.md).

## What still runs a CLI

This is the complete list. Everything else is an HTTP request.

| Command | Why a process |
|---|---|
| `gh pr create`, `glab mr create` | Pushes the branch and may prompt; runs interactive under the user's git credentials. |
| `gh auth token --hostname H` | Token handoff for a GitHub host. |
| `glab config get token --host H`, `glab config get api_host --host H`, `glab config get api_protocol --host H`, `glab config get is_oauth2 --host H` | Token and API base handoff for a GitLab host. |
| `glab auth status --hostname H -t` | Token handoff when glab keeps the token in the OS keyring (`config get token` is empty). |
| `glab api --hostname H version` | Makes glab refresh an expired OAuth2 login before the token is read again. |
| `ssh -G host` | Host alias resolution for remote identity; not a forge call. |

A forge method that needs a process goes through `git.Core.runSpec` as today.
`forgeCLIPolicy` keeps pinning those binaries to the fake in an isolated boot.

## Composition

```go
// internal/forgeapi
type Service struct { /* per-host clients, token sources, gates, caches */ }

func New(opts Options) (*Service, error)        // misconfiguration is a construction error
func (s *Service) GitHub(host string) *Client   // REST base + GraphQL URL for host
func (s *Service) GitLab(host string) *Client   // REST base + GraphQL URL for host
func (s *Service) Close()                       // zero every token, close idle connections
```

Every `git.Forge` method takes a leading `context.Context`: the PR pump's
context ends with the pump, a bound method's with its call, and
`forgeapi.WithInteractive(ctx)` marks a user's action (opening a PR pane,
Refresh, Save, Send, Submit, Reply, Resolve) so the same forge method is
interactive from a click and background from the pump without a second
signature. The context's deadline is the request deadline when it is
shorter than `Request.Timeout`.

`forgeapi.SetupError{Forge, Binary, Kind, Message, Err}` is the one setup
error type (`Kind` is `missing` or `unauthenticated`); `git` re-exports
nothing, and `appupdate`'s release feed restates it with the host's
`glab auth login --hostname HOST` step. The type lives in `forgeapi` because `git` imports
`forgeapi`, so the token source must not return a `git` type.

`App.newGitCore` constructs one `Service` per process and hands it to
`gitops.NewCore(gitops.WithForgeAPI(svc))`; a construction error fails the
boot's store phase. A `Core` built without a `Service` (what `gitCore()`
returns before `Start` or after a failed one) answers every forge request
with `git.ErrNoForgeAPI`. The updater receives the same `Service`.
`App.Shutdown` closes it. The
isolated boot constructs it with `Options.Isolated` (below) beside
`ForgeCLI`, so a test cannot reach a real forge by construction. `Options.TokenSource` is the
CLI-backed source in production and a fixed fake token when isolated.

## Hosts and base URLs

Three kinds of forge method, two sources of the host:

- PR-scoped methods (`ReadPR`, `SubmitReview`, `ReplyToThread`,
  `SetThreadResolved`, the merge and comment actions) take a
  `PRReference`, which carries `Host` beside
  `Forge`, `Namespace`, `Repo` and `Number`. The host comes from the PR URL
  every reference is built from (`ParsePRURL`, `prRefFromUrl`); the wire
  shape and the frontend `PRRef` carry it, and `PRReference.Validate`
  (run by every PR-scoped `git.Core` method) refuses an empty one. `Host`
  is spelled as the browser's `URL.host`: lowercase, a default or empty
  port dropped, so both sides build the same key. These methods take no
  `cwd`, so no PR read depends on a checkout's remote (the workflow paths
  used to pass the item worktree, and glab then took the host from it).
  A request goes to the client of `Host`, port included; a GitHub
  attachment is the one absolute URL, requested as given. The PR key stays
  `forge:namespace/repo:number` on the forge's public host and becomes
  `forge@host:namespace/repo:number` elsewhere, so existing keys and
  persisted draft sourceKeys are unchanged.
- Repository-scoped methods that need no checkout (`ListProjectPRs`,
  `ListPipelines`, `ReadPipeline`, `StreamCIJobLog`, `RunCIAction`,
  `ListCIArtifacts`, `StreamCIArtifact`, `FetchAttachment`,
  `ReadPRStates`) take a `RepoReference`: a `PRReference` without the
  number (`PRReference.Repository()`), checked by
  `RepoReference.Validate`, keyed `forge:namespace/repo` or
  `forge@host:namespace/repo` as the PR key is. `Core.OriginRepository`
  builds one from a checkout's origin (below) and fails with
  `ErrOriginNotForge` for a checkout whose origin names no repository on
  a supported forge.
- Checkout-scoped methods (`ListOpenPRs`, `ListMergedPRHeads`,
  `CreatePR`, repository identity) take `cwd`; host and project path come
  from the one origin read forge detection already does
  (`Core.originCoordinates` over the cached `originIdentity`, through
  `repoidentity.Locator` and `classifyOriginURL`). An unknown or
  unparseable origin fails with `*git.OriginUnknownError` instead of
  guessing. GitLab names the project in the path as
  `projects/<url.PathEscape(path)>`. GitHub branch lookups use GraphQL
  `pullRequests(headRefName:)`, which needs no head owner and also matches
  forks' branches of the same name; the lookup drops those (below).
  Repository identity (`forge:host:id`) already carries the
  host; GitHub's is `GET repos/OWNER/REPO`, GitLab's
  `GET projects/<escaped path>`.

| Host | REST base | GraphQL |
|---|---|---|
| `github.com` | `https://api.github.com/` | `https://api.github.com/graphql` |
| GitHub Enterprise `H` | `https://H/api/v3/` | `https://H/api/graphql` |
| GitLab `H` | `api_protocol://api_host/api/v4/` from glab's per-host config, default `https://H/api/v4/` | `api_protocol://api_host/api/graphql`, default `https://H/api/graphql`; one quota pool with REST |

Base URLs are resolved once per host and kept with the host's client.

## Credentials

```go
// Token holds one host's bearer secret in a secret.Bytes. Only authorize
// reads it; reflection over a holding struct prints an address.
type Token struct {
	secret *secret.Bytes
	header AuthHeader
}

func (t *Token) authorize(req *http.Request) // Authorization: Bearer / Private-Token
func (t *Token) Zero()
func (Token) String() string      // "<redacted>"
func (Token) GoString() string    // "<redacted>"
func (Token) Format(fmt.State, rune)
func (Token) MarshalJSON() ([]byte, error)
func (Token) MarshalText() ([]byte, error)
```

- Source: `gh auth token --hostname H` for GitHub (honors `GH_TOKEN`,
  `GH_ENTERPRISE_TOKEN` and the hosts file). For GitLab,
  `glab config get token --host H`, then `glab auth status --hostname H -t`
  when that is empty (keyring). A glab login with `is_oauth2` true runs
  `glab api --hostname H version` before every read so glab refreshes an
  expired OAuth2 token first; the source keeps no state about expiry.
- Read: one read per client at a time, shared by every request that needs
  it and run on the Service's lifetime with a one-minute bound
  (`credential.go`). A request that leaves ends only its wait, never the
  handoff command, which may hold a refreshed login. `Close` ends and
  joins it.
- Expiry: GitLab answers an expired or revoked token with a bare 401, so
  the client learns expiry while the token works (`expiry.go`). A glab
  token that is not OAuth2 (`SourceInfo.ExpiryLookup`) costs one
  `GET personal_access_tokens/self` on the first request after it is read,
  once per token fingerprint, inside that request's `prepare` and context,
  10s bound; the client keeps `expires_at` (a date, UTC midnight) and
  `revoked`. The lookup never sends its own lookup, never refreshes on its
  401, and never fails the request: a failure is logged once per host and
  that token is not asked again. GitHub's expiry is the
  `GitHub-Authentication-Token-Expiration` response header (fine-grained
  and expiring tokens), kept per fingerprint from any response, no extra
  request. An isolated boot's fixed token is never looked up.
- Header: GitHub `Authorization: Bearer`. GitLab `Private-Token` for a
  personal token, `Authorization: Bearer` for an OAuth2 login (both verified
  against gitlab.com; glab itself sends `Private-Token` for a PAT).
- Lifetime: read at first use per host and re-read on the first request
  after 5 minutes (so `gh auth switch` or a new `glab auth login` takes
  effect) or after one 401; the old bytes are zeroed on replace. A failed
  read, setup or transient, is cached for 5s so pollers do not fork the
  CLI every tick, and every interactive request drops that cache before
  it reads (`Service.DropNegative(host)`), so the user's next action after
  a fresh login reads the token at once. Nothing is written
  to disk, settings or the SQLite store.
- The handoff process runs with a 10s deadline and
  `GH_PROMPT_DISABLED=1`, `GH_DEBUG=`, `GLAB_CHECK_UPDATE=false`,
  `GLAB_DEBUG_HTTP=false`. A missing binary is `Kind: "missing"`; a
  non-zero exit whose stderr names the login (`gh auth login`, `not logged
  in`, `no token`) is `Kind: "unauthenticated"`; any other failure (locked
  keyring, timeout) is a `*TransientError` carrying the exit status, never
  "not logged in".
- Fingerprint: `sha256(token)` keys the rate gates and the ETag store, so
  a token never appears in a map key, a log or a test name.
  `Client.CredentialFingerprint(ctx)` returns the current token's, read
  as a request reads it (first use, the 5-minute re-read, a 401), for a
  caller that caches what the login is allowed to see: GitLab's viewer
  and project settings are looked up by it and published under
  `Response.Credential`, the fingerprint of the token the request sent
  (a refresh between the lookup and the send, or a 401 inside the call,
  can have replaced the one the lookup saw), so
  another login never reads the
  previous one's.
- Where the token must never appear: argv, environment, log lines, error
  strings, `StatusError` bodies, the diagnostics bundle, `uiRenderTrace`,
  harness invocation records, the transport wire. Only `authorize` touches
  the bytes, and it runs inside the client's `RoundTripper`.
- `CheckRedirect` drops `Authorization` and `Private-Token` when the target
  host differs from the request host. GitHub job logs redirect to Azure blob
  storage with a signed URL; that URL is data, not a credential, and is
  redacted from errors by `redactForgeRequest` as today.

## Reads per tick

The PR pump reads a PR with one `Forge.ReadPR(ctx, ref, want, prev,
stepsFor)` per tick. `want` (`git.PRReadParts{Detail, Threads, CI, Events}`) names
the parts the caller needs and the read fetches nothing else: the pump
asks for `Detail`, `Threads` and `Events` (the forge events the
Conversation feed shows: approvals, merges, draft flips, auto-merge,
force pushes), the CI phase for `CI` alone on its own cadence. A merge
action (`ApprovePR`, `UnapprovePR`, `MergePR`, `CancelPRAutoMerge`,
`UpdatePRBranch`, `MarkPRReady`) or comment action (`PostPRLineComment`,
`PostPRComment`, `ReplyToPRThread`, `EditPRComment`, `DeletePRComment`,
`internal/app/app_pr_comments.go`), management action ([Managing a PR](#managing-a-pr)) or suggestion or reaction action ([Suggestions and reactions](#suggestions-and-reactions)) that succeeds asks the PR's live pump for
one snapshot poll at once (`prPumpRequest.snapshot`), so every pane on the
PR follows the forge's answer without waiting out the interval, and
answers with the pump's sequence after that poll (`PRActionResult.Seq`,
zero with no pane on the PR or when that poll did not run or failed, since
then no frame shows the change and the pump's failure frame marks the pane
stale) so the pane knows when the answer is on its screen; the
readiness, merge options and events in the detail come from that read,
never from the action's own response. `git.Core` keeps `GetPRDetail`, `ListReviewThreads` and
`ListPRCIJobs` as one-part wrappers for the callers that need one part.

GitHub answers a read with one GraphQL request, `PRTick`:

| Variable | Selects |
|---|---|
| `owner`, `name`, `number` | the pull request and the repository's `viewerPermission` (the merge box's and the CI actions' permission); both are always selected |
| `wantDetail` | `viewer`, the detail fields, `latestReviews(first: 100)`, and what readiness and merge options derive from: the merge state and viewer flags, `autoMergeRequest`, `latestOpinionatedReviews(first: 100)`, `threadStates: reviewThreads(first: 100) { isResolved }`, `baseRef { refUpdateRule rules(first: 100) }`, and the repository's merge methods, `deleteBranchOnMerge`, `autoMergeAllowed`, `viewerDefaultMergeMethod` and `owner { __typename }`, and the management fields (`viewerCanUpdate`, `viewerCanClose`, `viewerCanReopen`, `viewerCanLabel`, `viewerCanAssign`, `reviewRequests`, `assignees` and `labels`, each `first: 100`, and each standing review's `commit { oid }`; see [Managing a PR](#managing-a-pr)) |
| `wantChecks` | `commits(last: 1)`: the head commit's `id` and `statusCheckRollup.contexts(first: 100)`, each with `isRequired(pullRequestNumber: $number)`; true for `Detail` or `CI`, since the detail's check summary and readiness and the pipeline all come from the rollup |
| `wantThreads` | `reviewThreads(first: 100)`, each with `comments(first: 100)`, and `comments(first: 100)`, every comment with its `reactionGroups` while `wantReactions`; and the pull's `state` and `viewerCanApplySuggestion` (see [Suggestions and reactions](#suggestions-and-reactions)) |
| `wantEvents` | `timelineItems(last: 100, itemTypes: [...])`: the review, merge, close, reopen, draft, auto-merge, force-push, base-change and review-request items `ReviewEvent` maps |

The document is the same every tick; the parts are `@include` directives
on those variables. A connection longer than one page continues with its
own operation from the page's `endCursor`: `PRThreadsPage` and
`PRCommentsPage(owner, name, number, after)`, `ThreadCommentsPage(threadID,
after)` and `RollupContextsPage(commitID, number, after)` through `node(id:)`.
`PRThreadStatesPage(owner, name, number, after)` continues the thread
states only when readiness needs them (the base requires conversation
resolution and the first page has no unresolved thread);
`MergeReadiness.Unresolved` is the exact count when the states fit one
page and nil otherwise, and
`PRTimelinePage(owner, name, number, before)` reads the timeline backwards
from the tick's newest page. Each connection reads at most
`githubReadMaxPages` (10) pages of 100. A
CI read adds REST requests only for what the rollup lacks: the jobs list
of a run (`repos/O/R/actions/runs/ID/jobs?per_page=100`, paged by `Link`)
while a job in `stepsFor` has steps that are not settled in `prev`, and a
job's log on request (`StreamCIJobLog`: `Stream` with `NoLimit` into the
CI log store, 404 is `ErrCIJobLogNotFound`), and each completed run's
artifacts list (`repos/O/R/actions/runs/ID/artifacts?per_page=100`) for
the pipeline's artifacts action, read until a page holds an artifact
that has not expired or the list ends (at most `githubReadMaxPages`
pages). GraphQL has no artifacts field, so that answer is remembered per
run and job set (a re-run's new jobs read it again) for 10 minutes, never
past the earliest `expires_at` of the artifacts it found, and then
revalidated through the ETag store; a live run is not asked. A download
that finds a run's artifacts gone or expired drops the run's answer and
has every pump showing the run poll, so the flag clears with the
sentence the user already got. A failed artifacts read (or, for
`ReadPipeline`, permission read) leaves the CI read whole: the flags it
would have set are absent, `CIPipeline.ActionsUnavailable` carries one
sentence naming only what the failed read took away, and the failure is
logged. The failure is remembered as the open-PR lookup's are (20
seconds, doubling to 15 minutes, or a rate limit's release when later),
so it is asked again when that is due, not every tick. The read carries
that due time (`ActionsRetryAt`, not on the wire), and while the sentence
stands the CI phase schedules its next read for it, capped at 15
minutes, for a PR's CI and a Pipelines-tab pipeline alike, so a completed
pipeline recovers once the read succeeds.

| Tick | GitHub requests |
|---|---|
| Pump, a PR within one page per connection | 1 (`PRTick`) |
| Pump, threads past the body bound only with their reactions | 2 (`PRTick` with reactions, then without) |
| CI phase, no followed job | 1 (`PRTick`); + 1 artifacts page per completed run per attempt per 10 minutes |
| CI phase, a followed running job | 1 (`PRTick`) + 1 jobs-list page per run |
| CI phase, a followed running job's log | 1 logs request (a 302, then the blob's 404 until it exists, in practice until the job completed) |

A GitHub merge action reads the PR's node id, head branch and the
repository's `deleteBranchOnMerge` with `PRMergeTarget(owner, name,
number)`, then runs its mutation; `Approve` and `Unapprove` need no node
id.

| Action | GitHub requests |
|---|---|
| `Approve` | 1 REST `POST repos/O/R/pulls/N/reviews` (`event: APPROVE`, `commit_id`) |
| `Unapprove` | 1 `DismissReview` (`dismissPullRequestReview`), after 1 `ViewerApproval(owner, name, number)` (`latestOpinionatedReviews`) when the caller has no review id |
| `Merge` | `PRMergeTarget` + `MergePR` (`mergePullRequest`, `expectedHeadOid`), + 1 REST `DELETE repos/O/R/git/refs/heads/B` when asked and the repository does not delete merged branches |
| `Merge` with `AutoMerge` | `PRMergeTarget` + `EnableAutoMerge` (`enablePullRequestAutoMerge`, `expectedHeadOid`) |
| `CancelAutoMerge`, `UpdateBranch`, `MarkReady` | `PRMergeTarget` + `DisableAutoMerge`, `UpdatePRBranch` (`expectedHeadOid`) or `MarkPRReady` |

Each comment action is one request; GitHub decides who may edit or delete
and the read carries its answer (`viewerCanUpdate`, `viewerCanDelete`,
`lastEditedAt`, `url` on every comment, no extra request). A conversation
comment is a thread whose ID is its comment's (`CommentTarget`), which is
how an edit or delete picks the issue comment mutation.

| Comment action | GitHub requests |
|---|---|
| `PostLineComment` | 1 REST `POST repos/O/R/pulls/N/comments` (`commit_id` the caller's head, `path`, `line`/`side` with `start_line`/`start_side` for a range, or `subject_type: file`) |
| `PostPRComment` | 1 REST `POST repos/O/R/issues/N/comments` (answers the comment's `node_id`) |
| `ReplyToThread` | 1 REST `POST repos/O/R/pulls/N/comments/ROOT/replies` under the thread's first comment; a 404 is `*ReplyThreadGoneError` |
| `EditComment`, `DeleteComment` | 1 `CommentOwner(id)` (`node { __typename, pullRequest { number, repository { nameWithOwner } } }`: a node id is global, so a comment that is not of the target's kind on the named PR is refused as `*CommentNotOnPRError`, kind `not_found`, before any write), then 1 `UpdateReviewComment`/`UpdateIssueComment(id, body)` or `DeleteReviewComment`/`DeleteIssueComment(id)` |
| `SubmitReview` | 1 REST `POST .../reviews` with the line and range comments, then 1 `POST .../comments` per file comment and 1 `.../replies` per held reply, in order; a COMMENT review with no body and no line comments is skipped when follow-ups exist. A follow-up's failure after anything posted is a `*PartialSubmitError` naming the file (`FailedPath`) or the thread (`FailedThreadID`) |

GitLab's `ReadPR` reads the merge request once
(`projects/P/merge_requests/N`) and derives every part from it: `Detail`
adds its approvals, the management read (GraphQL `PRManage` or
`PRManageChecks`, after a `PRManageSchema` probe once per host per 10
minutes; see [Managing a PR](#managing-a-pr)), the project's merge settings (`projects/P`, cached
per login and project for 10 minutes, then revalidated through the ETag
store: a `W/` ETag answered 304) and the viewer's login (`user`, cached
per login and host for 10 minutes; the author may cancel an auto-merge, and a refused read
is cached as an unknown viewer), `Threads` and `Events` its discussions
(`per_page=100`, paged by `X-Next-Page`, positions normalized against
`diff_refs.head_sha`; one read serves both, system notes are the events),
`Events` also the resource state events (`resource_state_events?per_page=100`,
at most 3 requests: every page while they fit; past them page 1 and the
last two by `X-Total-Pages`, or the first 3 without it): GitLab records a close, a reopen (`opened`, or
`reopened` on some servers) and a merge as these rather than as notes,
and the merge request's `merged_at`/`closed_at` stand in for a merge or
close without one. A failed state events read leaves the events without
them, is logged, and is not asked again for that merge request until its
retry is due (20 seconds doubling to 15 minutes; a success resets it), and `CI`
the rows of its `head_pipeline` (GraphQL `PipelineJobs(fullPath, id,
after)`: its builds, bridges and external commit statuses, which the REST
jobs list lacks, 100 per page by cursor, at most `gitlabCIJobsMaxPages`
(5) pages, with each build's and the pipeline's `retryable`,
`cancelable`, `playable`, `canPlayJob`, `userPermissions` and archive
`artifacts` for the CI actions). Every field it selects exists from
GitLab 16.0 except the cancel permissions (`JobPermissions.cancelBuild`,
`PipelinePermissions.cancelPipeline`, 16.6), which alone decide cancel
where they exist, since a project can restrict cancel past update. A
server whose schema lacks them refuses the whole document ("Field
'cancelBuild' doesn't exist"); the read is sent once more without them,
update deciding cancel as it did there, and the host is read that way for
10 minutes. A build's trace is read on request (`StreamCIJobLog`, as on
GitHub); a bridge or a commit status has no log and no job id, as a
GitHub external check.

| Tick | GitLab requests |
|---|---|
| Pump (`Detail`, `Threads`, `Events`), warm | 1 merge request + 1 discussions page per 100 discussions + 1 state events page per 100 events (at most 3) + 1 approvals + 1 `PRManage` (or `PRManageChecks`) |
| Pump, the notes changed, the viewer reacted, or the reactions read is 2 minutes old | the warm requests + 1 `MRNoteReactions` per 100 notes (at most 10) |
| Pump, cold host or project (first read, then every 10 minutes) | the warm requests + 1 `PRManageSchema` per host + 1 `projects/P` per login and project + 1 `user` per login and host |
| CI phase, a head pipeline | 1 merge request + 1 `PipelineJobs` per 100 rows |
| CI phase, no head pipeline | 1 merge request |
| Log follow, a followed running job (every 5s) | 1 trace (304 while unchanged) |

GitLab's REST notes carry no permission and no edit time: the
discussions read has `updated_at`, which a resolve also moves, and no
`last_edited_at`, so GitLab comments report no edit time. A read of
`Threads` derives who may edit and delete each note from the cached
viewer login and project role (the same `user` and `projects/P` reads the
detail makes): the author on its own notes, a maintainer or owner on
every note (GitLab's "Edit comments posted by other users"). A note's ID
is its decimal id and its URL the merge request's page at `#note_<id>`.

| Comment action | GitLab requests |
|---|---|
| `PostLineComment` | 1 merge request read, then the diffs read below, then 1 `POST .../discussions` with `{body, position}` against its `diff_refs`; a head other than the caller's is `*HeadMovedError` (kind `stale_head`) before the post |
| `PostPRComment` | 1 `POST .../notes` (answers the note's `id`) |
| `ReplyToThread` | 1 `POST .../discussions/D/notes`; a 404 is `*ReplyThreadGoneError` |
| `EditComment`, `DeleteComment` | 1 `PUT` (`{body}`) or `DELETE .../discussions/D/notes/N` |
| `SubmitReview` | The diffs read below when a comment is positioned, 1 `POST .../draft_notes` per comment, then `bulk_publish`. With held replies, 1 discussions read before (a discussion that is gone fails the submit with `*ReplyThreadGoneError` before any draft; each reply's draft carries `in_reply_to_discussion_id` and `resolve_discussion` set to its discussion's state, since publishing a reply without it reopens a resolved discussion) and 1 after the publish (GitLab drops a reply whose discussion went in between and answers success, so a reply that is not a new note of its discussion is a `*PartialSubmitError` naming its thread, `UnpostedReplies` its ordinal). A 400 on a reply's draft is the discussion gone when `GET .../discussions/D` answers 404. The publish posts every draft at once, so the result counts every file comment posted whatever becomes of the replies and the approval, and each draft is sent but the replies that check could not find. An approval that fails after that check failed or found a reply missing is reported beside it (`PartialSubmitError.ApproveErr`), not in its place |

A position names its file by `old_path` and `new_path` and, in a
`line_range`, each end by a `line_code` of `sha1(new_path)` and the old
and new cursor of the diff at that line (an added line's old cursor is
the next old line, a removed line's new cursor the next new line) with
`type` `new`, `old`, or none for an unchanged line. The caller names a
file by one path and a line by its own numbers, so a positioned write
first reads `GET .../merge_requests/:iid/diffs?per_page=100`
(`readGitLabDiffFiles`, `internal/git/gitlab_diff.go`; at most 30 pages)
and takes both paths and both cursors from it. A named path is the entry
whose new path it is; the entry whose old path it is (a file renamed
away) stands in only when no entry has it as its new path, which is known
once every page is read, so the read stops early only when every path
was found as a new path. A file or line that diff lacks, or a file GitLab collapsed or
found too large for lines, fails the write before anything is posted. An
unchanged line outside every hunk keeps the offset of the gap it is in.
A thread read back takes a range's side and last line from its
`line_range` end and its first line from the start's number on the
start's own half, as GitHub's `startLine` is, with that half as
`startSide` (GitHub `startDiffSide`): a range from a removed line to an
unchanged one reads on the right from the removed line's old number, with
`startSide` left.

Git status looks up a branch's open PR (`lookupOpenPR`,
`internal/git/status_pr_cache.go`). The forges' head-branch filters also
match a fork's PR from a branch of the same name, so `ListOpenPRs` keeps
only PRs whose head is the origin repository: GitHub
`OpenPRsByHead` nodes with `isCrossRepository` false (paged, at most
`githubReadMaxPages` pages of 100), GitLab merge requests whose
`source_project_id` is their `project_id` (the full view, since the simple
one lacks it; paged by 100 until one is found, at most
`gitlabOpenMRsMaxPages` pages). A branch with no
`refs/remotes/origin/<branch>` has no PR, so it is answered without a
forge request. An answer is cached for `prLookupTTL` (30s), a "no open PR"
included and never longer. A failed lookup is retried after 20s, doubling
per consecutive failure of that (workspace, branch) up to 15 minutes, and
keeps showing the PR it last found; a success resets the count. Both the
TTL and the wait run from when the forge answered, so a request that hung
does not use up the wait after it. The
status watcher re-runs the lookup when that retry is due
(`GitStatus.OpenPRRecheckAt`), since a forge that recovers changes nothing
on disk; `AO_HARNESS_TIMING` `pr-lookup-retry` shortens the first retry
for specs. An answer outlives its expiry as the branch's last known PR:
only an answer from the forge replaces it, and the fast status a
subscribe returns carries it, marked pending, while the lookup runs. The
cache is bounded by `prCacheMaxEntries` (evicting the entry stored
longest ago), not by age. `InvalidatePRCache` (a subscribe, an explicit
refresh) makes a workspace's answers stale, and a lookup in flight
across it does not cache its result; `ForgetPRCache` drops them when the
worktree is removed, in the app or outside it. The app's create action, and an
agent command that settles running `gh pr create` or `glab mr create`
(`git.CreatesPullRequest`), invalidate and look the branch up again at
once (`Core.RefreshOpenPR`), so the new PR shows without waiting out a
cached "no open PR" and is tracked whether or not a pane watches it.

## Project lists

The Pull requests page reads each project's open PRs with one
`Core.ListProjectPRs(ctx, repo)` (`internal/git/pr_list.go`), and its
Pipelines tab a project's pipelines on any ref with one
`Core.ListPipelines(ctx, repo, filter)` and a selected one with
`Core.ReadPipeline(ctx, repo, id, prev, stepsFor)` (`ci.go`,
`ci_github.go`, `ci_gitlab.go`). The rows' fields, and which a forge
leaves zero, are documented on `PRListRow` and `PipelineSummary`.

`ListProjectPRs` returns every field the page shows, so no row is read
on its own. Its requests run concurrently; only a failed list fails the
read. Another failed request leaves its fields unknown (nil or empty) and
comes back as a `*PRListDetailError` beside the rows, which the page shows
as the project's `DetailError`.

GitHub lists open PRs with one GraphQL `PullsSearch(query, after)`:
`search(type: ISSUE, query: "is:pr is:open repo:O/R", first: 100)`
beside `viewer { login }`. Each node selects its review state
(`reviewDecision`, `reviewRequests(first: 20)`,
`latestOpinionatedReviews(first: 100) { state viewerDidAuthor }`, whose
`APPROVED` count is the approvals given, `viewerLatestReview`, since the
login is not known before the answer), the approvals required
(`baseRef { refUpdateRule rules(first: 100) }`, as the detail read
derives them), `reviewThreads(first: 100) { pageInfo { hasNextPage }
isResolved }` (the unresolved count, unknown when the threads run past
the page), its merge state, `totalCommentsCount`, `commits(last: 1)`
with the head commit's time and rollup `state` (the CI status), and
`comments(last: 10)` and `reviews(last: 10)` with each author and time,
from which the row takes the PR author's latest reply
(`AuthorActivityAt`; a review thread reply is a review of its own) and
the count of the author's replies after the viewer's latest review. The
viewer's activity is `viewerLatestReview.submittedAt`.

GitLab reads, per repository, the viewer first (`user`, cached per login
and host for 10 minutes; a viewer that cannot be read fails the
project), then concurrently:

- `projects/P/merge_requests?state=opened&order_by=updated_at&sort=desc&per_page=100`,
  the rows, whose basic view carries no pipeline.
- GraphQL `PullsReviewing` and `PullsAuthored(fullPath, viewer, after)`:
  the opened merge requests the viewer reviews or authored
  (`reviewerUsername:` or `authorUsername:`, `sort: UPDATED_DESC`), each
  with its reviewers' `mergeRequestInteraction.reviewState`,
  `approvedBy` and `headPipeline.status`.
- `PullsReviewingActivity` and `PullsAuthoredActivity`, the same filters
  selecting `resolvableDiscussionsCount`, `resolvedDiscussionsCount` and
  the head commit's `committedDate`.
- `PullsReviewingApprovals` and `PullsAuthoredApprovals`, the same
  filters selecting `approvalState { rules { type approvalsRequired } }`:
  the merge request's own rules, its target branch and any override
  applied. `approvalState` is an EE field: GitLab CE answers that it
  does not exist, which marks the host as having no required approvals
  for 10 minutes (the covered rows require none, and the two reads are
  not sent). The required count is the larger of the any-approver rule's
  and the other rules' sum (an approval from a rule's member also counts
  toward any approver), which matched `approvalsRequired` on all 56 merge
  requests measured. The review, the discussion and commit, and the
  approval fields each cost about a second on a large project, so they
  are separate concurrent requests; `approvalsRequired` and
  `approvalsLeft` are never selected, since they add about 2 s.
- `users/ID/events?action=commented&per_page=100&after=<30 days ago>`
  and the same with `action=approved`, concurrently, each at most
  `gitlabActivityMaxPages` (5) pages: the viewer's latest
  `commented on` (a note, diff note or discussion note on a
  `MergeRequest`) or `approved` event per project and merge request is
  the row's viewer activity. GitLab filters each feed, so pushes and
  other events do not use up the pages. GitLab drops the events the
  reader may not see after selecting a page, so a page can be short and
  still name a next page: the read follows `X-Next-Page` whatever the page
  held, and a feed ends at the page that names none. An ended feed's floor
  is the day after the after date; a feed cut at the cap's is its oldest
  event read (now when it read none). The window is complete only back to
  where both feeds are, so its floor is the later of the two. A row whose
  activity in the window is missing or older than the floor carries it
  as `ViewerActivityFloor`, and attention takes the later of the two as
  the viewer's activity, since a cut feed can hide activity newer than
  the other feed found. They are read once per host per round
  (`git.WithPRListRound`, one per tick or prefetch): the round's
  concurrent project reads share it and a later round replaces it.

A merge request neither filter returns carries no review detail. Both
forges read at most `prListMaxPages` (10) pages of 100 per connection:
GitHub search answers at most 1000 results, and GitLab's list stops at
the 1000 most recently updated.

| Read | GitHub | GitLab |
|---|---|---|
| Open PRs of a project | 1 `PullsSearch` per 100 PRs | 1 merge request list page per 100 MRs + 6 GraphQL reads, each 1 page per 100 MRs it returns + 2 to 6 `users/ID/events` (two feeds) per host per round; + 1 `user` per login and host per 10 minutes |
| Pipelines of a project (`ListPipelines`, at most `Limit` rows, 100 when unset or larger) | 1 `GET repos/O/R/actions/runs?per_page=N[&branch=][&status=]` | 1 `GET projects/P/pipelines?per_page=N[&ref=][&status=]`; + `GET projects/P/pipelines?per_page=100&scope=tags[&ref=]` pages, at most `gitlabTagScopeMaxPages` (10): 1 for a ref, 1 or 2 without a status filter, none for a merge request ref or an empty list; + 1 `GET projects/P/pipelines/ID` per row on a tag's ref name whose id the tag scope does not hold, once per pipeline while the repository's listings hold it and shared by concurrent listings, 4 at a time per listing |
| One pipeline (`ReadPipeline`) | 1 `GET repos/O/R/actions/runs/ID` + 1 jobs page per 100 jobs, at most `githubReadMaxPages`; + 1 `GET repos/O/R` (the viewer's `permissions`) per login and repository per 10 minutes, and the completed run's first artifacts page as the CI phase reads it | 1 `GET projects/P/pipelines/ID` + 1 `PipelineJobs` per 100 rows, at most `gitlabCIJobsMaxPages` |
| Stages of 30 unchanged rows (`ReadPipelineStages`) | 30 runs + 30 jobs pages, each a 304 | 30 pipelines + 30 `PipelineJobs`, each a 200 with its body |

`PipelineListFilter.Status` is a normalized status each forge filters by
its own value: success, failed, running, pending (GitHub `queued`),
canceled, skipped, and manual on GitLab only; another value is refused
before any request. A GitHub run is one row and, read, one stage named
after its workflow; its jobs list carries every job's steps, so
`stepsFor` costs no extra request. A row's `RefKind` is `merge_request`
for a GitLab `refs/merge-requests/N` ref or a GitHub `pull_request`
event, `tag` for a GitLab tag pipeline or a GitHub `release` event, and
`branch` otherwise. GitLab's list does not say which pipelines ran on a
tag (only the single pipeline carries `tag`), and its `scope=tags`
answers only the latest tag pipeline of each tag name (GitLab's pipelines
API documentation and its pipelines finder), so `ListPipelines` reads the
tag scope for the ref names it holds. A pipeline in the scope is a tag's;
one on a ref name the scope does not name is a branch's. One on a ref
name the scope names but not in it is an older run of the tag or a run
of a branch of the same name, which GitLab allows, and is classified by
the single pipeline's `tag`. Those reads use the list's admission and
their answers, with the scope's, are remembered per repository by
pipeline id (a pipeline's tag flag never changes). A listing that needs
a pipeline another listing is reading joins that read, and tries once
more when that read fails for other than a 404 (another listing's
admission or cancellation). A
finishing listing prunes the repository's memory to the pipelines it
and every listing that overlapped it in time answered, its own alone
when none did. A read that
answers 404 (the pipeline was deleted) leaves the row out; any other
failure fails the listing. The scope is read without the status filter,
since a tag whose latest run has another status is still a tag. It is
ordered by id, newest first, and a listed pipeline's tag has its latest
pipeline no older than it, so the pages stop once one reaches past the
oldest listed id or comes back short. A list of one ref reads that
ref's tag scope once; a merge request ref reads none. The rows are the
newest `Limit` of the list and the scope's pipelines the list did not
hold that the status filter admits. The list is the newest pipelines,
so a scope pipeline is among them only when it is newer than a listed
one (created between the two reads); a tag whose latest run is older
than the newest `Limit` pipelines has no row. `CIPipeline`
carries the pipeline's `ID`, `Ref` and `SHA`; a GitHub PR's head checks
span many runs, so `ReadPR` leaves its `ID` empty.

### The PR list pump

`SubscribePRList` (`internal/app/app_pr_list.go`) starts or joins the one
list pump of a backend; its handle calls (`SetPRListActive`,
`RefreshPRList`, `UnsubscribePRList`) route home and the caller pins
them to the computer whose subscribe answered, as the PR pump's. The pump
shares the PR pump's core (`app_pump_core.go`): refcounted handles bound
to their connections, paused while every handle is hidden, frames stamped
from the shared PR update sequence. The subscribe answers at once: with
the pump's last frame when one runs, else with the snapshot assembled
from the store (`pr_list_projects`, one row per repository holding its
`ProjectPRList`) for the current forge projects, `Stale` while a
project's stored read is missing or older than the PR update interval.
A new pump starts once that answer is read, so no forge request precedes
it. Its first tick is the user's action and so interactive, as a refresh
is; later ticks run at the PR update interval.

A tick (`app_pr_list_tick.go`) reads the backend's forge projects
(`ListForgeProjects`: the non-archived projects whose origin names a
forge repository, and those whose origin git could not read, with that
failure as their `Error`) and lists each repository once however many
projects clone it, at most `prListReadConcurrency` (4) at a time. Each
repository's read, as it lands, is stored with its `ReadAt` and the
whole snapshot published (`pulls:updated` when it changed), so one slow
project holds no other back. The first tick of a pump seeds it from the
store; frames are `Stale` until that tick's reads landed. A project's
failed list keeps the rows read before beside its error
(`ProjectPRList.Error`, the caller-safe sentence, and `ErrorDetails`, the
correlation id the server log holds the raw error under with that
error's chain, fixed while the same failure repeats), a
first transient failure with rows on screen is held back once, a
rate-limited repository is skipped by background ticks until its
release, a project whose origin cannot be read keeps the repository and
rows the last tick read beside that error, and a removed project takes
its rows along, from the pump and the store. Each row's attention state,
group and reason come from `prAttention` (`app_pr_attention.go`), a pure
function of the row. The last unsubscribe stops the pump, cancels its reads in flight and
drops what it held.

The boot prefetch (`app_pr_list_store.go`) runs once after
`awaitFirstReadsSettled`: one background read, with no pump, of every
forge repository whose stored read is missing or older than the PR
update interval, storing each landed read, pruning repositories no
project names, and holding nothing after. It reads nothing while a list
pump runs. One writer owns the stored copy at a time
(`commitPRListStore`, keyed by `listGen`, which counts the writers
started: each pump and the prefetch, which takes its own generation): the
latest started, and a pump only while it runs. A write or prune of a
writer that lost the copy, or of a pump that stopped, is dropped, so a
read that lands after a newer writer's neither replaces it nor undoes a
prune. The prefetch is what lets a page opened after boot subscribe to
a current stored copy rather than a stale one.

| Tick | GitHub | GitLab |
|---|---|---|
| Per repository | 1 `PullsSearch` per 100 PRs | the project list read above |
| Per host | none | 2 to 6 viewer events pages (comments and approvals) |

### The pipeline pump and stage reads

`SubscribePipeline(repo, id)` (`app_pipeline_pump.go`) follows one
pipeline of a repository, one pump per (repository, id) however many
panes show it. It is the PR pump's CI phase without a PR
(`app_ci_phase.go`): the first subscriber reads the pipeline, a joiner
takes the pump's state, the pipeline is polled at the live cadence while
it or a job of it is queued or running (`git.CIPipelineLive`: a queued
pipeline may list no job yet) and its followed job logs (`SetPipelineFollows`) at the follow
cadence, and frames go out on `ci:pipeline_updated`; a log frame rides
`pr:ci_log` with the pipeline's key and no PR key. A terminal pipeline
is read again at the PR update interval while visible, since a job
retried on the forge restarts it with nothing else to say so. Job logs
are a repository's (`ciLogKey(repo, job)`), whichever PR or pipeline
showed them, so `GetPRCILogLines`, `SavePRCIJobLog` and
`SavePRCIJobLogSection` take a `RepoReference`.

`ListProjectPipelines(projectID, filter, interactive)` is one
`ListPipelines`; the Pipelines tab reads each repository once, through the
first picked project that clones it, so a repository two projects clone
lists each pipeline once. Both calls carry `interactive`: the Pipelines tab
passes true for its open, its refresh and a filter change, and for the stage
read of the first rows on screen after one, and false for its interval
read and for stages read on scroll, so the page's own polling never
spends the rate limit reserve.
`ReadPipelineStages(repo, ids, interactive)` reads at most `maxPipelineStageReads` (30)
pipelines, 4 at a time, with `ReadPipeline` and answers each stage's name
and status, or that pipeline's failure. An unchanged pipeline costs what
`ReadPipeline` does (the table in "Project lists"): on GitHub the run and
its jobs list are GETs the ETag store revalidates, a 304 each (the jobs
list measured free of the limit); on GitLab `PipelineJobs` is GraphQL, which
has no ETag (the REST jobs list it replaced answered 200 to
`If-None-Match` too), and the single pipeline's revalidation is not
measured, so 30 unchanged rows cost 30 pipeline reads and 30
`PipelineJobs` bodies. GitHub's permission and artifacts reads are the
remembered answers `ReadPipeline` keeps.

### CI actions

`RunPRCIAction(subscriptionID, req)` and `RunPipelineAction(subscriptionID,
req)` (`app_ci_actions.go`) act on the pipeline a PR pump's or a pipeline
pump's CI phase holds, through one implementation. The request is checked
against that pipeline's capability flags (`CIJob.Actions`,
`CIPipeline.Actions`: empty when the action does not apply, `denied` when
the viewer may not take it) before any forge request. An action reserves,
in one step under the pumps' lock, the identities its writes reach: a
job action its GitHub run or GitLab pipeline, a pipeline action every run
or the pipeline it shows. A second action reaching any reserved identity
is refused, so a PR's head checks and a Pipelines-tab view of one of
their runs cannot fire the same run twice. `Core.RunCIAction` runs on
`context.WithoutCancel` and interactive, and posts each write once. Each
write is confirmed, refused (`forgeapi.WriteNotApplied`) or unconfirmed
(it may have been applied: a reset, a timeout or a 5xx after sending);
anything short of every write confirmed or every write refused is one
`CIPartialError` counting each. When any write was confirmed or
unconfirmed, the caller's pump polls at once and the answer is its
sequence after that poll (`CIActionResult.Seq`, as `PRActionResult.Seq`);
every other pump showing a touched pipeline or run polls without holding
up the answer, and each polls at the live cadence for a minute after,
since GitHub queues a re-run asynchronously.

| Action | GitHub | GitLab |
|---|---|---|
| Retry a job | `POST repos/O/R/actions/jobs/ID/rerun`, within 30 days of the run's creation | `POST projects/P/jobs/ID/retry` |
| Retry the failed jobs | `POST repos/O/R/actions/runs/ID/rerun-failed-jobs` per completed run with a failed or canceled job | `POST projects/P/pipelines/ID/retry` |
| Cancel a job | not offered | `POST projects/P/jobs/ID/cancel` |
| Cancel the pipeline | `POST repos/O/R/actions/runs/ID/cancel` per live run | `POST projects/P/pipelines/ID/cancel` |
| Run a manual job | not offered | `POST projects/P/jobs/ID/play` |
| Download artifacts | `GET repos/O/R/actions/runs/ID/artifacts` pages, then `GET repos/O/R/actions/artifacts/ID/zip` per unexpired artifact | `GET projects/P/jobs/ID/artifacts` |

`ExplainCIError` words a `CIPartialError` with its counts and says an
unconfirmed write may have been applied. GitHub re-runs a run only within
30 days of its creation: `ReadPipeline`'s run carries `created_at`, so a
run past it offers no retry; a PR's rollup does not, so no request is
added and GitHub's refusal ("created over a month ago") reads as the run
being too old. A retried job is a new job under a new id on both forges,
and the old one drops out of the read; the log view follows the new
attempt with `git.FindCIJobRetry` (and `findCIJobRetry` in
`ciSubject.svelte.ts`, the same rule) only when the old job is gone from
the read, the read is the old job's pipeline (the same GitLab pipeline id,
or the same GitHub workflow run), and exactly one job of the same stage
and name has a higher id.

`SaveCIArtifacts(repo, req, subject)` (`app_ci_artifacts.go`) streams each
artifact with `Stream` into a temp file under the app data directory's
`ci-artifacts`, in the repository's directory
(`<forge>/<host>/<namespace...>/<repo>`, each segment escaped one to one,
so the path is the repository's whole identity; saved logs use the same
directory under `ci-logs`), renamed into place once whole, as a saved CI
log is: at
most 100 artifacts per save, each at most 1 GiB (a GitHub artifact the
list sizes past it is refused before its request) within a 10-minute
timeout, and the answer names the directory and files. Downloads are not
pipeline actions and do not take the pending slot.

## Managing a PR

The review pane's management actions (`ForgeManager`,
`internal/git/forge_manage.go`; bound in `internal/app/app_pr_manage.go`)
follow the merge actions' contract: each write is interactive, validated
in `git.Core`, checked inside the adapter against the PR it names, and on
success the PR's pump polls at once (`PRActionResult.Seq`). The app runs
the management writes of one PR one at a time (`prManageLocks`, keyed by
the PR's identity, `PRReference.Identity`: owner and repository
lowercased, as both forges resolve them; waiting bounded by the call's
context, which is checked before and after the wait; an entry dropped
once no call holds or waits on it); merge actions are outside it. A
review submit's lock keys on the same identity. The two reads
outside the tick, a picker's search (`SearchPRCandidates`) and the default
merge commit message (`GetPRMergeMessage`), are their own calls; their
failure never touches the PR's read.

A write has three outcomes. Applied; refused, which changed nothing; or
unconfirmed (`git.UnconfirmedWriteError`, code `forge_unconfirmed`), which
may have applied, is not sent again, refreshes the pump all the same, and is
explained as "the pane shows what the forge did" (a comment or reply: "check
the thread before sending it again"), never with a retry. A GraphQL error answer is a
refusal (`forgeapi.WriteNotApplied`) only when the answer had no data or
the mutation's field was null, its data decoded, and every error is a
known refusal: GitHub `FORBIDDEN`, `NOT_FOUND`, `UNPROCESSABLE` or
`INSUFFICIENT_SCOPES`, a validation error before execution (no `data`), or
GitLab's permission answer. A rate-limited GraphQL answer is a refusal
only while its operation produced no data: data beside the rate limit
means the operation ran. A people or label change removes first, then
adds, one write per non-empty side; a failure of the second write is a
`git.PartialWriteError` that names what the first did and whether the
second was refused ("removed X; adding Y was refused: ...") or may have
applied ("removed X; adding Y may have applied").

The read rides the detail (`PRDetail.Manage`): reviewers with their state
(`APPROVED`, `CHANGES_REQUESTED`, `COMMENTED`, `PENDING`), assignees,
labels and one capability flag per control (`PRManageCan`), from the
forge's own answer. A GitHub reviewer's state and staleness come from the
same review: the standing approval or change request when one stands,
else the latest review. A failed management read never fails or hides
the PR: `PRManage.Unavailable` says so in one sentence, every flag is
false and the lists are empty. On GitLab that is the `PRManage` read or
its probe; on GitHub, a PRTick answer whose every error lies under the
management selections (the viewer flags, `reviewRequests`, `assignees`,
`labels`) with the rest decoded; an error anywhere else fails the read. The changes-requested blocker carries
the reviews a dismissal needs (`MergeBlocker.ChangeRequests`) and whether
a GitLab bypass is offered (`Bypassable`); a GitLab bypass that stands is
a settled row (`Bypassed`).

People are addressed by login on both forges (`PRPerson.ID` is GitLab's
user id, needed only to re-request a review); labels by their global id
on both (`PRLabel.ID`: GitHub's node id, GitLab's
`gid://gitlab/ProjectLabel/N` or `GroupLabel/N`).

| Action | GitHub requests | GitLab requests |
|---|---|---|
| Edit title or description | `PREditTarget(owner, name, number)` (node id, body), then `EditPR` (`updatePullRequest`, only the fields given) | 1 merge request read for a description edit, then `PUT projects/P/merge_requests/N` with `title`, `description` |
| Reviewers | REST `DELETE` then `POST repos/O/R/pulls/N/requested_reviewers` (`reviewers`, `team_reviewers` by slug; a team must be the repository owner's) | `PRSetReviewers` (`mergeRequestSetReviewers`, by username) with `operationMode` `REMOVE`, then `APPEND`, each checked against the reviewers it answers |
| Assignees | `PRAssignees` (the PR's assignees, to refuse a change leaving more than 10), then REST `DELETE` then `POST repos/O/R/issues/N/assignees` | `PRSetAssignees` (`mergeRequestSetAssignees`), as reviewers |
| Labels | `PRLabelTarget(owner, name, number, ids)` (the PR's node id, each added id's repository), then `RemoveLabels` then `AddLabels` | `PRSetLabels` (`mergeRequestSetLabels`, by label id), `REMOVE` then `APPEND`, checked as reviewers |
| Close, reopen | `PRMergeTarget` + `ClosePR` / `ReopenPR` | `PUT` with `state_event` |
| Convert to draft, mark ready | `PRMergeTarget` + `ConvertPRToDraft` / `MarkPRReady` | `PRSetDraft` (`mergeRequestSetDraft`), checked against the `draft` it answers |
| Re-request a review | as adding the reviewer | `PRRereview` (`mergeRequestReviewerRereview`) |
| Dismiss a change request (GitHub) | `ViewerApproval` (the review must be one of the PR's standing change requests), then `DismissReview` | not offered |
| Bypass requested changes (GitLab) | not offered | `PRBypassChanges` (`mergeRequestUpdate(overrideRequestedChanges: true)`) |
| Picker search, 20 per page with `More` | `SearchAssignable` (`assignableUsers(query)` and the owner's type), and for an organization's reviewers `SearchTeams` (`organization(login).teams(query)`), whose failure of any kind keeps the users; or `SearchLabels` | `projects/P/users?search=` (`X-Next-Page` is `More`) or `SearchProjectLabels` (`project.labels(searchTerm, includeAncestorGroups: true)`) |
| Default merge message | `PRMergeMessage` (`viewerMergeHeadlineText`, `viewerMergeBodyText` for the method) | `PRMergeMessage` (`defaultMergeCommitMessage`, `defaultSquashCommitMessage`), split at the first line |
| Merge with an edited message | `MergePRMessage` / `EnableAutoMergeMessage` (`commitHeadline`, `commitBody`) | `merge_commit_message` or `squash_commit_message` on the merge `PUT`, auto-merge included |

Neither forge takes a precondition on the description, so a description
edit names the description it replaces (`PREdit.ExpectedDescription`) and
the adapter compares it with a fresh read (CRLF and trailing newlines
ignored) before writing; a mismatch is `git.ErrDescriptionChanged`. A
removal is sent as asked, even of someone or something the last read did
not show. GitHub ignores an assignee it cannot assign without an error, so
the next read is the answer. A GitLab set mutation skips a non-member or
an unknown label id without an error; the adapter checks the list the
mutation answers (`first: 100` with `pageInfo { hasNextPage }`). A miss
from a whole list is a refusal naming who or what was not applied; one
the list cannot show (it has a next page) may have applied; some applied
and some not is a partial change naming both, also as the first write.

GitHub's capability flags come from its viewer fields: edit and convert to
draft from `viewerCanUpdate`, close from `viewerCanClose`, reopen from
`viewerCanReopen`, labels from `viewerCanLabel`, assignees from
`viewerCanAssign`. Requesting and re-requesting reviews follow triage
access or the viewer's authorship of the PR (`viewerDidAuthor`; GitHub's
review requests reference: "Pull request authors and repository owners
and collaborators can request a pull request review"), and dismissing
write access, which have no viewer field; team
reviewers need an organization owner. GHES before 3.10 is outside
support. GitLab's: edit, close, reopen, draft and re-request from
`userPermissions.updateMergeRequest`; reviewers, assignees and labels from
`adminMergeRequest` and the host's set mutation; bypass from the REST
`user.can_merge` and requested changes blocking the merge.

GitLab's schema decides which controls exist and which document is sent.
`PRManageSchema` asks for `MergeRequestReviewerRereviewInput` (gates
re-request), `MergeRequestUpdateInput`'s `overrideRequestedChanges`
(gates bypass) and `operationMode` on `MergeRequestSetReviewersInput`,
`MergeRequestSetAssigneesInput` and `MergeRequestSetLabelsInput` (each
gates its control; a host without it offers none and refuses the write).
The answer, either way, is cached per host for 10 minutes; a failed probe
is not cached and makes the read unavailable. Concurrent cold reads of a
host share one probe, which runs past the caller that started it, each
waiting bounded by its own context. A harness reset forgets the answers
with the adapters' other per-host and per-project memory
(`Core.ResetForgeCaches`). A host with the override is
read with `PRManageChecks`, which adds `mergeabilityChecks`: the
`REQUESTED_CHANGES` check failing is what blocks (elsewhere the
`detailed_merge_status` `requested_changes`), and the check passing while
a reviewer still requests changes is shown as bypassed. That is an
inference: GitLab reports no bypass as such.

The GitLab GraphQL arguments these use: `project.labels` takes
`searchTerm`, `includeAncestorGroups`, `first` and `after`;
`MergeRequestSetReviewersInput` and `MergeRequestSetAssigneesInput` take
usernames with `operationMode` (`REPLACE`, `APPEND`, `REMOVE`);
`MergeRequestSetLabelsInput` takes `labelIds` with `operationMode`;
`MergeRequestSetDraftInput` takes `draft`;
`MergeRequestReviewerRereviewInput` takes `userId`; and
`mergeabilityChecks` reports the `REQUESTED_CHANGES` identifier.

## Suggestions and reactions

A comment's suggestions and reactions ride the threads read; applying a
suggestion and changing a reaction are writes bound in
`internal/app/app_pr_suggestions.go` (`ApplyPRSuggestion`,
`SetPRCommentReaction`) that run as management writes do: under the PR's
write lock, interactive, with the three outcomes and the pump's poll
(`PRActionResult.Seq`). The adapters are `internal/git/github_suggestions.go`,
`github_reactions.go`, `gitlab_suggestions.go` and `gitlab_reactions.go`.

A suggestion is a fenced code block whose info string's first word is
`suggestion` (`internal/mdsuggest`, read with goldmark's CommonMark
block parse): only one at the top level of the body counts, so a fence
in a blockquote, a list item or an HTML block (a `<!-- -->` comment, a
`<details>` block before its blank line) is not one, and neither is a
fence the body never closes. Each block carries the 0-based lines, in
the body split on "\n", of its opening and closing fences (`StartLine`,
`EndLine`), and each `CommentSuggestion` its block's
(`BodyStartLine`, `BodyEndLine`, set when `BodyPlaced`): always on
GitHub; on GitLab, whose suggestions come from the API, only when the
note's body holds as many blocks as GitLab reports suggestions, block i
being suggestion i. Positions are not part of the digest. GitLab's `suggestion:-N+M` widens the replaced lines (N
and M at most 100); GitHub has no such header, so a widened block reads
as no suggestion there. `ParseCommentSuggestions` is the same parser for
an editor's preview, for a body of at most 1,000,000 characters
(GitLab's note limit, the larger forge's).

`ReviewComment.Suggestions` is one `CommentSuggestion` per block in body
order, each with a `Digest`: the SHA-256 of the forge, the replaced
lines and the replacement lines, length-prefixed (`SuggestionDigest`).
An apply presents it (`SuggestionApply.Digest`), and a suggestion that
no longer has it, or whose index or GitLab id is gone, is
`*SuggestionChangedError` (code `forge_suggestion_changed`, "The
suggestion changed since it was loaded. Reload the pull request."),
refused before anything is read or written: the GitHub apply recomputes
it from the comment's current body on the thread's current anchor; the
GitLab apply re-reads the note's suggestions from its discussion. GitLab's
apply takes no expected content, so an edit between that read and the
apply is not caught by the app; GitLab refuses an apply whose lines
changed. GitLab's come from the note's own `suggestions` in the discussions
read (`from_content`, `to_content`, `appliable`, `applied`), so `From` is
known; `Appliable` is GitLab's answer and the viewer at developer or
above, as GitLab's suggestions API requires. GitHub's are parsed from the
body and anchored on the thread's new-side lines (`StartLine`,
`EndLine`); the replaced text is not read, since `diffHunk` on every
thread would grow the one `PRTick` answer past its bound, so `From` is
empty and `FromKnown` false. A GitHub suggestion is appliable when the
pull is open, `viewerCanApplySuggestion`, and its thread is on the new
side, not outdated and not resolved.

| Action | GitHub requests | GitLab requests |
|---|---|---|
| Apply a suggestion | `SuggestionTarget(threadID, commentID)` (the viewer, the thread with its anchor and pull, its comment ids, the comment's body and author), `ThreadCommentsPage` past 100 comments, `SuggestionFile(owner, name, oid, path)` (the head commit's `file(path:)` tree entry: `mode`, `type`, `oid`, the blob's `byteSize` and `isBinary`), REST `GET repos/HEAD_REPO/git/blobs/OID` with `Accept: application/vnd.github.raw+json` (at most 1 MiB), `SuggestionCommit` (`createCommitOnBranch` with `expectedHeadOid` and one file addition), then `SetThreadResolved` | 1 merge request read, `GET .../discussions/D` (the suggestion must be one of its notes'), then `PUT suggestions/:id/apply` |
| Add or remove a reaction | `ReactionTarget(id)` (the node's type, pull and `reactionGroups`), then `AddReaction` or `RemoveReaction` | `user` (cached), `GET .../notes/N/award_emoji?per_page=100` (paged), then `POST .../notes/N/award_emoji` with `{name}` or `DELETE .../award_emoji/ID` |

Every write checks inside the adapter that the comment is on the PR it
names; one that is not is `*CommentNotOnPRError` (kind `not_found`)
before anything is written. An apply names the head the pane loaded: a
head that moved is `*HeadMovedError` (kind `stale_head`). The GitHub
apply refuses, before the commit, a pull that is not open, an outdated,
resolved or old-side thread, and from the tree entry, before any
content is read: no entry at the path, a symbolic link, a submodule, a
directory, an executable file (`createCommitOnBranch` takes no file
mode, and GitHub's reference does not say an addition keeps an existing
one), any other mode than a regular file's, a blob past 1 MiB by
`byteSize`, a blob GitHub calls binary, and an object id that is not
SHA-1; then blob bytes whose `sha1("blob <len>\0" + bytes)` is not the
entry's id, a file that is not UTF-8 or holds NUL, and lines past the
file's end. Each is `*SuggestionNotAppliedError` (kind `refused`, "The
suggestion was not applied: <reason>."). A Git LFS pointer is a regular
blob and is rewritten as one. The apply is a byte splice: the bytes
before the first replaced line and after the last one's line ending are
the file's; the replacement lines are joined and ended with the last
replaced line's ending, or, when it has none, joined with the nearest
preceding line ending (LF when there is none) and left without one; a
UTF-8 byte order mark stays in front. The commit has the headline `Apply suggestion from
code review` and a `Co-authored-by: Name <ID+login@users.noreply.github.com>`
trailer when the suggestion's author is a user with a name and id other
than the viewer. A fork head is attempted; GitHub's refusal is explained
as any refusal. A commit whose resolve then fails is a
`*PartialWriteError` ("committed the suggestion to the head branch;
resolving its thread ..."). GitLab answers an apply with the suggestion;
one that does not read applied is unconfirmed.

A reaction is named in GitLab's award vocabulary
(`CommentReaction.Name`): GitHub's eight contents map to `thumbsup`,
`thumbsdown`, `laughing`, `tada`, `confused`, `heart`, `rocket` and
`eyes`, and a GitLab note may carry any award name. A reaction already
in the asked state answers success with no write; GitLab's "has already
been taken" on an add and its 404 on a delete count as that. GitHub's
groups ride every comment of the threads read (`reactionGroups { content
viewerHasReacted reactors { totalCount } }` under `@include(if:
$wantReactions)`), with no request of their own and no names. GitHub
answers all eight groups of every comment, so a threads read that passes
its body bound with them runs once more, whole, without them, and says
"Reactions are not shown: this pull request has too many comments to
read them."; a read too large without them fails as any read does.
Nothing remembers that outcome, so each tick may cost the failed
request again. GitLab's discussions read has no awards, so the read
adds `MRNoteReactions(fullPath, iid, after)`: the merge request's notes,
100 per page with each note's first 100 `awardEmoji` (name and
username), at most 10 pages. It is sent only when the note ids changed,
after the viewer's own reaction write, or when the last answer is 2
minutes old; concurrent reads of one merge request share it. The
viewer's reaction write and `Core.ResetForgeCaches` each start a new
generation of the memory: a read in flight across one is neither joined
nor kept, nor is its failure or its finding that the server lacks the
field. Its failure
never fails the threads: the comments carry no reactions,
`PRRead.ReactionsUnavailable` (on the subscription result and every
frame as `reactionsUnavailable`) says "Reactions are not shown: GitLab's
reactions could not be read.", the failure is logged with its retry
time, and it is retried after 20 seconds, doubling to 15 minutes. A
server whose schema lacks `Note.awardEmoji` says "Reactions are not
shown: this GitLab server does not report them." and is not asked again
for 10 minutes. Past the read's bounds the sentence is "Reactions are
shown on the first 1000 comments only." or "Reactions are not shown on
comments with more than 100."

Every comment carries its forge link in `ReviewComment.URL`: GitHub's
comment `url`, GitLab's merge request URL with `#note_<id>`.

## Merged PR worktree cleanup

`internal/app/app_worktree_merge_cleanup.go` removes a linked worktree
when the PR its threads track merges; the product rules are in
[decisions](../decisions.md). Every fresh `lookupOpenPR` answer (not a
cache hit) reaches the app through `Core.SetOpenPRObserver`. An open PR
seen for a thread's linked worktree is recorded in `thread_pull_requests`
(one row per thread, with the GitHub node id). A lookup that answers "no
open PR" for a branch whose tracked PR is still unmerged reads that PR's
state once (a failed read leaves it to the poll); the memo of reads made
holds only threads whose tracked PR is still pending and in its worktree.

States are read in one request per repository, batched by
`Forge.ReadPRStates`: GitHub `PRStates` (`nodes(ids:)` over the tracked
node ids, `state` and `mergedAt`), GitLab
`projects/P/merge_requests?iids[]=N&state=all`. Up to 100 PRs per request.
A PR the forge no longer has counts as closed.

| Trigger | Requests |
|---|---|
| A repository with no unmerged tracked PR whose thread is in its worktree | none |
| Poll, per repository with one, while background fetch is on (every 5 minutes, first after 45s from boot or the first tick after it is turned on) | 1 state read |
| A failed poll | the next after 10, 20, 30 minutes (doubling from the poll interval, capped at 30) |
| A watched lookup that stops finding the tracked PR | 1 state read |
| Acting on a merged PR, after the local checks pass | 1 open-PR lookup of the branch (one page unless forks' PRs share the name) |
| An act-time lookup that fails, or still lists the merged PR as open | the next after 2, 4, 8 ... minutes, capped at 30, per repository |
| A removal git refuses | no request; the removal retries after 2, 4, 8 ... minutes, capped at 30 |

A `*forgeapi.RateLimitedError` waits until its `Until` instead. Two
goroutines run the cleanup, serialized per pass. The worker handles
lookup answers and turn ends, always. The poll, which reads states and
retries waiting PRs on its tick, runs only while
`Settings.BackgroundGitFetch` is on: `UpdateSettings` starts it, or stops
and joins it, with no restart (`reconcileMergedPRPoll`). With the setting
off the cleanup sends no request of its own; a merged PR waiting for a
retry is tried again at the next act pass a turn end or a merge found by
a lookup starts, and the worker's start arms that turn-end pass when a
previous run left one (`armMergedPRWaiting`, a store read). Both wait for the first client's reads like other unattended
work. An act pass that finds a
thread busy marks itself waiting and runs again when a turn completes;
each pass first skips what is backing off, so a busy or failing worktree
costs no forge request per pass. `AO_HARNESS_TIMING` `merged-pr-poll`
shortens the poll, and with it the one-minute tick these retries start
from, for specs.

## Requests

```go
type Request struct {
    Method      string
    Path        string        // relative to the REST base, or absolute for a declared attachment fetch
    Query       url.Values
    Header      http.Header   // Range, If-None-Match, Accept; any header keeps the call off the ETag store
    Body        any           // JSON-encoded; io.Reader passes through
    Timeout     time.Duration // per call; zero takes Options.ReadTimeout (45s); covers headers and body
    Attachment  bool          // allows an absolute Path
    MaxBodyBytes int64        // Do, JSON and Pages body bound; zero takes Options.MaxBodyBytes (1 MiB), at most 8 MiB
}
// Interactive comes from the context (WithInteractive), not the Request.

type Response struct {
    Status    int
    Header    http.Header
    Body      []byte          // bounded by Request.MaxBodyBytes, else Options.MaxBodyBytes (1 MiB)
    NotModified bool          // 304 answered from the ETag store
    Rate      *RateLimit      // parsed from the pool headers when present
}

func (c *Client) Do(ctx context.Context, r Request) (*Response, error)
func (c *Client) JSON(ctx context.Context, r Request, out any) (*Response, error)
func (c *Client) GraphQL(ctx context.Context, operation, query string, vars map[string]any, out any) (*Response, error)
func (c *Client) Stream(ctx context.Context, r Request, dst io.Writer, limit int64) (*Response, error)
func (c *Client) Pages(ctx context.Context, r Request, visit func(*Response) (more bool, err error)) error
func (c *Client) CredentialFingerprint(ctx context.Context) (string, error) // see Credentials
```

- `GraphQL` sends variables as JSON `variables`, never interpolated into
  the query, and the operation as `operationName`; a call without one is
  refused. Errors in the GraphQL envelope become `*GraphQLError`; data
  beside errors is handed back so a caller can decide what partial data it
  accepts.
- A body past its bound fails the call with `ErrBodyTooLarge`. A list
  read whose measured page can pass 1 MiB names its own bound
  (`internal/git/forge_body_limits.go`, sized in
  [list page sizes](../references/forge-api-measurements.md#list-page-sizes));
  a bound below zero or past 8 MiB, a quarter of the ETag store, is
  refused before anything is sent, so one stored body cannot evict the
  rest. `Stream` refuses one: its `limit` argument is its bound.
- `Stream` writes the body to `dst` and fails with `ErrBodyTooLarge` once
  `limit` bytes pass; `NoLimit` streams any size, for a `dst` on disk. A
  `dst` that implements `StreamStarter` is told the status and headers
  before the body, so the CI log store can tell a 206 resume from a 200
  restart. A JSON body is re-encoded for
  the retry after a 401 refresh; a one-shot `io.Reader` body refreshes the
  token and fails with `*TransientError` instead.
- `Pages` follows GitHub `Link: rel="next"` and GitLab `X-Next-Page` /
  `Link`; the caller's `visit` returning false is the page cap.
- An absolute `Path` is accepted only when `Request.Attachment` is set; the
  attachment fetchers set it, nothing else does. An isolated boot rewrites an
  absolute URL onto the fake's base and records the original absolute URL,
  with any signed query removed, as the invocation `path`.
- Every request carries `User-Agent: agent-overflow/<version>` (GitHub
  answers 403 without one). GitHub requests add
  `Accept: application/vnd.github+json` and `X-GitHub-Api-Version`;
  GitLab requests add `Accept: application/json`.
- Transport: one `http.Transport` per `Service` with
  `ProxyFromEnvironment`, system roots, `ForceAttemptHTTP2`,
  `MaxConnsPerHost: 8`, `MaxIdleConnsPerHost: 8`, `IdleConnTimeout: 90s`,
  `ResponseHeaderTimeout: 30s`, dial timeout 10s. No `http.Client.Timeout`;
  deadlines come from `ctx` and `Request.Timeout`. A per-host semaphore of 8
  (`hostConcurrency`: a cold GitLab project read is its list, six GraphQL
  reads and two events feeds, so the ninth waits for the first to finish)
  bounds concurrency so a burst of panes cannot open more sockets.

## Errors

A response is classified in this order: rate limited, 401, 404, GraphQL
errors, success. Rate limited means 429; 403 with `X-RateLimit-Remaining:
0`, a `Retry-After` header or a body matching `rate limit`; or a 200
GraphQL envelope with a `RATE_LIMITED` error or errors beside
`X-RateLimit-Remaining: 0`. Any other 403 (SSO enforcement, a missing
scope or permission) is a `*ForbiddenError`, never "unauthenticated".

| Observation | Error | Caller behavior |
|---|---|---|
| CLI missing when the token is read | `*SetupError{Kind: "missing"}` | Setup banner, as today |
| CLI present, no token for the host | `*SetupError{Kind: "unauthenticated", Host}` | Setup banner |
| 401 after one token refresh, on a read, or on a mutation whose probe of the login's own `user` is answered 401 (one GET, sent after the mutation released its host slot; GitLab answers a merge the login may not perform with 401) | `*SetupError{Kind: "unauthenticated", Host, ExpiredAt}`: `ExpiredAt` when the token's remembered expiry (see Credentials) has passed; a remembered revocation leaves it zero and says "revoked" in `Message` | Setup banner; `git.ExplainForgeError` names the expiry date |
| 401 after one token refresh on a mutation while the login still reads `user` | `*StatusError` (the forge refused the action) | `git.ExplainForgeError`: "FORGE did not let you ACTION NAME." |
| 401 after one token refresh on a mutation whose `user` probe fails otherwise (rate limited, 5xx, unreachable, timeout, cancelled) | `*RefusalError{Refusal, Step: "probe", Err}`: the mutation's 401 and the probe's own error, which `errors.As` finds first | Retry; never "log in again"; `git.ExplainForgeError`: the action was refused, nothing changed, try again |
| 401 on a mutation whose token re-read then fails | `*RefusalError{Refusal, Step: "credential", Err}`: the 401 and the re-read's error, found first; on a read, the re-read's error alone | `git.ExplainForgeError`: a setup error's sentence, else the token was refused and the re-read failed, check the login |
| A mutation's 4xx whose body is cut short | the status's error, with the body that arrived; on a read, `*TransientError` | As the status |

A complete refusal is a fact about the write that a later step cannot
erase: `WriteNotApplied` reports a write the forge refused in full, or
never received, as not applied, whatever failed after it.
| 403 without rate-limit headers | `*ForbiddenError{Status, Scope, Message}`: `Scope` only with evidence the token lacks it, GitLab's `insufficient_scope` body or GitHub's `X-Accepted-OAuth-Scopes` beside an `X-OAuth-Scopes` granting none of them (the first accepted one); a fine-grained token's "Resource not accessible by personal access token" is a permission, no `Scope`; `Message` the body's | `git.ExplainForgeError` names the scope; else an action shows the forge's message and a read the project |
| Rate limited (rules above), or refused for the reserve | `*RateLimitedError{Host, Pool, Until, Reserve}` | Pollers wait for `Until`; see below |
| 404 on a PR-, job- or thread-scoped path | `*StatusError` matching `errors.Is(err, ErrNotFound)` | `ErrCIJobLogNotFound`, PR gone, log not published yet |
| 404 on a repository- or project-scoped path | `*StatusError` | Reaches the user: wrong project or no access, never an empty list |
| 304 | no error, `Response.NotModified` | Caller keeps its state |
| Dial, DNS, TLS, timeout | `*TransientError` wrapping the cause | Retry on the next tick |
| GraphQL `errors` | `*GraphQLError{Problems, Data}` | Caller decides on partial data |
| GraphQL `errors` with a `NOT_FOUND` problem | `*GraphQLError{Problems, Data}` | `git.ExplainForgeError`: kind `not_found`, the REST 404 sentence |

`StatusError.Error()` carries the method, the redacted URL, the status and a
bounded body (4 KiB). No forge error is classified from CLI stderr.

A bound method that acts on a PR (`SubmitPRReview`, `SetPRThreadResolved`,
the merge and comment actions) returns its forge failure
through `git.ExplainForgeError`: one sentence naming the cause and the
fix, carried as reviewed prose under the code `forge_<kind>` (`transient`,
`rate_limited`, `setup`, `forbidden`, `not_found`, `stale_head`, `refused`,
`forge`, and `unconfirmed` for any write that may have applied, after
which the PR's pump polls) with the forge's own error in the detail chain. The client shows
the sentence and keys its one action on the code (`forge_stale_head`
offers Reload).

## Rate limits

One gate per `(host, pool, fingerprint)`, owned by the `Service` and shared
by every caller; there is no second pause layer in the app. GitHub names
the pool in `X-RateLimit-Resource` (`core`, `graphql`, `search`); the pools
are separate 5000/hour budgets, so a REST exhaustion must not stall
GraphQL. GitLab names it in `RateLimit-Name`.

- Quota is the forge's own count from the latest response headers,
  including 304s; nothing is debited locally. Among responses the latest
  `reset` wins, then the latest response.
- A rate-limited response closes the gate until `Retry-After` (seconds or
  HTTP date), else `X-RateLimit-Reset` / `RateLimit-Reset` when in the
  future, else a fallback of 60s doubling per consecutive closure to 15
  minutes. A new closure never shortens an open one.
- Lease guard: `check()` hands the caller the gate's generation; a
  rate-limited response from a request that started before the current
  closure extends `Until` but does not count as another closure, so a burst
  of in-flight 429s doubles once. A success with a current lease after
  `Until` resets the doubling.
- The pool is chosen from the request (`core`, `graphql`,
  `throttle_authenticated_api`) and the response header's pool name
  receives that response's quota snapshot.
- Background requests (every poller) fail fast with `*RateLimitedError`
  while the gate is closed or while the pool's remaining is under 10% of
  its limit (`Until` = reset; `Reserve` = true on the error). Interactive
  requests may spend the reserve down to one but honor a closed gate: a
  secondary limit is a secondary limit for a click too.
- Release is staggered: a poller resuming after `Until` adds a random
  0 to 2s so every pane does not fire together.
- Gates and quota snapshots are bounded to the hosts with a live client
  and dropped with the client's token on `Close`.

## Failure presentation

The PR pump keeps its last snapshot through every failure and dedups
identical failures, except that a rate-limited failure dedups on
`(kind, reserve, resumeAt)` rather than on message text, so a frame goes
out when the resume time moves and not on every tick. `pr:updated`,
`pr:ci_updated`, `pr:ci_log` and the `SubscribePRUpdates` result (for CI,
`CIErrorKind`, `CIReserve`, `CIResumeAt`) carry `ErrorKind` (`transient`,
`rate_limited`, `setup`, `forbidden`, `forge`), `Reserve` and `ResumeAt` (RFC 3339, set
for `rate_limited` only) beside the caller-safe `Error`, so the pane
chooses the surface instead of parsing text. `classifyForgeFailure`
(`internal/app/app_forge_failure.go`) is the one classification. A bound
method's rate-limited failure (`forge_rate_limited`) carries the same
`reserve` and `resumeAt` on its error frame
([RPC failures](transport.md#rpc-failures)).

- `transient` while the snapshot poll has a snapshot on screen and shows
  no failure: nothing is sent until the retry (the retry base) also fails,
  whatever its kind; a success in between sends nothing. A pump whose
  first fetch failed has nothing to show and reports at once. Every other
  kind, and every CI and log failure, is sent on the first failure.
- `rate_limited`: the snapshot poll, the CI poll and log follows send
  nothing to the forge before `ResumeAt` plus the stagger; a person's
  request (a refresh, a new follow) still runs and meets the gate's own
  rule. The pane shows a warning banner (`review-pr-rate-limited`), not an
  error, with no "Retrying" prefix. Exhausted: "GitHub rate limit reached.
  Updates resume at HH:MM." Reserve: "GitHub rate limit nearly used up.
  Updates pause until HH:MM so your own actions still go through." The
  forge name comes from the PR reference and HH:MM is `ResumeAt` in local
  time. It clears on the first successful poll. The CI chips read "Checks
  paused" and an open log shows the same sentence in the warning tone.
- `setup`: `Error` is the `SetupError` message, which names the login to
  fix and carries no forge text; the pane shows it in its own banner
  (`review-pr-setup`) with no "Retrying" prefix.
- `forbidden`: `Error` is the `git.ExplainForgeError` sentence naming the
  missing scope or the project the login cannot see, no forge text; the
  pane shows it in the setup banner.
- `transient` and `forge`: "Retrying: failed to refresh pull request
  (id: ...)", the correlation id that finds the raw error in the server
  log.

## Conditional requests and ranges

- ETag store per `Service`, keyed `(host, fingerprint, method, URL)`,
  holding the ETag and the body it validated, bounded by entry count (256)
  and bytes (32 MiB) with LRU eviction; a host's entries go when its token
  changes. A GET whose key is held
  sends `If-None-Match`; a 304 returns the stored body with `NotModified`,
  or `ErrBodyTooLarge` when the body is past this request's bound. The key
  holds no request header, so a request with a header of its own
  (`Accept`, `Range`, its own `If-None-Match`) does not use the store: the
  client adds no `If-None-Match` and stores no answer, and a 304 to the
  caller's `If-None-Match` is `NotModified` with no body.
  GitHub REST 304s are free of the limit; GraphQL (POST) has no ETag, so
  `PullsSearch` is read whole each refresh. GitLab honors `If-None-Match`
  on merge requests, their list and job traces, not on the pipeline jobs
  list (200 with the same weak ETag), which the app no longer reads.
- `Stream` never stores a body; it passes the caller's `If-None-Match`
  through and exposes the response ETag, and follows a redirect with the
  caller's `Range` and `If-None-Match` (a GitHub job log's blob). The CI
  log store (`internal/cilog`) keeps the ETag of the version it holds and
  sends it on its next fetch; a 304 keeps the version and sends no frame.
- A CI log is downloaded whole to disk, of any size, each attempt bounded
  by `ciLogAttemptTimeout` (2 minutes: GitLab's default 100 MB job log
  limit at under 1 MB/s). An attempt cut off after it made progress keeps
  its part file and the next fetch resumes it with `Range: bytes=N-`,
  where N is the raw bytes taken; the follow shows it as downloading and
  resumes at the follow cadence, and a Save retries until it is whole. A
  completed GitLab trace and the GitHub blob answer that with a 206; an
  answer of 200, or a 206 under another ETag, is another log, and a 416
  (the cut came after the last byte, or the log is shorter now) has no
  byte to resume from: either way the download starts over from the
  first byte. A running GitLab trace ignores `Range` (200 with
  the whole body), so its growth is read whole each time; a completed
  trace honors it. Suffix ranges (`bytes=-N`) are refused by GitLab traces
  (400) and ignored by the GitHub log blob (200 with the whole body); the
  store never sends one.

## Isolation

- The forgefake `Engine` gains `ServeHTTP` over the existing `apiRoute`
  tables (patterns lose the `:fullpath` placeholder and match the real
  project path), served from its own loopback listener owned by the
  harness, separate from the control server. `Options.Isolated{BaseURL,
  Token}` points every host's REST base at `BaseURL/github/rest/` or
  `BaseURL/gitlab/api/v4/` and GraphQL at `BaseURL/github/graphql` or
  `BaseURL/gitlab/api/graphql`, sets
  the fixed token, and sends the forge host (port included) as the `Host`
  header, which the fake compares with the seeded repository's host and
  records on the invocation. The fake refuses any other bearer and does not
  record a request with no token at all (the boot's dev-server scan probes
  every listener it owns). REST route names (`gh api job logs`, `glab api job trace`,
  ...) are unchanged so specs keep asserting them; a GitHub GraphQL request
  is dispatched on its `operationName` and recorded under
  `gh graphql <operation>` (`gh graphql PRTick`), a GitLab one under
  `glab graphql <operation>`.
- The invocation record becomes a union:
  `{seq, route, unhandled, via: 'cli', cli, args, cwd, stdin, exitCode, stderr}`
  for the commands above and
  `{seq, route, unhandled, via: 'http', forge, host, method, path, operation, variables, status, detail}`
  for requests. `path` is the request path and query relative to the forge
  base, which is what specs matched through `args[1]` before; `operation`
  and `variables` are a GraphQL request's, so a spec can assert which parts
  a read asked for.
- `HarnessForgeOffline` turns the fake server's keep-alives off, which
  closes every idle pooled connection and lets a response already being
  written finish. Each request sent while offline arrives on a fresh
  connection, is recorded as route `offline` and is dropped without a reply,
  so the app sees the same `*TransientError` a dead network gives, not a 5xx
  or a reply on a stale connection, and a spec can count its retries. The
  listener stays up, so a request answered before going offline is never
  cut short. The CLI fake
  keeps answering `pr create` and `mr create` with its offline exit as today.
- `ao-mockforge` keeps only the CLI commands in the table above.

## Checks the change carries

- Unit: holder redaction through every formatter, zeroing on replace,
  negative cache expiry, header choice per forge and login kind, 401 refresh
  once then setup error, gate per pool, ETag hit and clear on token change,
  Range 200/206/416, redirect header drop, absolute URL refusal, a CI log
  download cut off and resumed, every row of the error table.
- Harness: `review-ci-live`, `review-ci-large-log`, `review-pr-outage` (its banner now comes from a
  `*TransientError`, shown once the retry also fails), `review-rate-limit`
  (`HarnessForgeRateLimit`), `forge-image-menu`, `compact-forge-image-menu`,
  `create-pr-flow` over the HTTP fake; offline recovery; unhandled route
  reporting.
- Manual read-only check against gitlab.com and github.com before the
  change ships (`docs/references/forge-api-measurements.md` lists the probes).
