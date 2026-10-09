# Fake forge

`forgefake` answers `gh`, `glab` and repository `ssh -G` invocations an isolated boot makes,
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
   `HarnessForgeInvocations`, `HarnessForgeOffline` and the `harness:forge`
   event. `HarnessReset` clears the fixture, the log and the offline state.

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

Handled, covering the reads the review pane, git status and CI make, and
the Create PR/MR dialog:

| CLI | Invocation | App caller |
|---|---|---|
| gh | `api --hostname H repos/O/R` | repository identity |
| glab | `api --hostname H projects/P` | repository identity |
| ssh | `-G -o CanonicalizeHostname=no -o PermitLocalCommand=no [-l U] H` | local SSH host resolution, from fixture `sshHosts` |
| gh | `pr create --title T --body B [--base B] [--draft]` | GitCreatePR |
| glab | `mr create --title T --description D --yes --no-editor [--target-branch B] [--draft]` | GitCreatePR |
| gh | `pr view --repo P N --json ...` | GetPRDetail, CI rollup |
| gh | `pr list --head B --state open --json ...` | open PR lookup (checkout origin) |
| gh | `pr list --state merged --limit N --json ...` | merged heads (checkout origin) |
| gh | `run view ID --repo P --json jobs,workflowName` | CI jobs |
| gh | `api user --jq .login` | GetPRDetail viewer login |
| gh | `api graphql -f query=...` review threads and PR comments | ListReviewThreads |
| gh | `api repos/O/R/actions/jobs/ID/logs` | CI job log |
| gh | `api <attachment URL> -H "Accept: */*" [--allow-escape-sequences]` | forge attachments |
| glab | `api projects/P/merge_requests/N` | GetPRDetail, CI |
| glab | `api projects/P/merge_requests/N/approvals` | GetPRDetail |
| glab | `api --include projects/P/merge_requests/N/discussions?...` | ListReviewThreads |
| glab | `api projects/:fullpath/merge_requests?...` (opened, merged) | open MR lookup, merged heads |
| glab | `api projects/P/pipelines/ID/jobs?...` | CI jobs |
| glab | `api projects/P/jobs/ID/trace` | CI job log |
| glab | `api projects/P/uploads/SECRET/NAME` | forge attachments |

Unhandled (fail with their argv): `gh api` review, file comment and reply
POSTs; the GraphQL resolve and unresolve mutations; `glab api` draft
notes, bulk publish, approve, discussion note POST and discussion resolve
PUT.

## Fixture notes

- `gh pr list` without `--repo` and GitLab `:fullpath` resolve the
  repository from the call's cwd (`git config remote.origin.url`). The
  seeded `host` and `project` must match that origin.
- A GitLab path may name the project by its numeric `id` instead of its
  path; both resolve.
- A create opens the pull for the cwd's checked-out branch and commit
  (read with git) into `main` unless the call names a base, numbered one
  past the repo's highest. It does not check that the branch was pushed.
- The app caches the GitHub viewer login for 15 minutes per `Core`, and
  `HarnessReset` does not clear it. A `viewer` seeded after the boot's
  first GitHub PR load is not observed, so shared-worker specs keep the
  default viewer.
- Generated ids start at 1,000,000 and stay unique across reseeds and
  resets.

Run `go test ./internal/harness/forgefake ./cmd/ao-mockforge` after a
change here, and the Playwright specs that seed a forge
(`rg -l seedForge e2e/tests`).
