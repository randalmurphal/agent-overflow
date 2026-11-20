# Tracker API measurements

External behavior behind the [Issues spec](../specs/issues.md): Jira Cloud,
Linear, Tempo, the planning poker add-on and the SQLite and rendering facts
that shape dashboards. Facts marked "measured" were observed on one Jira
Cloud site on 2026-11-15 with a classic API token. The rest come from the
linked page as fetched that day. Recheck before building on any of them.

## Jira Cloud

Sources: the
[platform REST v3 spec](https://developer.atlassian.com/cloud/jira/platform/swagger-v3.v3.json),
the
[Jira Software spec](https://developer.atlassian.com/cloud/jira/software/swagger.v3.json)
and the [developer changelog](https://developer.atlassian.com/changelog/).

Authentication and limits:

- A token is created at
  `https://id.atlassian.com/manage-profile/security/api-tokens`. It expires
  in 1 to 365 days. A classic token calls the site URL; a scoped token must
  call `https://api.atlassian.com/ex/jira/{cloudId}`
  ([manage API tokens](https://support.atlassian.com/atlassian-account/docs/manage-api-tokens-for-your-atlassian-account/)).
- Basic auth with email and token is accepted by `rest/api/3`,
  `rest/agile/1.0` and `rest/software/1.0` (both specs; measured for all
  three).
- OAuth 2.0 (3LO) requires a registered app and client secret and has no
  PKCE flow
  ([3LO apps](https://developer.atlassian.com/cloud/jira/platform/oauth-2-3lo-apps/),
  [ECO-283](https://jira.atlassian.com/browse/ECO-283)).
- The hourly points quota applies to Forge, Connect and 3LO apps; "API
  token-based traffic is not affected" (CHANGE-3080). Token traffic is under
  burst limits (GET and POST 100 requests per second, PUT and DELETE 50)
  and per-issue write limits (20 per 2 seconds, 100 per 30 seconds). A 429
  carries `Retry-After` and `RateLimit-Reason`
  ([rate limiting](https://developer.atlassian.com/cloud/jira/platform/rate-limiting/)).

Boards, backlog and sprints:

- Board, backlog, sprint and epic issue lists moved to
  `/rest/software/1.0/...` with `nextPageToken` paging, no `total`, and
  `approximate-count` endpoints (CHANGE-3192). The `/rest/agile/1.0` list
  paths are deprecated for removal after 2026-11-01 (CHANGE-3193). Measured:
  both still answered, and the new paths returned `isLast` and
  `nextPageToken`.
- `GET /rest/agile/1.0/board/{id}/configuration` gives the column to status
  mapping, the estimation field and the rank field. The estimate must be
  read per board. Measured: a board estimated in a field other than the
  site's "Story Points", and the site had several estimate fields with
  near-identical names.
- Ranking is `PUT /rest/agile/1.0/issue/rank` with `rankBeforeIssue` or
  `rankAfterIssue`, at most 50 issues, returning 204 or a 207 with per-issue
  results. Moving into a sprint (`POST /sprint/{id}/issue`, future and
  active sprints only) and to a board's backlog
  (`POST /backlog/{boardId}/issue`) take the same rank fields and the same
  50 issue cap; `POST /backlog/issue` takes the issues alone. The spec
  documents a 207 for ranking and the board backlog move, and only a 204
  for the sprint move.
- A sprint is created with `POST /rest/agile/1.0/sprint`, started by setting
  `state` from `future` to `active` (start and end dates required) and
  completed by setting `active` to `closed`. No other state change is
  allowed. Completion has no parameter for where unfinished issues go; the
  web UI asks
  ([complete a sprint](https://support.atlassian.com/jira-software-cloud/docs/complete-a-sprint/)).
  The client moves them before closing. What the API does when open issues
  remain is not verified.
- There is no supported sprint report or velocity endpoint
  ([product team reply](https://community.developer.atlassian.com/t/agile-api-equivalent-for-a-greenhopper-sprintreport-url/3997)).
  Committed, added, removed and completed work is rebuilt from sprint dates
  and each issue's Sprint and estimate field changelog. Measured: Sprint
  changelog items carry sprint IDs in `from` and `to`.
  `POST /rest/api/3/changelog/bulkfetch` takes up to 1000 issues and 10
  field IDs.

Fields and search:

- Create metadata is `GET /issue/createmeta/{project}/issuetypes` and
  `.../issuetypes/{issueTypeId}`; edit metadata is
  `GET /issue/{key}/editmeta`. Each field has `required`, `schema`,
  `allowedValues` and `operations`. The option-listing endpoint needs
  Administer Jira, so a normal user's options come from `allowedValues`.
- Transitions with their screen fields come from
  `GET /issue/{key}/transitions?expand=transitions.fields`.
- A rejected write returns `errorMessages` plus an `errors` map of field to
  message. Field errors are kept per field.
- Rich text in v3 is Atlassian Document Format.
- Search is `/rest/api/3/search/jql`: bounded queries only, token paging, no
  total. `POST /issue/bulkfetch` returns up to 100 issues, or 1000 with
  explicit fields.
- JQL date literals have minute precision and are read in the user's time
  zone
  ([JQL fields](https://support.atlassian.com/jira-software-cloud/docs/jql-fields/)).
  There is no "changed since" feed, and webhooks need a Connect or OAuth app
  and a public HTTPS URL
  ([webhooks](https://developer.atlassian.com/cloud/jira/platform/webhooks/)).

Other:

- Team membership is read through the Teams REST API with an API token
  ([Teams REST API](https://developer.atlassian.com/platform/teams/components/team-public-rest-api/));
  the issue Team field is documented
  [here](https://developer.atlassian.com/platform/teams/components/team-field-in-jira-rest-api/).
- The branch and pull request data in Jira's development panel has no public
  read API
  ([staff reply](https://community.developer.atlassian.com/t/rest-graphql-api-for-issue-pull-requests/56965)).
  Reviewer and merge data comes from this app's forge integration, matched
  by ticket key. Remote issue links can be read and written with a token.
  Measured: the web UI's own endpoints answer a classic token.
  `GET /rest/dev-status/latest/issue/summary?issueId=` returns counts per
  instance type, and
  `.../issue/detail?issueId=&applicationType=<instance type>&dataType=pullrequest`
  or `branch` returns pull requests (author, reviewers with approval,
  source and destination branch, status, URL, last update) and branches
  (last commit, repository). They are undocumented and can change without
  notice.
- A forge can be searched for a ticket key. GitLab's merge request list
  takes `search` with `in=title,description`, and `source_branch`
  ([merge requests API](https://docs.gitlab.com/17.9/api/merge_requests)).
  GitHub issue search takes `head:`, `in:title` and `type:pr`
  ([searching issues and pull requests](https://docs.github.com/en/search-github/searching-on-github/searching-issues-and-pull-requests)).
- Measured: `GET /rest/api/3/screens` is refused for a user who is not a
  Jira administrator, so the tab grouping of fields on a ticket screen
  cannot be read with a normal user's token.
- An issue's type is changed by `PUT /issue/{key}` with a new `issuetype`.
  Users report it works when both types share a workflow and is refused
  between a subtask and a standard type or when the target workflow lacks
  the current status
  ([developer thread](https://community.developer.atlassian.com/t/unable-to-change-issue-type-via-rest-api/64003),
  [community thread](https://community.atlassian.com/t5/Jira-questions/Update-both-issue-type-and-fields-for-Jira-issue/qaq-p/1663305)).
  Not measured.
- Measured on 2026-11-16, reads only. A Jira Service Management comment
  that is internal reads `jsdPublic: false` and carries the comment
  property `sd.public.comment` with value `{"internal": true}`. A
  customer-visible one reads `jsdPublic: true` and carries
  `sd.allow.public.comment` with value `{"allow": true}` instead. The REST
  spec documents neither property.
- Measured the same day. `GET /rest/api/3/attachment/content/{id}`
  answers 303 with a `Location` on another host whose path is
  `/file/{media id}/binary` and whose query carries a signed token. With
  `redirect=false` the same path answers 200 with the file from the Jira
  site itself.
- `POST /issue/bulkfetch` is documented to take 1000 issues when `fields`
  names at least one field and at most 100, none of them a wildcard or
  one that "returns multiple values (for example `comment`, `worklog`, or
  `attachment`)", and `expand` asks for nothing heavy; 100 otherwise,
  with a 400 for more. It is documented to find an issue by an old key
  and by a key in another case. Measured 2026-11-20: 101 issues with the
  default fields and 101 with `comment` among the fields were refused;
  250 issues with summary, status, issue type, priority, assignee, labels,
  parent, project, created, updated, resolution date and the sprint field
  returned all 250 (85 with labels, 105 with sprints, 912 KB), so labels
  and sprints do not count as multiple values. A key that names nothing,
  sent beside two IDs, was left out with no `issueErrors` entry.
- Measured the same day. `POST /rest/api/3/changelog/bulkfetch` gives each
  history's `created` as a number of epoch milliseconds (13 digits) and the
  issue as a string `issueId`, with no key. The spec declares `created` a
  string and its example shows 10 digits. `GET /issue/{key}/changelog`
  gives `created` as an ISO 8601 string with milliseconds and an offset.
- Measured on 2026-11-19, reads only, over changelogs expanded on
  `GET /rest/api/3/search/jql`. Where a changelog item holds its values
  depends on the field. A custom number field (story points): `from` and
  `to` are null and the numbers are in `fromString` and `toString` (27 of
  27 items); a first value has `fromString` null (22 of 22) and a cleared
  one has `toString` as the empty string (1 of 1). `timeoriginalestimate` (`fieldtype` `jira`): `from` and `to`
  hold seconds as digits, equal to `toString` (3 of 3). `Sprint`
  (`fieldtype` `custom`): `from` and `to` hold sprint IDs separated by
  ", ", and no sprint is the empty string, never null (58 of 58).
  `status`: `to` is the status ID as digits (168 of 168). `assignee`:
  `from` or `to` is set (70 of 70). No sprint is the empty string in
  `fromString` and `toString` too (25 of 25). `Link`: no `fieldId`,
  `fieldtype` `jira`, the other issue's key in `to` when added and in
  `from` when removed, the other side null (96 items). `Rank`: a custom
  field, `from`, `to` and `fromString` empty strings, text in `toString`
  (32). `Attachment`: `fieldId` `attachment`, the attachment ID as digits
  in `to` when added and in `from` when removed, the other side null
  (101). A parent change: `field` `IssueParentAssociation`, no `fieldId`,
  issue IDs as digits in `to`, and in `from` when there was a parent, else
  null (17). `resolution`: `from` null, `to` the resolution ID as digits
  (11). `labels`, `description` and `summary`: `from` and `to` null, the
  text in `fromString` and `toString`; no labels before is the empty
  string (21, 115, 13). `priority` and `issuetype`: IDs as digits in both
  (9, 6). `timeestimate`: seconds as digits, `from` null when unset (511).
  `Fix Version` (`fieldId` `fixVersions`) and `Component` (`components`):
  the ID as digits on the side that has a value, null on the other.
- Measured the same day. `GET /rest/api/3/search/jql` with no `ORDER BY`
  returned 50 issues in neither ID nor creation order, either direction.
  With `maxResults=5000` it returned 100 issues when `fields=summary` and
  5000 when `fields=id`. `timetracking.originalEstimate` is written in
  weeks, days and hours (`w`, `d`, `h` in 69 of 100 issues; whole hours as
  `Nh`); no issue had a mixed hours and minutes estimate.
- Measured the same day. A search for an issue key, a project
  or an assignee that does not exist answered 200 with no issues and no
  `warnings` member; the spec documents `warnings` as experimental and
  possibly absent.
- Measured on 2026-11-19, reads only, through `GET /rest/api/3/search/jql`.
  A date-time field takes epoch milliseconds as a bare number
  (`updated >= 1763590000000`) and compares at millisecond precision with
  no time zone: bounded at one issue's exact update time, `>=` returned
  that issue first and `>` the next one. Atlassian documents only the
  text date formats, which are read in the user's zone to the minute.
  `ORDER BY updated ASC` sorted 100 issues at millisecond precision, and
  `ORDER BY updated ASC, id ASC` is accepted. A key or ID in `key in (...)`
  or `id in (...)` that names no issue is left out of a 200 answer with no
  warning. The board issue list (`/rest/software/1.0/board/{id}/issue`)
  reads the same JQL: the millisecond literal, the two-key order, and
  `updated > T OR updated >= T AND id > N` to resume after one row, with
  or without parentheses around the `AND`. It takes `id in (...)` of 100
  IDs and returns those on the board; the ID of an issue that is not on
  the board is left out with a 200.
- Measured on 2026-11-19, read-only: `GET /rest/api/3/issue/{A}/comment/{id}`
  and `GET /rest/api/3/issue/{A}/worklog/{id}` return the record when it
  is A's and answer not found when it is another issue's. Whether the
  matching writes refuse the same way is not measured.
- Measured on 2026-11-20, reads only, to size requests. The board issue
  list (`/rest/software/1.0/board/{id}/issue`) is not clamped the way
  search is: `maxResults=5000` returned every issue of 12 boards in one
  page (the largest 2,687) with `fields=id`, and also with ten card fields,
  where the default page is 50. One reading of the largest board with ten
  card fields: 100 issues 0.50 s and 214 KB of JSON, 250 in 0.66 s
  (608 KB), 500 in 0.99 s (1.27 MB), 1,000 in 1.72 s (2.53 MB), all 2,687
  in 3.69 s (6.4 MB, 231 KB compressed). `/rest/api/3/search/jql` with the
  same fields and `maxResults=5000` returned 100.
- Measured the same day. A board configuration's `location` has `id`,
  `key`, `name`, `self` and `type`; an entry of the board list has
  `projectId`, `projectKey` and the display members instead.
  `GET .../board/{id}/sprint?state=active,future` returns only sprints in
  those states.
- Measured the same day. `GET /issue/{id}?fields=*all,-comment` leaves the
  comments out (187 KB of one issue's 281 KB). `expand=editmeta,transitions,changelog`
  on the same read embeds all three; `expand=transitions.fields` adds each
  transition's screen fields. The embedded changelog is newest first, 100
  entries at most, with `total` (222 on that issue); `GET /issue/{id}/changelog`
  is oldest first. `GET /issue/{id}/comment?orderBy=-created` is newest
  first, and `maxResults=100` is honored there. A changelog expanded on a
  search is cut short the same way (39 of 100 issues had more entries than
  were embedded).
- Measured the same day. A search with `fields=worklog` embeds each
  issue's first 20 worklogs with `total` and `maxResults: 20` (4 of 100
  issues with the user's time in 60 days had more than 20). An embedded
  entry has `id`, `issueId`, `author.accountId`, `started`,
  `timeSpentSeconds`, `comment`, `created` and `updated`.
- Measured the same day on one issue whose key had changed. JQL
  `key = <old key>` and `key in (<old key>)` return the issue, and
  `GET /issue/<old key>` answers 200 with the new key.
- Measured the same day. A 1.98 MB search answer with descriptions and
  comments (259 KB compressed) held no `\u` escape: `<`, `>` and `&` are
  sent as themselves and text outside ASCII as UTF-8.
- Measured the same day, reads only, on what moves an issue's `updated`.
  Of 80 issues read with all their comments, none had `updated` earlier
  than its newest comment, and 7 had it within a second of that comment.
  Of 84 issues read with their changelog, none had `updated` earlier than
  the newest entry; where that entry was a link (6 issues), an attachment
  (1) or a worklog (33), `updated` was within a second of it in 6, 1 and
  32. A comment, a link, an attachment and a worklog each move `updated`.
- Measured the same day on one throwaway issue, deleted after. Adding a
  comment, editing it and deleting it each moved `updated` (the delete by
  the 4 seconds since the edit), and it did not move again in the 5
  seconds after. After each of the three, `POST /issue/bulkfetch` with
  `fields=updated` returned the same `updated` as `GET /issue/{id}` at
  once. Not measured: whether removing a link moves it; a
  [community report](https://community.atlassian.com/forums/Jira-questions/Changing-Links-without-changing-the-updated-date-when-deleting/qaq-p/2810120)
  says it does.
- Measured on 2026-11-20 on one throwaway issue, deleted after, on how
  soon a list read shows a write. A new issue was returned by
  `GET /rest/api/3/search/jql` 0.6 s after its create answered. After each
  of five summary edits 4 s apart, the search and the board issue list
  (`/rest/software/1.0/board/{id}/issue`), each asked for the issue with
  `updated >=` its new update time, returned it between 0.5 and 2.1 s
  after the edit answered. Those times include about 0.6 s of two
  requests, so a list read showed an edit within about 2 seconds.
- Measured the same day, on who an answer was made for. Every answer to
  a request with a valid token carried `X-AAccountId`, equal to
  `myself.accountId`: a search, a board configuration, a board issue list
  and a 404 for an issue that does not exist. No answer to a request with
  no credential carried it: a search answered 200 with no issues and no
  login-reason header, an issue read 404, `myself` and a board issue list
  401. So an empty 200 or a 404 without the header was answered to nobody,
  and one with it was answered to that account. Answers to a wrong or
  revoked token were not measured.
- Measured the same day. A sprint ID that names nothing in JQL answers 400
  on the board issue list and 200 with no issues on `search/jql`. 500 IDs
  in one `id in (...)` clause (3.3 KB) returned all 500 on both. Of 100
  issues in sprints, the 3 whose newest change was a sprint change alone
  had `updated` within two seconds of it, so a sprint change moves
  `updated`. A board configuration's `location.id` is a string. An
  embedded changelog with fewer than 100 entries reports `maxResults`
  equal to the number embedded (20 of 20).
- Mentions of the user are found with `comment ~ currentUser()`
  ([JRACLOUD-27594](https://jira.atlassian.com/browse/JRACLOUD-27594)).
  Neither spec has a notification inbox endpoint.
- Data Center: no new customers since 2026-03-30, read-only at end of life
  on 2029-03-28
  ([Atlassian](https://www.atlassian.com/licensing/data-center-end-of-life)).

## Linear

Source for schema facts: the
[public GraphQL schema](https://raw.githubusercontent.com/linear/linear/master/packages/sdk/src/schema.graphql).

- A personal API key is sent as `Authorization: <key>` and can be limited
  to read or write and to teams
  ([GraphQL](https://linear.app/developers/graphql),
  [API and webhooks](https://linear.app/docs/api-and-webhooks)). OAuth with
  PKCE works without a client secret
  ([OAuth](https://linear.app/developers/oauth-2-0-authentication)).
- Limits per user per hour: 2,500 requests and 3,000,000 complexity points
  with an API key
  ([rate limiting](https://linear.app/developers/rate-limiting)). Pages are
  Relay cursors, 50 by default and 250 at most
  ([pagination](https://linear.app/developers/pagination)).
- Measured on 2026-11-16 against a blank workspace with a personal API
  key. Every answer carries `x-complexity` (the cost of that request) and,
  for each of requests and complexity, `x-ratelimit-<kind>-limit`,
  `-remaining` and `-reset`, the reset a Unix time in milliseconds. A
  made-up key and a missing one both answer HTTP 401 with a GraphQL error
  whose `extensions.code` is `AUTHENTICATION_ERROR`. A valid key sent with a
  `Bearer` prefix answers HTTP 400 with `extensions.code` `INPUT_ERROR`, so
  a 400 is not proof that the query is wrong.
- Measured the same day with the client's live check
  (`internal/tracker/linear/live_test.go`). An issue ID that does not
  exist answers with `extensions.code` `INPUT_ERROR` and a message that
  starts `Entity not found`; there is no code of its own for it.
  `issues(sort: [{updatedAt: {order: Ascending}}])` returns issues oldest
  update first, which the polling walk relies on. A new issue gets a
  `sortOrder` below every existing one (three created in a row: -10,
  -1080, -1986), and `issueUpdate` with a `sortOrder` between two
  neighbors places it between them in the manual order.
- Measured on 2026-11-19 with the live history checks
  (`live_fold_test.go`, `live_bisect_test.go`). A change made within a
  window after an issue's creation is folded into the creation and never
  becomes a `history` entry: priority changes at +2m30s and earlier were
  never recorded and those at +3m and later were, and an assignee change at
  +5s and a state change at +3m45s agree. An entry can first appear a few
  seconds after its change (5 s seen). Three edits 5 s apart, well after
  creation, were three entries that did not change afterwards. A page of
  issues is charged by what it returns, not by `first`: `first: 100` holding
  two issues cost 9 points where the documented rule gives 1,201, and the
  histories of two issues in one request cost 324. Archived and trashed
  issues are left out of a poll and a list unless `includeArchived` is set,
  and then carry `archivedAt`, with `trashed` true for the trashed one.
  `dueDate: null` clears a due date. A team key matches with
  `eqIgnoreCase`. A new title was found by `searchIssues` two seconds
  after its create.
  A page's end cursor continues the walk from a second client, and an
  issue updated after the cursor was taken arrives on that continuation.
  Reported cost per request: a mutation 2 to 8 points; one page of three
  issues 9; the catalog reads (teams, states, users, labels, projects,
  cycles) 310 to 730 each; one issue with its comments, attachments and
  relations 286; an issue's history 641.
- Measured on 2026-11-19 with `live_scope_test.go`, on a workspace with
  one team, three labels, no projects and cycles off. Which connections
  are charged by their results and which by `first`: the `issues`
  connection is charged by what it returns (IDs and sort orders of three
  issues cost 4 at `first: 50` and at `first: 250`), and so is a `labels`
  list nested in it (two issues cost 3 without labels and 6 with, at any
  outer or inner page size and with one label or three). The catalog
  connections are charged by `first`: labels by ID cost 4 for one ID and
  10 for three, teams by key 9 for two keys and one match, and a cycles
  read that returned nothing 441 at `first: 50` and 176 at `first: 20`.
  The largest page is 250, outer or nested: `first: 251` answers
  `INVALID_INPUT` with "first must not be greater than 250". `comment`, `issueRelation`, `cycle` and
  `team` for an ID that names nothing answer `INPUT_ERROR` with "Could not
  find referenced" and the type's name, at that field. A sub-issue created
  naming only its parent took none of the parent's priority, assignee, due
  date or labels. Not measured there: project and cycle inheritance, what
  a cycle filter selects, and whether `issueUpdate` refuses another team's
  status, cycle, label or project.
- Webhooks need a public HTTPS URL
  ([webhooks](https://linear.app/developers/webhooks)). Polling filters and
  orders by `updatedAt`. Polling cannot see hard deletes.
- There are no custom fields
  ([Jira import](https://linear.app/docs/jira-to-linear)), no time tracking
  and no planning poker (schema).
- Cycles are generated from team settings. `cycleCreate` always returns an
  error; `cycleStartUpcomingCycleToday` starts the next cycle and completes
  the current one. Unfinished issues roll over automatically
  ([cycles](https://linear.app/docs/use-cycles)).
- Per-cycle scope and completion history is team-wide, one entry per day,
  and is not a commitment snapshot. A "mine" split is computed from issues
  and `Issue.history`. `Issue.stateHistory` gives status intervals.
- Manual order is `sortOrder`, set through `issueUpdate` to a value between
  the neighbors. `issueBatchUpdate` applies one input to up to 50 issues, so
  it cannot set distinct orders.
- Estimates are per team on one of four fixed scales
  ([issue properties](https://linear.app/docs/issue-property)). Workflow
  states have fixed types (triage, backlog, unstarted, started, completed,
  canceled, duplicate).
- Descriptions and comments are Markdown. Pull requests appear as
  `Attachment` records, and a client can create such links.
- Measured on 2026-11-19, reads only. `issue(id:)` for an ID that does not
  exist answers HTTP 200 with `data` null and one error, message
  `Entity not found: Issue`, `extensions.code` `INPUT_ERROR`, path
  `["issue"]`. `teams(first: 251)` answers HTTP 200 with `data` null and
  one error, message `Argument Validation Error`, code `INVALID_INPUT`,
  path `["teams"]`. `issueLabels(filter: {team: {id: {eq: <team>}}})`
  returned none of the workspace's three labels, which belong to no team;
  `filter: {team: {null: true}}` returned all three.
- Measured on 2026-11-20 to size requests. A `history` page nested in an
  `issues` page is charged by its own `first` and not by how many issues
  carry it: 203 points at `first: 50`, 403 at 100 and 1,003 at 250, the
  same for 1, 20 and 42 issues. A page of 42 issues with card fields and
  nested labels cost 8 at `first: 100` and at `first: 250`. An issue
  filter takes `or: [...]`. `issues(filter: {id: {in: [...]}})` leaves an
  ID that names nothing out with no error, where one aliased `issue(id:)`
  that names nothing makes the whole `data` null (the error's `path` is
  the alias).
- Measured the same day with throwaway issues. Three aliased `issueUpdate`
  fields in one document each set their own `sortOrder` and answered under
  their alias, 7 points. With the middle one naming a missing issue the
  answer was `data` null and one `INPUT_ERROR` at that alias, and a read
  right after showed neither of the other two applied.
  `issueBatchUpdate` on three issues answered `success` with the three
  issues, 3 points; with one missing ID it answered `INPUT_ERROR`
  "id not found" and changed none; with 51 IDs, `INVALID_INPUT`. One
  failing case each, so a failed document's effect is read back, not
  assumed.
- Measured the same day with throwaway issues. A page cursor follows its
  issue: it means "after where that issue is now", not the place the issue
  had when the page was read. An oldest-update-first walk of three issues
  read one, that issue was then updated (moving it to the end), and the
  continuation from its cursor returned no issues and no next page, with
  two issues never delivered. A walk ordered by a value that can change
  therefore cannot page by cursor. `updatedAt` is serialized with
  millisecond precision; filtered by an issue's own `updatedAt`, `gt`
  leaves the issue out and `gte` includes it. `issueBatchUpdate` on three
  issues gave them three distinct `updatedAt` values. A filter takes
  `updatedAt: {gte: ...}` together with `id: {nin: [...]}` under an
  oldest-update-first sort and leaves the named issues out (48 issues, 46
  with two named), and takes that floor with
  `or: [{updatedAt: {gt: ...}}, {id: {nin: [...]}}]` beside it. Under
  `sort: [{updatedAt: {order: Descending}}]` the mirrored filter,
  `updatedAt: {lte: ...}` with
  `or: [{updatedAt: {lt: ...}}, {id: {nin: [...]}}]`, continued a walk of
  six issues exactly: after two were delivered, the next page was the
  other four in order. `updatedAt: {eq: ...}` with an issue's own
  millisecond `updatedAt` returned that issue alone under a
  creation-ordered sort. That `eq` beside `id: {nin: [...]}` was accepted,
  returned the issue when another was named and nothing when it was, and
  took `after` with the first page's cursor (no more issues, no error).
  Two issues sharing one `updatedAt` could not be produced, so a cursor
  page inside such a run is not measured.
- Measured the same day. A catalog nested under an issue is charged by
  its `first` whatever the issue page's `first`:
  `issues(first: F) { nodes { id team { members(first: N) { nodes { id } } } } }`
  cost 4 points at N=1, 14 at N=50 and 54 at N=250, the same for F of 1,
  10 and 50. Without the catalog, `issues(first: 10)` with the team's ID
  and name, the assignee's and the state's IDs cost 4, and with only
  issue IDs 2.
- Measured the same day with throwaway issues. A walk ordered by creation
  pages by cursor safely: with six open issues read two at a time, the
  continuation was right after the cursor's issue was completed (leaving
  the filter), after the next cursor's issue was trashed, and after an
  earlier issue left the filter. In a document of several aliased
  `issue(id:)` fields that name nothing, the answer reports only the first
  as an error, so the missing ones cannot be told apart in one request.
- Measured the same day with one throwaway issue. A comment does not move
  the issue's `updatedAt`: it read the same 4 and 24 seconds after
  `commentCreate` as before it, and `issues(filter: {updatedAt: {gte:
  <the comment's createdAt>}})` did not list the issue. An issue's
  `updatedAt` therefore does not say whether its comments changed.
- Measured the same day with ten throwaway issues, three of them
  commented. The ticket view's selection (the issue with its children,
  comments, attachments, relations both ways, history and its team's
  states, at the page sizes of the single-issue read) cost 621 points
  through `issue(id:)` and 622 through
  `issues(filter: {id: {in: [...]}})` for 1, 2, 5 and 10 issues alike, at
  `first` equal to the count and at `first: 50`; each issue came back with
  its own comments and states. An ID that names nothing was left out with
  no error. Reading N issues in full in one request therefore costs the
  request and the points of one.

## Tempo

Source: the [Tempo OpenAPI spec](https://apidocs.tempo.io/tempo-openapi.yaml),
API v4.

- A user creates an API token in Tempo settings with a chosen expiry
  (default 30 days) and scopes; it is shown once and sent as a Bearer token
  ([REST API integrations](https://help.tempo.io/timesheets/latest/using-rest-api-integrations)).
  OAuth requires a client secret and an admin-registered app
  ([OAuth](https://help.tempo.io/timesheets/latest/using-oauth-2-0-authentication)).
- The base URL is regional (`api.tempo.io`, `api.eu.tempo.io`,
  `api.us.tempo.io`); the wrong one returns an authorization error.
- Worklogs take the numeric issue ID, not the key, and identify the author
  by Atlassian account ID
  ([v4 comparison](https://help.tempo.io/planner/latest/tempo-api-version-4-0-vs-version-3-0-a-comparison)).
  `PUT /4/worklogs/{id}` replaces the whole record.
- `billableSeconds` is optional on create and update, and the spec names no
  default. `atl` records that an update without it bills the whole time
  spent, which would make a non-billable worklog billable; this was not
  measured again here. Tempo's update example bills 7200 seconds on 3600
  spent, so billable time is not capped at the time spent
  ([worklog migration](https://help.tempo.io/cloudmigration/latest/worklog-rest-apis-for-jira-cloud)).
- Required hours per day come from `GET /4/user-schedule`; approval status
  and submit from `/4/timesheet-approvals/user/{accountId}`; work attributes
  and their allowed values from `/4/work-attributes`.
- The limit is 5 requests per second per IP, shared by every integration on
  that address
  ([rate limits](https://help.tempo.io/kb/latest/tempo-api-rate-limits-and-429-errors)).
- Tempo and Jira worklogs sync both ways, and the full record exists only in
  Tempo
  ([worklog sync](https://help.tempo.io/kb/latest/tempo-and-jira-worklog-sync-on-cloud)).
  Sites without Tempo use `rest/api/3/issue/{key}/worklog`.

## Dashboards and SQLite

- The app's Content-Security-Policy has `script-src 'self'` with no
  `unsafe-eval`, no `frame-src`, and `X-Frame-Options: DENY`
  (`internal/transport/wireheaders.go`). Charts are therefore app
  components, not framed or evaluated content. No directive governs a
  sandboxed frame navigating itself, which is a data exfiltration path
  ([CSP3](https://www.w3.org/TR/CSP3/)).
- The frontend has no chart library. LayerChart 2.x declares the app's
  Svelte version as its peer
  ([registry](https://registry.npmjs.org/layerchart/2.10.1)). The library
  choice is not ruled.
- Read-only agent SQL needs the SQLite authorizer, which
  `modernc.org/sqlite` exposes from v1.61.0, with query cancellation fixes
  in v1.61.1
  ([changelog](https://gitlab.com/cznic/sqlite/-/raw/master/CHANGELOG.md)).
  `go.mod` pins v1.60.1. SQLite's guidance for untrusted SQL adds defensive
  mode, lowered limits and an interrupt
  ([security](https://www.sqlite.org/security.html),
  [authorizer](https://www.sqlite.org/c3ref/set_authorizer.html)).
- The widget vocabulary should cover what users build in the trackers: Jira's
  [dashboard gadgets](https://support.atlassian.com/jira-cloud-administration/docs/use-dashboard-gadgets/)
  and Linear's [Insights](https://linear.app/docs/insights). Jira's velocity
  chart measures in points, time, issue count or any numeric field
  ([velocity chart](https://support.atlassian.com/jira-software-cloud/docs/view-and-understand-the-velocity-chart/)).

## Planning poker add-on

Applies only to a site with Easy Agile TeamRhythm installed
([Marketplace record](https://marketplace.atlassian.com/rest/2/addons/com.kretar.jira.plugin.user-story-map/versions/latest?hosting=cloud)).
Votes are stored on the vendor's servers, not on the issue, and the vendor
documents no API
([app data storage](https://help.easyagile.com/easy-agile-teamrhythm/app-data-storage)).
Jira has no estimation session of its own
([estimate an issue](https://support.atlassian.com/jira-software-cloud/docs/estimate-an-issue/)).

The add-on makes the calls the vendor's issue panel makes. Everything in
this section was measured on 2026-11-15 with a classic API token and no
browser session. Both Atlassian endpoints are undocumented, and Atlassian
does not support undocumented APIs
([help and feedback](https://developer.atlassian.com/developer-guide/help-and-feedback/)).

1. **Find the extension.** `POST /gateway/api/graphql`, query
   `extensionContexts(contextIds: ["ari:cloud:jira::site/{cloudId}"])` with
   `extensionsByType(type: "jira:issuePanel")`. The poker panel is the entry
   whose `key` is `planning-poker-forge-panel`. Take its `id`, `appId`,
   `environmentId`, `installationId` and `appVersion` from the response.
   Absence means the add-on is off for that site.
2. **Get a context token.** `POST /rest/internal/2/forge/context/token` with
   `{extension, extensionData, localId, useWorkspaceAri: true}`.
   `extension` is the listing entry (`id`, `type`, `key`, `appId`,
   `environmentId`, `installationId`, `appVersion`). `extensionData` is
   `{type: "jira:issuePanel", issue: {key, id, type, typeId}, project: {id,
   key, type}}`. `localId` is the extension `id`. The response is `{token,
   expiresAt, expiresIn, extensionId}`; `expiresIn` was about 899,000, so
   roughly 15 minutes if milliseconds (units not verified). The gateway's
   own signer, `forgeApp_signForgeContextTokens`, refuses API tokens.
3. **Invoke.** `POST /gateway/api/graphql`, mutation `invokeExtension` with
   input `{contextIds, extensionId, entryPoint: "resolver", payload}`.
   `contextIds` must equal the `contextIds` claim inside the token (a
   workspace ARI), or the call fails with `FCT_VALIDATION_CLAIM_MISMATCH`.
   `payload` is `{call: {functionKey: "requestConnectBackend", payload:
   {path, method, headers, body}}, context: {cloudId, localId, moduleKey,
   extension: extensionData}, contextToken}`. Without `contextToken` the
   call fails with "Missing Forge Context Token". The backend's HTTP status
   and body come back in `response.body`.

Routes, each under the path prefix `/forge/rest/eausm/latest/`:

| Action | Method and path | Body | Result |
|---|---|---|---|
| Read | `GET planningPoker/{issueId}` | none | `{issueId, isRevealed, votes: [{id, issueId, userId, vote, createdAt}]}` |
| Vote or change vote | `PUT planningPoker/vote` | `{issueId, vote}` | the vote record |
| Reveal | `PUT planningPoker/reveal/{issueId}` | none | the read shape with `isRevealed: true` |
| Clear all votes | `DELETE planningPoker/votes/{issueId}` | none | 204 |

`issueId` is the numeric Jira issue ID and `userId` is an Atlassian account
ID. The calls add no changelog entry and no entity property to the issue. A
vote is a number; the vendor documents 0 to 1000, and a vote can be changed
but not deleted on its own
([planning poker](https://help.easyagile.com/easy-agile-teamrhythm/planning-poker-1)).
The panel also subscribes to a Forge realtime channel; the add-on polls
while a ticket is open.

Not verified: whether the read hides other voters' values before reveal. The
test had one voter. The client hides other voters' values until
`isRevealed` is true, whatever the response contains.

The agreed estimate is written by the client to the board's estimation
field through the normal Jira API. The poker backend does not write it.
