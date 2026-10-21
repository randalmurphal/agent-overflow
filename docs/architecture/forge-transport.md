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
func (s *Service) GitLab(host string) *Client   // REST base for host
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
  `SetThreadResolved`, the merge actions) take a
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
  `ListPipelines`, `ReadPipeline`, `StreamCIJobLog`, `FetchAttachment`,
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
| GitLab `H` | `api_protocol://api_host/api/v4/` from glab's per-host config, default `https://H/api/v4/` | none |

Base URLs are resolved once per host and kept with the host's client.

## Credentials

```go
// Token holds one host's bearer secret. Only authorize reads it.
type Token struct{ b []byte }

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
stepsFor)` per tick. `want` (`git.PRReadParts{Detail, Threads, CI, Events, Unresolved}`) names
the parts the caller needs and the read fetches nothing else: the pump
asks for `Detail`, `Threads` and `Events` (the forge events the
Conversation feed shows: approvals, merges, draft flips, auto-merge,
force pushes), the CI phase for `CI` alone on its own cadence. A merge
action (`ApprovePR`, `UnapprovePR`, `MergePR`, `CancelPRAutoMerge`,
`UpdatePRBranch`, `MarkPRReady`) that succeeds asks the PR's live pump for
one snapshot poll at once (`prPumpRequest.snapshot`), so every pane on the
PR follows the forge's answer without waiting out the interval, and
answers with the pump's sequence after that poll (`PRActionResult.Seq`,
zero with no pane on the PR) so the pane knows when the answer is on its
screen; the
readiness, merge options and events in the detail come from that read,
never from the action's own response. `git.Core` keeps `GetPRDetail`, `ListReviewThreads` and
`ListPRCIJobs` as one-part wrappers for the callers that need one part.

GitHub answers a read with one GraphQL request, `PRTick`:

| Variable | Selects |
|---|---|
| `owner`, `name`, `number` | the pull request; `number` is always selected |
| `wantDetail` | `viewer`, the detail fields, `latestReviews(first: 100)`, and what readiness and merge options derive from: the merge state and viewer flags, `autoMergeRequest`, `latestOpinionatedReviews(first: 100)`, `threadStates: reviewThreads(first: 100) { isResolved }`, `baseRef { refUpdateRule rules(first: 100) }`, and the repository's merge methods, `deleteBranchOnMerge`, `autoMergeAllowed`, `viewerPermission` and `viewerDefaultMergeMethod` |
| `wantChecks` | `commits(last: 1)`: the head commit's `id` and `statusCheckRollup.contexts(first: 100)`, each with `isRequired(pullRequestNumber: $number)`; true for `Detail` or `CI`, since the detail's check summary and readiness and the pipeline all come from the rollup |
| `wantThreads` | `reviewThreads(first: 100)`, each with `comments(first: 100)`, and `comments(first: 100)` |
| `wantEvents` | `timelineItems(last: 100, itemTypes: [...])`: the review, merge, close, reopen, draft, auto-merge, force-push, base-change and review-request items `ReviewEvent` maps |

The document is the same every tick; the parts are `@include` directives
on those variables. A connection longer than one page continues with its
own operation from the page's `endCursor`: `PRThreadsPage` and
`PRCommentsPage(owner, name, number, after)`, `ThreadCommentsPage(threadID,
after)` and `RollupContextsPage(commitID, number, after)` through `node(id:)`.
`PRThreadStatesPage(owner, name, number, after)` continues the thread
states only when readiness needs them (the base requires conversation
resolution and the first page has no unresolved thread) or the read asks
for `Unresolved` (the exact count `MergeReadiness.Unresolved` carries;
without every page it is nil), and
`PRTimelinePage(owner, name, number, before)` reads the timeline backwards
from the tick's newest page. Each connection reads at most
`githubReadMaxPages` (10) pages of 100. A
CI read adds REST requests only for what the rollup lacks: the jobs list
of a run (`repos/O/R/actions/runs/ID/jobs?per_page=100`, paged by `Link`)
while a job in `stepsFor` has steps that are not settled in `prev`, and a
job's log on request (`StreamCIJobLog`: `Stream` with `NoLimit` into the
CI log store, 404 is `ErrCIJobLogNotFound`).

| Tick | GitHub requests |
|---|---|
| Pump, a PR within one page per connection | 1 (`PRTick`) |
| CI phase, no followed job | 1 (`PRTick`) |
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

GitLab's `ReadPR` reads the merge request once
(`projects/P/merge_requests/N`) and derives every part from it: `Detail`
adds its approvals, the project's merge settings (`projects/P`, cached
per login and project for 10 minutes, then revalidated through the ETag
store: a `W/` ETag answered 304) and the viewer's login (`user`, cached
per login and host for 10 minutes; the author may cancel an auto-merge, and a refused read
is cached as an unknown viewer), `Threads`, `Events` and `Unresolved` its discussions
(`per_page=100`, paged by `X-Next-Page`, positions normalized against
`diff_refs.head_sha`; one read serves both, system notes are the events,
and the merge request's `merged_at`/`closed_at` supply the merge and close
GitLab records as state events rather than notes), and `CI`
the jobs of its `head_pipeline` (`per_page=100`, paged by `X-Next-Page`,
at most `gitlabCIJobsMaxPages` (5) pages). A job's trace is read on
request (`StreamCIJobLog`, as on GitHub).

