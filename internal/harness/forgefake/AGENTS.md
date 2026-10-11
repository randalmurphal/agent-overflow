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
   `HarnessForgeRateLimit` and the `harness:forge` event. `HarnessReset`
   clears the fixture, the log, the offline state and the rate limits.

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
  that accepts a mutation and answers success without changing state.
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
  on any other route `If-None-Match` is an unimplemented flag. `Range` has no handler, so a
  request carrying it is unhandled.
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
| GitLab | `GET projects/P` (`glab repository identity`) | repository identity |
| ssh | `-G -o CanonicalizeHostname=no -o PermitLocalCommand=no [-l U] H` | local SSH host resolution, from fixture `sshHosts` |
| GitHub | `gh pr create --title T --body B [--base B] [--draft]` | GitCreatePR |
| GitLab | `glab mr create --title T --description D --yes --no-editor [--target-branch B] [--draft]` | GitCreatePR |
| GitHub | GraphQL `PRTick(owner, name, number, wantDetail, wantThreads, wantChecks)` (`gh graphql PRTick`) | ReadPR: detail and viewer, threads and comments, head commit rollup (CI) |
| GitHub | GraphQL `PRThreadsPage`, `PRCommentsPage(owner, name, number, after)` | ReadPR: review threads and comments past the first 100 |
| GitHub | GraphQL `ThreadCommentsPage(threadID, after)`, `RollupContextsPage(commitID, after)` | ReadPR: one thread's comments, the rollup's contexts past 100 |
| GitHub | GraphQL `SetThreadResolved(threadID, resolved)` | SetThreadResolved (flips the seeded thread) |
| GitHub | GraphQL `OpenPRsByHead(owner, name, head)` | open PR lookup (checkout origin) |
| GitHub | GraphQL `MergedPRs(owner, name, first, after)` | merged heads (checkout origin), in fixture order |
| GitHub | `GET repos/O/R/actions/runs/ID/jobs?per_page=N[&page=N]` (`gh api run jobs`, conditional) | CI steps of a followed job's run; `Link` rel="next" while more pages exist |
| GitHub | `GET repos/O/R/actions/jobs/ID/logs` (`gh api job logs`, conditional) | CI job log (404 while the job is running or pending, as the real endpoint, and while the job sets `logWithheld`); answered directly where the real endpoint redirects to its log blob, with the blob's ETag behavior |
| GitHub | `GET <attachment URL>` (`gh api attachment`) | forge attachments |
| GitLab | `GET projects/P/merge_requests/N` (`glab api merge request`, conditional) | ReadPR, once per read: detail, head SHA for threads, head pipeline for CI |
| GitLab | `GET projects/P/merge_requests/N/approvals` (`glab api approvals`) | ReadPR detail |
| GitLab | `GET projects/P/merge_requests/N/discussions?per_page=N[&page=N]` (`glab api discussions`) | ReadPR threads; `X-Next-Page` while more pages exist |
| GitLab | `GET projects/P/merge_requests?state=opened\|merged&...` (`glab api merge request list`, conditional) | open MR lookup, merged heads (checkout origin) |
| GitLab | `GET projects/P/pipelines/ID/jobs?per_page=N[&page=N]` (`glab api pipeline jobs`) | ReadPR CI |
| GitLab | `GET projects/P/jobs/ID/trace` (`glab api job trace`, conditional) | CI job log (the current `log` of a started job, running included; 404 while the job sets `logWithheld`) |
| GitLab | `GET projects/P/uploads/SECRET/NAME` (`glab api upload`) | forge attachments |

Unhandled (fail with their request): GitHub's review, file comment and
reply POSTs; GitLab's draft notes, bulk publish, approve, discussion note
POST and discussion resolve PUT.

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
- Generated ids start at 1,000,000 and stay unique across reseeds and
  resets.

Run `go test ./internal/harness/forgefake ./cmd/ao-mockforge` after a
change here, and the Playwright specs that seed a forge
(`rg -l seedForge e2e/tests`).
