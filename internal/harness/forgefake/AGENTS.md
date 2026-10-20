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
   `HarnessForgeRateLimit`, `HarnessForgeRefuse` and the `harness:forge`
   event. `HarnessReset` clears the fixture, the log, the offline state,
   the rate limits and the refusals.

The HTTP path: `harnessrpc.StartForgeAPI` serves `Engine.ServeHTTP` on its
own loopback listener and hands its base URL and fixed token to the boot
(`app.SetForgeAPI`), whose `forgeapi.Service` is built with
`Options.Isolated` (see
[forge transport](../../../docs/architecture/forge-transport.md#isolation)).
`ServeHTTP` maps a REST request onto the `api` route it stands for
(`http.go`) and a GraphQL request onto the operation its `operationName`
names (`github_graphql.go`), and answers from the same fixture. Offline
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
  operation's entry in `githubGraphQL`. An HTTP request with a token
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
  name (`glab api approve`) or `gh graphql <operation>`. It is answered
  after the route matched and before its handler runs, so nothing
  changes, and is recorded under the route. A 401 on a mutation is
  retried once after the transport's token refresh and then probed with
  `GET user`, so drive one with `times` 0.
- `routes.go` is the whole CLI and REST route table, `githubGraphQL` the
  whole GraphQL one. A route's flag list is closed; a REST route's flags
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

Handled, covering the reads the review pane, git status and CI make, the
thread resolve toggle and the Create PR/MR dialog. Every row but the CLI
ones is served over HTTP only: GitHub REST at `/github/rest/E`, GraphQL at
`/github/graphql` (a POST of `{"query", "operationName", "variables"}`),
an attachment URL at `/github/absolute/<host>/<path>`, GitLab REST at
`/gitlab/api/v4/E`. The request's Host header is the invocation's
recorded `host` (an attachment's is the host in its path); a GraphQL
invocation also records its `operation` and `variables`. P is the
project's path, escaped as one segment (`grp%2Fsub%2Ftool`).

| Forge | Invocation (route) | App caller |
|---|---|---|
| GitHub | `GET repos/O/R` (`gh repository identity`) | repository identity |
| GitLab | `GET projects/P` (`glab repository identity`, conditional) | repository identity; the merge settings and the viewer's access the Merge section reads |
| ssh | `-G -o CanonicalizeHostname=no -o PermitLocalCommand=no [-l U] H` | local SSH host resolution, from fixture `sshHosts` |
| GitHub | `gh pr create --title T --body B [--base B] [--draft]` | GitCreatePR |
| GitLab | `glab mr create --title T --description D --yes --no-editor [--target-branch B] [--draft]` | GitCreatePR |
| GitHub | GraphQL `PRTick(owner, name, number, wantDetail, wantThreads, wantEvents, wantChecks)` (`gh graphql PRTick`) | ReadPR: detail and viewer with the repository's merge settings and the merge box (readiness, opinions, thread states, base protection, auto-merge), threads and comments, the newest 100 timeline items, head commit rollup (CI, each check's `isRequired`) |
| GitHub | GraphQL `PRThreadsPage`, `PRCommentsPage`, `PRThreadStatesPage(owner, name, number, after)` | ReadPR: review threads, comments and thread states past the first 100 |
| GitHub | GraphQL `PRTimelinePage(owner, name, number, before)` | ReadPR events: older timeline pages, read backwards |
| GitHub | GraphQL `ThreadCommentsPage(threadID, after)`, `RollupContextsPage(commitID, number, after)` | ReadPR: one thread's comments, the rollup's contexts past 100 |
| GitHub | GraphQL `PRMergeTarget(owner, name, number)`, `ViewerApproval(owner, name, number)` | merge actions: the pull's node id and head; Unapprove: the viewer's standing approval |
| GitHub | GraphQL `MergePR`, `EnableAutoMerge(pullRequestID, method, expectedHeadOid)` | Merge now (merged, MergedEvent and ClosedEvent, head deleted when the repository does) and Auto-merge (armed, the method's enabled event); a head other than `expectedHeadOid` answers "Head branch was modified. Review and try the merge again." |
| GitHub | GraphQL `DisableAutoMerge`, `MarkPRReady(pullRequestID)`, `UpdatePRBranch(pullRequestID, expectedHeadOid)`, `DismissReview(reviewID, message)` | Cancel auto-merge, Mark ready, Update branch (the head moves to a new commit), Unapprove (the review reads DISMISSED, a ReviewDismissedEvent) |
| GitHub | `POST repos/O/R/pulls/N/reviews` with `{event, body?, commit_id?}` (`gh api pull reviews`) | Approve and the send strip's verdict without line comments: the viewer's review, a timeline item; the author's approval is GitHub's 422 |
| GitHub | `DELETE repos/O/R/git/refs/heads/B` (`gh api delete branch`) | the head branch delete after a merge: 204, then 422 "Reference does not exist" |
| GitHub | GraphQL `SetThreadResolved(threadID, resolved)` | SetThreadResolved (flips the seeded thread) |
| GitHub | GraphQL `OpenPRsByHead(owner, name, head, after)` | open PR lookup (checkout origin): pages of 100 with each PR's node id and `isCrossRepository` (a pull with `headRepo`) |
| GitHub | GraphQL `PRStates(ids)` | merged-PR cleanup: state and `mergedAt` of tracked PRs by node id; an unknown id is a `NOT_FOUND` error at its `nodes` index |
| GitHub | GraphQL `MergedPRs(owner, name, first, after)` | merged heads (checkout origin), in fixture order |
| GitHub | `GET repos/O/R/actions/runs/ID/jobs?per_page=N[&page=N]` (`gh api run jobs`, conditional) | CI steps of a followed job's run; `Link` rel="next" while more pages exist |
| GitHub | `GET repos/O/R/actions/jobs/ID/logs` (`gh api job logs`, conditional) | CI job log (the current `log` of a started job; 404 while the job is pending and while it sets `logWithheld`, as the real endpoint does until the job's log blob exists, which for a running job is its whole run; a running job with a log models the jobs API still reporting a completed job running); answered directly where the real endpoint redirects to its log blob, with the blob's ETag and Range behavior: a start or closed range is a 206 with `Content-Range`, a suffix range is ignored (200, whole body), a start at or past the end is a 416 (HTTP range semantics; not measured on the forges) |
| GitHub | `GET <attachment URL>` (`gh api attachment`) | forge attachments |
| GitLab | `GET projects/P/merge_requests/N` (`glab api merge request`, conditional) | ReadPR, once per read: detail, head SHA for threads, head pipeline for CI, the merge fields with `detailed_merge_status` derived from the fixture |
| GitLab | `GET projects/P/merge_requests/N/approvals` (`glab api approvals`) | ReadPR detail: approvals required and left, the viewer's standing |
| GitLab | `GET projects/P/merge_requests/N/discussions?per_page=N[&page=N]` (`glab api discussions`) | ReadPR threads and, from the system notes, events; `X-Next-Page` while more pages exist |
| GitLab | `POST .../approve` with `{sha}`, `POST .../unapprove` (`glab api approve`, `glab api unapprove`) | Approve (409 on a moved head, 401 for a viewer GitLab takes no approval from) and Unapprove (404 with none), each with its system note |
| GitLab | `PUT .../merge` with `{sha, squash, should_remove_source_branch, auto_merge, merge_when_pipeline_succeeds}` (`glab api merge`) | Merge now (405 while blocked, 401 without the role, 409 on a moved head) and Auto-merge (either key or both: arms a blocked merge request, with its system note) |
| GitLab | `POST .../cancel_auto_merge`, `PUT .../rebase`, `PUT projects/P/merge_requests/N` with `{title}` | Cancel auto-merge (409 with none armed), Update branch (202, the head moves), Mark ready (the title loses its draft prefix) |
| GitHub, GitLab | `GET user` (`gh api user`, `glab api user`) | the fixture's viewer: GitLab's detail read (once per host per 10 minutes), and the transport's probe after a 401 on a mutation on both forges |
| GitLab | `GET projects/P/merge_requests?state=opened\|merged&...` (`glab api merge request list`, conditional) | open MR lookup, merged heads (checkout origin); the full view, with `project_id` and `source_project_id` (a pull with `headRepo` answers its fork's id) |
| GitLab | `GET projects/P/merge_requests?iids[]=N...&state=all&per_page=100` (`glab api merge request list`) | merged-PR cleanup: state and `merged_at` of tracked MRs |
| GitLab | `GET projects/P/pipelines/ID/jobs?per_page=N[&page=N]` (`glab api pipeline jobs`) | ReadPR CI |
| GitLab | `GET projects/P/jobs/ID/trace` (`glab api job trace`, conditional) | CI job log (the current `log` of a started job, running included; 404 while the job sets `logWithheld`). A completed job's trace honors a start or closed range (206) and refuses a suffix range (400); a running job's trace ignores `Range` (200, whole body) and a running `synthetic` trace grows on every request |
| GitLab | `GET projects/P/uploads/SECRET/NAME` (`glab api upload`) | forge attachments |

Unhandled (fail with their request): a GitHub review with line
comments, GitHub's file comment and reply POSTs; GitLab's draft notes,
bulk publish, discussion note POST, discussion resolve PUT and
`cancel_merge_when_pipeline_succeeds`.

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
- Generated ids start at 1,000,000 and stay unique across reseeds and
  resets.
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