| Tick | GitLab requests |
|---|---|
| Pump (`Detail`, `Threads`, `Events`) | 1 merge request + 1 approvals + 1 discussions page per 100 discussions; + 1 `projects/P` per login and project and 1 `user` per login and host per 10 minutes |
| CI phase, a head pipeline | 1 merge request + 1 jobs page per 100 jobs |
| CI phase, no head pipeline | 1 merge request |
| Log follow, a followed running job (every 5s) | 1 trace (304 while unchanged) |

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

GitHub lists open PRs with one GraphQL `PullsSearch(query, after)`:
`search(type: ISSUE, query: "is:pr is:open repo:O/R", first: 100)`
beside `viewer { login }`. Each node selects its review state
(`reviewDecision`, `reviewRequests(first: 20)`,
`latestOpinionatedReviews(first: 100) { viewerDidAuthor }`,
`viewerLatestReview`, since the login is not known before the answer),
its merge state, `totalCommentsCount`, `commits(last: 1)` with the
head commit's time and rollup `state`, and `comments(last: 10)` and
`reviews(last: 10)` with each author and time, from which the row takes
the PR author's latest reply (`AuthorActivityAt`; a review thread reply
is a review of its own). Each of those connections costs one point per
page of 100 PRs. GitLab lists
`projects/P/merge_requests?state=opened&order_by=updated_at&sort=desc&per_page=100`,
whose basic view carries no pipeline; the viewer is the login's cached
username, as the detail read uses it. Both read at most `prListMaxPages`
(10) pages of 100: GitHub search answers at most 1000 results, and
GitLab's list stops at the 1000 most recently updated.

| Read | GitHub | GitLab |
|---|---|---|
| Open PRs of a project | 1 `PullsSearch` per 100 PRs | 1 merge request list page per 100 MRs; + 1 `user` per login and host per 10 minutes |
| Pipelines of a project (`ListPipelines`, at most `Limit` rows, 100 when unset or larger) | 1 `GET repos/O/R/actions/runs?per_page=N[&branch=][&status=]` | 1 `GET projects/P/pipelines?per_page=N[&ref=][&status=]`; + `GET projects/P/pipelines?per_page=100&scope=tags[&ref=]` pages, at most `gitlabTagScopeMaxPages` (10): 1 for a ref, 1 or 2 without a status filter, none for a merge request ref or an empty list; + 1 `GET projects/P/pipelines/ID` per row on a tag's ref name whose id the tag scope does not hold, once per pipeline while the repository's listings hold it, 4 at a time |
| One pipeline (`ReadPipeline`) | 1 `GET repos/O/R/actions/runs/ID` + 1 jobs page per 100 jobs, at most `githubReadMaxPages` | 1 `GET projects/P/pipelines/ID` + 1 jobs page per 100 jobs, at most `gitlabCIJobsMaxPages` |
| Stages of 30 unchanged rows (`ReadPipelineStages`) | 30 runs + 30 jobs pages, each a 304 | 30 pipelines + 30 jobs pages, each a 200 with its body |

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
pipeline id (a pipeline's tag flag never changes); each listing prunes
the repository's memory to the pipelines it answered. A read that
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
`SetPRListFocus`, `RefreshPRList`, `UnsubscribePRList`) route home and
the caller pins them to the computer whose subscribe answered, as the
PR pump's. The pump shares the PR pump's core (`app_pump_core.go`):
refcounted handles bound to their connections, paused while every handle
is hidden, frames stamped from the shared PR update sequence. The
subscribe waits for the first tick, which is the user's action and so
interactive, as a refresh is; later ticks run at the PR update interval.

