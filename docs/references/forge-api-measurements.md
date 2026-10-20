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
  `x-ms-error-code: BlobNotFound`); `gh` prints `gh: HTTP 404`. A running
  job has no log blob: every running job of randalmurphal/agent-overflow
  probed on 2026-10-11 (CodeQL analyses and Playwright shards, 1.5 to 7.5
  minutes long, polled every 30 to 40 seconds, and one more re-probed the
  same day) answered 404 for its whole run and served a `BlockBlob` from
  the poll after it completed. The jobs API can still report a job
  `in_progress` after its log is served: one such job answered 200 with
  2.1 MB, then 5.7 MB a minute later, an append blob whose `ETag` and
  `Last-Modified` moved while the log was assembled after completion, not
  a live stream; it answered `Range: bytes=-500` with 200 and the whole
  body. A 404 for a running job is a wait, not a failure.
- GitLab `GET /projects/ID/jobs/ID/trace` on a completed job honors
  `Range: bytes=0-99` (206, `Content-Range: bytes 0-99/1068374`) and refuses
  a suffix range (400). It sends `Content-Length` and an `ETag`. A running
  trace (gitlab-org/gitlab-runner, 20 KB) ignores `Range` entirely: `0-99`,
  `1000-` and a start past the end all answer 200 with the whole body. A
  missing job answers `{"message":"404 Not found"}`; `glab` prints
  `glab: 404 Not found (HTTP 404)`.

## Merge readiness and actions

Read-only `glab api` GETs on 2026-10-20 against gitlab.com (server
`19.6.0-pre`, glab 1.117.0, an OAuth2 login) on the merge requests the
login authored, plus GitLab's own source (`app/services/system_notes/`,
`lib/api/merge_requests.rb`, `app/models/merge_request.rb`) read through
the same API. No merge action was sent.

- `projects/P` carries `merge_method`, `squash_option`,
  `remove_source_branch_after_merge`,
  `only_allow_merge_if_pipeline_succeeds`,
  `only_allow_merge_if_all_discussions_are_resolved` and
  `permissions.project_access` / `group_access` (`access_level`). It sends
  a weak `ETag` (`W/"..."`, `Cache-Control: max-age=0, private,
  must-revalidate`) and answers 304 to a matching `If-None-Match`.
- The single merge request carries `detailed_merge_status`,
  `has_conflicts`, `draft`, `state`, `merged_at`, `merged_by`,
  `merge_user`, `closed_at`, `closed_by`, `user.can_merge`,
  `merge_when_pipeline_succeeds`, `squash`, `squash_on_merge`,
  `force_remove_source_branch`, `should_remove_source_branch`,
  `blocking_discussions_resolved`, `project_id`, `source_project_id`,
  `merge_commit_sha`, `squash_commit_sha`, `merge_after`. No
  `auto_merge_strategy`. A user field with no user is an object of nulls.
  A merged merge request keeps `merge_when_pipeline_succeeds: true` when
  it was auto-merged.
- The approvals read carries `approvals_required`, `approvals_left`,
  `approved`, `approved_by[].user`, `approved_by[].approved_at`,
  `user_has_approved`, `user_can_approve`. An author could approve their
  own merge request on this instance; a user who has approved reads
  `user_can_approve: false`.
- `detailed_merge_status` values observed on open merge requests:
  `mergeable`, `unchecked`, `not_approved`, `discussions_not_resolved`,
  `draft_status`, `requested_changes`, `conflict`; on merged ones
  `not_open`.
- System notes in the discussions (first line, the phrasing GitLab's
  source writes): `approved this merge request`, `unapproved this merge
  request`, `marked this merge request as **draft**` (optionally ` from
  <commit>`), `marked this merge request as **ready**`, `enabled an
  automatic merge when all merge checks for <sha> pass`, `canceled the
  automatic merge`, `aborted the automatic merge because <reason>`,
  ``changed target branch from `X` to `Y` ``, ``deleted the `X` branch.
  This merge request now targets the `Y` branch``, `requested review from
  @a, @b, and @c` (optionally ` and removed review request for ...` or
  ` and removed approval`), `added N commit(s)` followed by an HTML commit
  list and a "Compare with previous version" link. Merges, closes and
  reopens are resource state events, not notes: no merged, closed or
  reopened note appears on a merged merge request. GitLab writes no
  force-push or rebase wording; a list item `N commits from branch
  <target>` appears for a rebase and a merge of the target alike.
- Draft titles: GitLab's prefix is `[draft]`, `(draft)` or `draft:`, any
  case, stripped repeatedly (at most 20); `WIP:` is no longer a draft
  prefix.
- `PUT .../merge` reads `auto_merge = merge_when_pipeline_succeeds ||
  auto_merge` (both still accepted on 19.6); Grape ignores an undeclared
  parameter, so a server before 17.11 drops `auto_merge` rather than
  answering 400. A user who cannot merge gets 401 (`unauthorized!`), a
  merge request that is not mergeable 405, a moved head 409, a failed
  merge 422. `POST .../cancel_auto_merge` (19.5+) answers 201 with the
  merge request, 403 or 409;
  `POST .../cancel_merge_when_pipeline_succeeds` answers 201 with a status
  hash whose `status` is `error` when nothing was armed. An explicit
  `should_remove_source_branch: false` overrides the merge request's
  `force_remove_source_branch` (`MergeService#delete_source_branch?`).
- `GET personal_access_tokens/self` with an OAuth2 token answers 400
  "This endpoint requires token type to be a personal access token". The
  personal-token answer (`expires_at` as a date, `revoked`, `active`) was
  not observed on this machine.
