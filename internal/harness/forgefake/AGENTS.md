# Fake forge

`forgefake` answers the `gh` and `glab` invocations an isolated boot makes,
from a fixture a test seeds. The path is:

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
   `HarnessForgeInvocations` and the `harness:forge` event. `HarnessReset`
   clears the fixture and the log.

The engine lives in the harness process, not the binary, so seeding and
inspection go through the harness wire with no fixture files.

## Answer rules

- A call no route claims, or one carrying a flag, `--json` field, query
  parameter, GraphQL query shape, header or jq expression its handler does
  not implement, is unhandled: exit 1, the full argv on stderr, recorded
  with `unhandled: true`. Specs assert none occurred
  (`e2e/tests/forge-helpers.ts`, `expectEveryForgeCallHandled`).
- A well-formed call for something the fixture lacks (an unknown PR, a
  missing upload) answers the forge's own not-found error. That is a
  handled call.
- A handler implements everything its route admits. Do not add a route
  that accepts a mutation and answers success without changing state.
- `routes.go` is the whole route table. A route's flag list is closed.

## Adding an endpoint

1. Add the fixture field the answer needs to `fixture.go`, with defaults
   and validation in `normalize`.
2. Add one handler in `github.go` or `gitlab.go` and one row in
   `routes.go` (a `commands` entry, or an `api` route with its method,
   pattern and allowed flags).
3. Test the routing and refusal in `engine_test.go`, and drive the app's
   own parser through the real binary in `cmd/ao-mockforge/binary_test.go`.
   The second test is what catches drift between the app's invocation and
   the handler.
4. Update the table below.

## Invocations

Handled, covering the reads the review pane, git status and CI make:

| CLI | Invocation | App caller |
|---|---|---|
| gh | `pr view --repo P N --json ...` | ViewPR, GetPRDetail, CI rollup |
| gh | `pr diff --repo P N` | GetPRDiff (no checkout) |
| gh | `pr list --head B --state open --json ...` | open PR lookup (checkout origin) |
| gh | `pr list --state merged --limit N --json ...` | merged heads (checkout origin) |
| gh | `run view ID --repo P --json jobs,workflowName` | CI jobs |
| gh | `api user --jq .login` | GetPRDetail viewer login |
| gh | `api graphql -f query=...` review threads and PR comments | ListReviewThreads |
| gh | `api repos/O/R/actions/jobs/ID/logs` | CI job log |
| gh | `api <attachment URL> -H "Accept: */*" [--allow-escape-sequences]` | forge attachments |
| glab | `mr diff N -R P` | GetPRDiff (no checkout) |
| glab | `api projects/P/merge_requests/N` | ViewPR, GetPRDetail, CI |
| glab | `api projects/P/merge_requests/N/approvals` | GetPRDetail |
| glab | `api --include projects/P/merge_requests/N/discussions?...` | ListReviewThreads |
| glab | `api projects/:fullpath/merge_requests?...` (opened, merged) | open MR lookup, merged heads |
| glab | `api projects/P/pipelines/ID/jobs?...` | CI jobs |
| glab | `api projects/P/jobs/ID/trace` | CI job log |
| glab | `api projects/P/uploads/SECRET/NAME` | forge attachments |

Unhandled (fail with their argv): `gh pr create`; `gh api` review, file
comment and reply POSTs; the GraphQL resolve and unresolve mutations;
`glab mr create`; `glab api` draft notes, bulk publish, approve,
discussion note POST and discussion resolve PUT.

## Fixture notes

- `gh pr list` without `--repo` and GitLab `:fullpath` resolve the
  repository from the call's cwd (`git config remote.origin.url`). The
  seeded `host` and `project` must match that origin.
- A GitLab path may name the project by its numeric `id` instead of its
  path; both resolve.
- The app caches the GitHub viewer login for 15 minutes per `Core`, and
  `HarnessReset` does not clear it. A `viewer` seeded after the boot's
  first GitHub PR load is not observed, so shared-worker specs keep the
  default viewer.
- Generated ids start at 1,000,000 and stay unique across reseeds and
  resets.

Run `go test ./internal/harness/forgefake ./cmd/ao-mockforge` after a
change here, and the Playwright specs that seed a forge
(`rg -l seedForge e2e/tests`).