A tick (`app_pr_list_tick.go`) reads the backend's forge projects
(`ListForgeProjects`: the non-archived projects whose origin names a
forge repository, and those whose origin git could not read, with that
failure as their `Error`), lists each repository once however many projects
clone it, at most `prListReadConcurrency` (4) at a time, then reads the
summary detail of the rows the subscribers focus, joins the user's
`pr_views` and emits `pulls:updated` when the snapshot changed. A
project's failed list keeps the rows read before beside its error
(`ProjectPRList.Error`, the caller-safe sentence, and `ErrorDetails`, the
correlation id the server log holds the raw error under with that
error's chain, fixed while the same failure repeats), a
first transient failure with rows on screen is held back once, a
rate-limited repository is skipped by background ticks until its
release, a project whose origin cannot be read keeps the repository and
rows the last tick read beside that error, and a removed project takes
its rows along. Each row's
attention state and group come from `prAttention`
(`app_pr_attention.go`); `NeedsYou`, the sidebar badge, counts each PR
once.

`SetPRListFocus` names at most `maxPRListFocus` (60) PR keys per handle:
the rows on screen. The tick reads the detail of the rows a visible
handle focuses and then of the user's own and reviewed rows
(`ViewerIsAuthor`, `ViewerIsReviewer`), whose attention state needs it
whether or not a filter shows them, at most `maxPRListFocus` in all:
each group newest update time first, then by PR key, so unchanged rows
keep the same set every tick. A row that falls out of the set loses its
detail. A focusing handle that hides or shows recomputes the set at
once (`SetPRListActive`), as does one that leaves. The reads a focus
change or a show starts are the user's and interactive; those a hide or
a leave starts are background, since they read rows no user action
asked for, and a pending user wake keeps its admission: it is recorded
with the focus change and taken with the selection that reads it. A
background read refused at the reserve is not a read, so the next
interactive pass reads that row again. A row's detail is
`Core.ReadPRSummary`: `ReadPR` with `Detail` and `Unresolved`, which adds
the approval count and the unresolved thread count to the readiness. It
is read when the row is first a candidate, when its list row changes
(update time, head, comments, checks, merge status, draft, review
decision), on a refresh, and otherwise once `prListDetailMaxAge` (5
minutes) old. Reading
60 GitHub details every interval would spend the 5000 points an hour
GitHub's GraphQL allows; the age bound is what a GitLab pipeline that
finished needs, since that moves no list field.

`MarkPRViewed` records an opening in `pr_views` with the comment and
commit counts of the pump's latest row, and the pump republishes so the
row's `Seen` and `Since` follow at once.

| Tick | GitHub | GitLab |
|---|---|---|
| Lists | 1 `PullsSearch` per repository per 100 PRs | 1 merge request list page per repository per 100 MRs |
| A focused row's detail, when due | 1 `PRTick` (detail and checks), + 1 `PRThreadStatesPage` per 100 threads past 100 | 1 merge request + 1 approvals + 1 reviewers page per 100 reviewers until the viewer's (the viewer's own review state) + 1 discussions page per 100 discussions |

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
list measured free of the limit); on GitLab the jobs list answers 200 to
`If-None-Match` (measured) and the single pipeline's revalidation is not
measured, so 30 unchanged rows cost 30 pipeline reads and 30 jobs list
bodies.

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
    Header      http.Header   // Range, If-None-Match, Accept
    Body        any           // JSON-encoded; io.Reader passes through
    Timeout     time.Duration // per call; zero takes Options.ReadTimeout (45s); covers headers and body
    Attachment  bool          // allows an absolute Path
}
// Interactive comes from the context (WithInteractive), not the Request.

type Response struct {
    Status    int
    Header    http.Header
    Body      []byte          // bounded by Options.MaxBodyBytes (1 MiB) for JSON
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
  `MaxConnsPerHost: 4`, `MaxIdleConnsPerHost: 4`, `IdleConnTimeout: 90s`,
  `ResponseHeaderTimeout: 30s`, dial timeout 10s. No `http.Client.Timeout`;
  deadlines come from `ctx` and `Request.Timeout`. A per-host semaphore of 4
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
| 401 after one token refresh on a mutation whose `user` probe fails otherwise (rate limited, 5xx, unreachable, timeout, cancelled) | the probe's own error | Retry; never "log in again" |
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

A bound method that acts on a PR (`SubmitPRReview`, `ReplyToPRThread`,
`SetPRThreadResolved`, the merge actions) returns its forge failure
through `git.ExplainForgeError`: one sentence naming the cause and the
fix, carried as reviewed prose under the code `forge_<kind>` (`transient`,
`rate_limited`, `setup`, `forbidden`, `not_found`, `stale_head`, `refused`,
`forge`) with the forge's own error in the detail chain. The client shows
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
  sends `If-None-Match`; a 304 returns the stored body with `NotModified`.
  GitHub REST 304s are free of the limit; GraphQL (POST) has no ETag, so
  `PullsSearch` is read whole each refresh. GitLab honors `If-None-Match`
  on merge requests, their list and job traces, not on the pipeline jobs
  list (200 with the same weak ETag).
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
  `BaseURL/gitlab/api/v4/` and GraphQL at `BaseURL/github/graphql`, sets
  the fixed token, and sends the forge host (port included) as the `Host`
  header, which the fake compares with the seeded repository's host and
  records on the invocation. The fake refuses any other bearer and does not
  record a request with no token at all (the boot's dev-server scan probes
  every listener it owns). REST route names (`gh api job logs`, `glab api job trace`,
  ...) are unchanged so specs keep asserting them; a GitHub GraphQL request
  is dispatched on its `operationName` and recorded under
  `gh graphql <operation>` (`gh graphql PRTick`).
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
