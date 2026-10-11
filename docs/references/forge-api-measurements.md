# Forge API measurements

Read-only probes against github.com and gitlab.com on 2026-10-10 with the
developer's own `gh` and `glab` logins, run from the WSL work laptop. They
justify [forge transport](../architecture/forge-transport.md). Re-run the
probe when a forge changes behavior; the design states which claim each
mechanism rests on.

The shipped transport was checked the same day with a build-tagged test in
`internal/git` (deleted after the run, not part of the suite) that built a
`forgeapi.Service` over `CLITokenSource` and called `Core.ReadPR` with every
part on a public GitHub PR and a private GitLab MR, `Core.GetCIJobLog` once
and again with the returned ETag (304 on both forges), a bogus job id (404
to `ErrCIJobLogNotFound`), `Core.ListOpenPRs` on a pushed and an unpushed
branch, and the branch lookup's unpushed skip. Write paths were not
exercised live: that needs a throwaway PR and MR.

## Process cost

| Call | Wall time |
|---|---|
| `gh api rate_limit` (one process) | 0.31 s |
| `gh pr view --json statusCheckRollup` | 0.40 s |
| `glab api user` (one process) | 0.45 s |
| `curl` to the same endpoints, fresh connection | 0.18 to 0.25 s |
| `curl`, reused connection | 0.13 to 0.19 s |

`gh run view --json jobs` costs 3 REST requests; `gh pr view --json
statusCheckRollup` costs 1 GraphQL request and preloads every page.

## Authentication

- `gh auth token --hostname github.com` prints the token and nothing else.
  `gh auth status --json hosts` lists hosts with `login`, `tokenSource`,
  `scopes` and `gitProtocol`.
- glab has no `auth token` command. `glab config get token --host H` prints
  the token when it is in the config file and nothing when it is in the OS
  keyring; `glab auth status --hostname H -t` prints it in either case.
  `glab config get api_host --host H` and `api_protocol` give the API base.
  `is_oauth2` is set for a browser login.
- glab 1.117 sends `Private-Token` for a personal access token.
  gitlab.com also accepts a personal access token as `Authorization: Bearer`
  (200 on `/api/v4/user`).
- GitLab personal access tokens contain `.`; a token regex must allow it.

## Rate limits

- GitHub: `X-RateLimit-Limit: 5000`, `X-RateLimit-Remaining`,
  `X-RateLimit-Reset` (epoch seconds), `X-RateLimit-Resource` naming the pool
  (`core` for REST, `graphql` for GraphQL, `search`). The pools are separate
  budgets. A secondary limit answers 403 or 429 with `Retry-After`.
- GitLab: `RateLimit-Limit: 2000`, `RateLimit-Observed`,
  `RateLimit-Remaining`, `RateLimit-Reset` (epoch seconds),
  `RateLimit-Name: throttle_authenticated_api`. Exhaustion answers 429 with
  `Retry-After`.

## Conditional requests

GitLab returns an `ETag` on the merge request list, a single merge request
and a job trace, running or completed (`Cache-Control: max-age=0, private,
must-revalidate`); the same URL with `If-None-Match` answers 304 with an
empty body. The pipeline jobs list carries a weak `ETag` but answers 200 to
`If-None-Match`. GitHub `actions/runs/ID/jobs` answers 304 for a matching
`If-None-Match` and `X-RateLimit-Remaining` does not move.

## Logs

- GitHub `GET /repos/O/R/actions/jobs/ID/logs` answers 302 to a signed Azure
  blob URL (`sig=` and friends in the query). The blob honors
  `Range: bytes=0-99` (206, `Content-Range: bytes 0-99/91712`,
  `Accept-Ranges: bytes`) and ignores a suffix range `bytes=-100` (200 with
  the whole body). It sends `Content-Length` and an `ETag`, and the blob
  answers 304 to a matching `If-None-Match` (checked through the shipped
  transport on 2026-10-10, as did a GitLab trace). A job whose log blob
  does not exist answers 404 on the blob hop (XML `BlobNotFound`, with
  `x-ms-error-code: BlobNotFound`); `gh` prints `gh: HTTP 404`. Measured
  2026-10-11 on running jobs of randalmurphal/agent-overflow: one running
  job answered 200 with 2.1 MB and 5.7 MB a minute later, an append blob
  whose `ETag` and `Last-Modified` moved as it grew, and which answered
  `Range: bytes=-500` with 200 and the whole body. Every other running job
  probed that day (CodeQL analyses and Playwright shards, 1.5 to 7.5
  minutes long, polled every 30 to 40 seconds) answered 404 for its whole
  run and served a `BlockBlob` from the poll after it completed. So a
  running job's log is served once its blob exists, and a 404 for a
  running job is a wait, not a failure.
- GitLab `GET /projects/ID/jobs/ID/trace` on a completed job honors
  `Range: bytes=0-99` (206, `Content-Range: bytes 0-99/1068374`) and refuses
  a suffix range (400). It sends `Content-Length` and an `ETag`. A running
  trace (gitlab-org/gitlab-runner, 20 KB) ignores `Range` entirely: `0-99`,
  `1000-` and a start past the end all answer 200 with the whole body. A
  missing job answers `{"message":"404 Not found"}`; `glab` prints
  `glab: 404 Not found (HTTP 404)`.
