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
- Not measured. An ADF `media` node names its file by the media service ID,
  which no field of an issue's attachment record carries, and by its `alt`,
  which the Jira editor fills with the file's name. The ticket screen shows
  a node as the attachment of that name only when exactly one attachment of
  the issue has it, and as the name alone otherwise
  (`frontend/src/lib/stores/trackerTicketModel.ts#mediaAttachment`).
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
- Measured the same day on one throwaway issue, deleted after, on reads
  that name the issue by ID right after a write. After each of six summary
  edits 4 s apart, the first answer of the board issue list and of the
  search, each with `jql=id in (<id>)`, and of `POST /issue/bulkfetch`
  carried the new summary (answers 0.2 to 0.4 s after the edit answered).
  So the lag above is in matching `updated >=`, not in the field values of
  an issue a list returns. After the create, the bulk fetch returned the
  issue in its first answer (0.3 s) and the search by ID did not return it
  until 23 s after the create answered: a new issue is found by ID through
  a list only once Jira has indexed it, which took 0.6 s in the earlier
  run and 23 s in this one.
- Measured the same day on one throwaway issue, deleted after, on
  `POST /rest/api/3/jql/match` (body `{"issueIds":[...],"jqls":[...]}`, up
  to 50 issue IDs and 10 queries, answered as
  `{"matches":[{"matchedIssues":[...],"errors":[...]}]}` in the queries'
  order; no permission beyond Browse projects, OAuth scope
  `read:jira-work`). Its first answer, 0.3 s after the create answered,
  was right: both of the project's board filters and `id = <new>` matched
  the new issue, and a query naming a label it lacked did not. The search
  by ID and one board's list by ID still missed it at 0.7 to 0.8 s and
  showed it by 1.3 and 1.6 s. After an edit that added the label the
  label query matched in the match's first answer (0.3 s), and after the
  label was removed it did not (0.4 s). So the match does not wait for
  the search index in either direction. A nonexistent issue ID answered
  200, matched by no query, with no error.
- Measured the same day, read-only, on two boards of one project, one
  kanban with a sub-filter and one scrum, sharing a saved filter, on what
  a board's issue list holds. Neither list held any of the filter's issues
  in a status no column maps (68 on the kanban board, 34 on the scrum
  board). The kanban list's total equaled the count of the filter's issues
  in its mapped statuses (931). The scrum list held none of the 47 epics
  in its mapped statuses; the kanban list held all 47. Every issue on the
  scrum list was at hierarchy level 0, and neither board had sub-tasks.
  The kanban sub-filter left out no issue, so whether a list applies it
  is not known. `jql/match` accepted the membership query built from each
  board's configuration (saved filter, mapped statuses,
  `hierarchyLevel != 1` on the scrum board, the sub-filter as a further
  query on the kanban board) with no error, and agreed with the board's
  list by ID on six samples.
- Measured the same day, read-only: the same 25 issues read from a board's
  issue list and from `POST /issue/bulkfetch` differed only in `self` URLs
  (of the issue type, priority, status, status category, assignee, parent
  and project) and the project's `avatarUrls`. The rank and every other
  value were equal.
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
  `team` for an ID that names nothing answer `INPUT_ERROR` at that field
  (the message is in the 2026-11-20 entry on missing entities below; the
  client's `GraphQLError.Message` shows the presentable one). A sub-issue created
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
- Measured the same day, missing entities, one run each with a fresh UUID
  that names nothing. A read by ID (`issue`, `comment`, `issueRelation`,
  `cycle`, `team`, `user`, `project`, `workflowState`, `issueLabel`)
  answers HTTP 200 with `data` null and one `INPUT_ERROR` at that field:
  message `Entity not found: <Type>`, `userPresentableMessage`
  `Could not find referenced <Type>.`. `issueUpdate` of an existing issue
  whose input names such an ID answers `data` null and one `INPUT_ERROR`
  at `["issueUpdate"]`: for `stateId`, `assigneeId`, `labelIds`,
  `cycleId`, `projectId` and `parentId` the message is
  `Entity not found in validateAccess: <member>` with the presentable
  message "An item needed to save your changes is unavailable."; for
  `teamId` it is `Entity not found: Team`. Not measured: `issueCreate`,
  the label delta members, and an ID that exists in another team.
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
- Measured 2026-11-26 on two throwaway issues, deleted after, one trial
  of each `issueUpdate`, on what moves `updatedAt`. A `title`, an
  `assigneeId` and a `priority` change each moved it. A `stateId` change
  moved it both times it was made (backlog to started, then started to
  completed), with the state read back as the target each time. Two reads
  with no write between returned the same `updatedAt`. Not measured: a
  `cycleId` change (the workspace had no cycle); the tracker tools'
  check after an approval compares a ticket's cycle by value, so it does
  not depend on one.
- Measured the same day on one throwaway issue, deleted after, on a team
  whose `issueEstimationType` is `notUsed`. `issueUpdate` with
  `estimate: 3` (from none), then `5`, then `null` each answered success,
  the estimate read back as the value written, and `updatedAt` moved each
  time. Not measured: a team with estimation on.
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
- Measured the same day on one throwaway issue, deleted after, on reads
  right after a write. After `issueCreate` and after each of five
  `issueUpdate` title edits 2 s apart, the first
  `issues(filter: {id: {in: [<id>]}})` and the first
  `issues(orderBy: updatedAt, filter: {team: ..., updatedAt: {gte: <the
  write's updatedAt>}})` returned the issue with its new title. A list
  read shows a create and an edit at once.
- Measured 2026-11-20 on throwaway issues, on what a delete's payload
  holds. `commentDelete` and `issueRelationDelete` answered
  `success: true` with `entityId` equal to the deleted ID; a second
  `commentDelete` of the same comment answered an error.
  `issueDelete` answered `success: true` with `entity` the issue (its
  `id`, `trashed: true`, `archivedAt` set), and the same for an issue
  already in the trash. `issueDelete` with `permanentlyDelete: true` was
  refused (`forbidden`) for a key without the workspace permission, so its
  payload is unmeasured.
- Measured on 2026-11-21 against a blank workspace with a personal API
  key, on one throwaway issue. `issueUpdate` of an existing issue whose
  input names a fresh UUID for `stateId`, `assigneeId`, `cycleId`,
  `projectId`, `parentId` or `labelIds` answered `data` null and one error
  whose `extensions` carried `code` `INPUT_ERROR`, `type` `invalid input`
  and `userError` true (keys: `code`, `meta`, `statusCode`, `type`,
  `userError`, `userPresentableMessage`). `issueDelete` with
  `permanentlyDelete: true` answered "Invalid scope: `admin` required" for
  this key; without it the issue was trashed.
- <a id="linear-history-names"></a>Measured the same day, reads only.
  `issues(first: N, includeArchived: true)` with `history(first: 100)`
  selecting the scalar history fields cost 653 points at N=1 and N=10;
  with the twelve nested objects (`fromAssignee`, `toAssignee`,
  `fromCycle`, `toCycle`, `fromProject`, `toProject`, `fromTeam`,
  `toTeam`, `fromParent`, `toParent`, `addedLabels`, `removedLabels`, two
  or three scalar fields each) it cost 2,153 at N=1 and N=10. The
  client's own documents: `IssueDetails` cost 2,598 points for 1 issue and
  for 10, an `IssueHistory` page at `first: 100` 2,151, and an
  `IssueChanges` page at `first: 100` 652.
- <a id="linear-trashed-history"></a>Measured the same day with two
  throwaway issues. After the creation fold, one was made the other's parent, then the parent was trashed with
  `issueDelete`. The child's history entry answered
  `toParent { id identifier }` with the parent before the trash and 5 s
  after it. So a nested history entity is still answered for a trashed
  issue; other kinds (an archived label, cycle, project or team, a
  deactivated user) were not measured.

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
- The spec lists `200` for `POST /4/worklogs` and `PUT /4/worklogs/{id}`
  and `204` for `DELETE /4/worklogs/{id}`.
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
  `modernc.org/sqlite` exposes from v1.61.0
  ([changelog](https://gitlab.com/cznic/sqlite/-/raw/master/CHANGELOG.md)).
  `go.mod` pins v1.63.0 (SQLite 3.54.1), the newest tag. A canceled
  context interrupts only a statement's first step: the driver watches the
  context for the duration of `stmt.query`, and `rows.Next` steps without
  an interrupt (see [Canceling agent SQL](#canceling-agent-sql)). SQLite's
  guidance for untrusted SQL adds defensive mode, lowered limits and an
  interrupt
  ([security](https://www.sqlite.org/security.html),
  [authorizer](https://www.sqlite.org/c3ref/set_authorizer.html)).
- The widget vocabulary should cover what users build in the trackers: Jira's
  [dashboard gadgets](https://support.atlassian.com/jira-cloud-administration/docs/use-dashboard-gadgets/)
  and Linear's [Insights](https://linear.app/docs/insights). Jira's velocity
  chart measures in points, time, issue count or any numeric field
  ([velocity chart](https://support.atlassian.com/jira-software-cloud/docs/view-and-understand-the-velocity-chart/)).

### Canceling agent SQL

Measured on v1.63.0 and SQLite 3.54.1. A Go function counted its calls over
a table of 12,000 rows and canceled the query's context from its first call;
each statement ran 3 times.

| Statement | Rows the function read after the cancel |
|---|---|
| `SELECT count(f(x)) FROM t`: the aggregate is the first row | 167, 221, 227 |
| `SELECT 0 UNION ALL SELECT count(f(x)) FROM t`: a cheap row first | 12,000 every time |
| the same, wrapped as below | 294, 113, 81 |
| `SELECT f(x) FROM t`, read row by row | 208, 204, 98 |
| the same, wrapped | 229, 124, 176 |

The connection answered the next query after every interrupt. Read row by
row, `database/sql` stops between rows. Work that one step does after the
first row runs to its end: a later compound arm, or an aggregate fed by a
per-row subquery.

The driver installs its interrupt watcher and progress handler only inside
`stmt.query`, which includes the first `sqlite3_step`, and exports no
interrupt; it owns the connection's one progress-handler slot. So an
evaluator makes SQLite produce the whole result in the first step: it
prepares the agent's statement to learn its column count n, then runs

```sql
WITH r(c1, ..., cn) AS (<agent SQL>)
SELECT json_group_array(json_array(c1, ..., cn)) FROM r
```

and decodes the one JSON value. The column list renames the columns, so
duplicate names do not matter. A row cap goes inside the wrapper
(`SELECT ... LIMIT n` as the CTE body); a `LIMIT` on the outer `SELECT`
caps the one aggregate row, not the rows.

`SQLITE_LIMIT_LENGTH` bounds single values, not the memory the wrapper
uses. With the limit at 1 KiB, a stored value of 2,000 bytes failed the
statement with `SQLITE_TOOBIG` before any row was read. The aggregate
checks the limit only on its finished result and grows without it while it
collects: over 160 values of 400 bytes it read all 160 rows, about 65 KB
of JSON, before it failed.

A cap on the encoded result's size counts, while the rows are collected,
the bytes of each row's encoding as it enters the result, with the bracket
or comma before it and the closing bracket, and evaluates each row once. A
Go function in the wrapper's `WHERE`, such as
`WHERE within_budget(octet_length(json_array(c1, ..., cn)))`, does neither:

- `length()` counts characters. With `length()` in its place and a
  2,048-byte budget, five rows of 400 emoji passed as an 8,026-byte result;
  `octet_length()` failed the statement at the second row. Neither counts
  the brackets and commas.
- SQLite evaluated the agent's column once for the `WHERE` and again for
  the aggregate. Over 20 rows of a function that
  returned 1 and 1,000 bytes in turn, the function ran 40 times, the
  `WHERE` was charged 100 bytes, and the result was 20,101 bytes.

A Go aggregate in place of `json_group_array` does both:

```sql
WITH r(c1, ..., cn) AS (<agent SQL>)
SELECT bounded_rows(c1, ..., cn) FROM r
```

Its step receives the row's SQL values once, encodes them as a JSON array,
and fails once the result so far, the separator, the row and the closing
bracket would pass the budget. With a 2,048-byte budget it returned one
emoji row as a 1,606-byte result and failed at the second row. Over the
alternating function it evaluated each row once and failed at the sixth
row, the first that would pass the budget. It is still one aggregate row,
produced in the first step: a cancel from inside the agent SQL stopped it
after 387, 653 and 682 of 12,000 rows, against 460, 1,496 and 1,246 for
`json_group_array` in the same run. With the process held to one CPU, the
cancel stopped neither form in 5 of 6 runs.

`json_array` takes a BLOB as SQLite JSONB when the bytes parse as JSONB,
and fails the statement otherwise ("JSON cannot hold BLOB values"). So
`x'00'` became `null`, `jsonb('[1,2]')` became the array `[1,2]`, and
`x'00ff'` and `zeroblob(0)` failed. Text that holds JSON stays a string:
`'[1,2]'` and `json('[1,2]')` both became `"[1,2]"`. An evaluator decides
by SQL type, writing each column as
`iif(typeof(ck) = 'blob', json_array(hex(ck)), ck)`. A BLOB then decodes as
a one-element array of its hex digits, which no text, number or `NULL`
decodes to. An evaluator that rejects BLOBs instead fails on the first
such array it decodes.

The aggregate receives a BLOB as Go `[]byte` whatever its bytes hold, so it
encodes BLOBs by SQL type without the `iif`: it wrote `x'00ff'` as
`["00FF"]` and `jsonb('[1,2]')` as `["4B13311332"]`.

### The built-in dashboard

Measured on this branch with modernc.org/sqlite v1.63.0 under WSL2 on a
developer machine, each figure the median and spread of 5 runs. The
board is the store's scale fixture: one board of n issues with 10
changes each (20,000 at 2,000 issues), 100 sprints and 20 people
(`TestDashboardAtScale` in `internal/store`, run with
`AO_TRACKER_SCALE=2000,20000`).

| Path | 2,000 issues | 20,000 issues |
|---|---|---|
| Evaluation, last 5 sprints | 40 ms (33 to 52) | 304 ms (298 to 331) |
| of which the store's fill | 30 ms (27 to 44) | 249 ms (244 to 277) |
| Evaluation, this sprint | 38 ms (36 to 56) | 335 ms (309 to 370) |
| Drill of the stuck tickets, first page of 100 rows | 23 ms (22 to 25) | 146 ms (142 to 158) |
| Answer as JSON | 12,516 bytes | 12,518 bytes |
| Drill page as JSON | 17,924 bytes | 17,928 bytes |
| Go allocations of one evaluation, last 5 sprints | 2.8 MB in 37,639 | 30.2 MB in 351,076 |

A count copies only the relations its metrics read. Each fill
statement timed alone on the same fixture (the median of 5 runs, with a
timing harness kept out of the repository), before that change (every
relation, for every count) and after it:

| Fill | Before, 2,000 | After, 2,000 | Before, 20,000 | After, 20,000 | Rows at 20,000 |
|---|---|---|---|---|---|
| tickets, with their start and finish | 8.5 ms (two fills) | 11.1 ms | 82.4 ms (two fills) | 118.3 ms | 20,000 |
| sprint_tickets | 41.9 ms | 1.8 ms | 566.0 ms | 24.8 ms | 20,000 |
| sprint_finished, closed sprints | 5.4 ms | 4.5 ms | 97.6 ms | 99.9 ms | 19,800 |
| sprint_finished, active sprints | 0.8 ms | 0.8 ms | 9.4 ms | 9.5 ms | 200 |
| people | 8.5 ms | 0.2 ms | 114.6 ms | 1.9 ms | 20 |
| statuses | 0.1 ms | 0.6 ms | 0.1 ms | 4.6 ms | 5 |
| epics, sprints, board, board state | 0.4 ms | 0.3 ms | 1.6 ms | 1.4 ms | |
| reading tickets back into Go | 0.2 ms | 0.1 ms | 1.7 ms | 0.2 ms | |
| **an Overview's copy** | **83.9 ms** | **19.3 ms** | **1,058 ms** | **260.5 ms** | |
| changes, copied only for a metric that reads them | 18.2 ms | 17.6 ms | 184.3 ms | 195.7 ms | 200,000 |
| the people and statuses the changes name | in people | 14.7 ms | in people | 179.3 ms | |

The rest of an Overview's copy at 20,000 issues is the tickets fill,
which reads each ticket's first move to in progress and last move to
done from its stored changes by key (about 36 ms over the two fills it
replaced), and what each closed sprint finished, the pace's
done-at-completion rule the Board's sprint sections share. A Stuck drill
copies the tickets and their statuses, so the tickets fill is most of
its 146 ms.

The fill is still most of an evaluation and grows with the board; the
metrics and the reduction take about 10 ms at 2,000 issues and 55 ms at
20,000. An open dashboard counts again at most once every 5 s while its
board changes, so at 20,000 issues a board that changes constantly
costs about a sixteenth of one core while the screen shows.

While the Dashboards screen shows, the board's subscription stays voted
shown so the copy keeps syncing, and the client reads the board's patch
on each change as the Board screen does. At 2,000 issues (the store's
`TestMirrorAtScale/1x2000`) a patch read costs 62 µs for one changed row
with the totals held, 0.73 ms for 100, and 3.6 to 4.4 ms when the totals
moved; the pump's idle tick costs 129 µs.

A hostile metric's temporary files, measured with
`TestHostileQuerySpill` in `internal/trackerdash` (a cross join of
3,000 changes, three ways, grouped; two runs of the test): the 1 s
deadline stopped it at 1.003 to 1.011 s, by when its temporary files
had reached 62.5 to 66.3 MB, and all were closed after it. The same join
made distinct stopped at the 20,000-row cap in 16 to 29 ms with no
temporary file.
Sorted with a `LIMIT`, SQLite keeps only the top rows and writes none.

The Dashboards screen opened and closed four times in the desktop
harness (`issues-dashboards.spec.ts`, heap snapshots after a forced
collection, compiled code left out): the page held 14,086 KB with the
Board shown, 14,761 to 14,927 KB with the dashboard open, and 14,234 KB
after the fourth close; the pane strip's DOM went from 163 elements to
277 and back to 163.

The Go side holds nothing between calls: each evaluation and drill opens
its session and closes it before it answers, and the screen's only
other Go state is the board's subscription, which the Board screen
shares. Measured with `TestDashboardMemoryAfterClose` in
`internal/store` (20 evaluations alternating the last 5 sprints and
this sprint, and 20 drills, heap read after a forced collection):

| Moment | 2,000 issues | 20,000 issues |
|---|---|---|
| Before the first evaluation | 4.7 MB, 7 goroutines, 15 files | 4.6 MB, 7 goroutines, 15 files |
| After the first | 4.6 MB, 7 goroutines, 16 files | 4.6 MB, 7 goroutines, 16 files |
| After 20 of each, not collected | 5.5 MB, 7 goroutines, 16 files | 9.7 MB, 7 goroutines, 16 files |
| After 20 of each, collected | 4.7 MB, 7 goroutines, 16 files | 4.6 MB, 7 goroutines, 16 files |

The one file the first evaluation leaves open is a descriptor of the app
database: SQLite's unix VFS does not close a file whose inode another
connection of the process still locks (the store's own), and reuses that
descriptor at the next attach, so the count stays at one and closes with
the store. A hostile metric's session (`TestHostileQuerySpill`, its
temporary files at 62.5 to 65.0 MB while it ran) left nothing once
closed: 3.2 MB in use, 2 goroutines and 8 open files before it opened,
8.7 MB and 3 goroutines with its four queries stopped, and 3.3 MB, 2
goroutines and the same 8 files after it closed.

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

0. **Find the cloud ID.** `GET /_edge/tenant_info` answers `{cloudId}`, a
   UUID string (measured 2026-11-24).
1. **Find the extension.** `POST /gateway/api/graphql`, query
   `extensionContexts(contextIds: ["ari:cloud:jira::site/{cloudId}"])` with
   `extensionsByType(type: "jira:issuePanel")`. The poker panel is the entry
   whose `key` is `planning-poker-forge-panel`. Take its `id`, `appId`,
   `environmentId`, `installationId` and `appVersion` from the response.
   Absence means the add-on is off for that site. Measured 2026-11-24 with
   the document `query Ext($contextIds:[ID!]!){ extensionContexts(contextIds:$contextIds){ id extensionsByType(type:"jira:issuePanel"){ id key type appId environmentId installationId appVersion } } }`
   and operation name `Ext`: `data.extensionContexts` has one entry, its
   `extensionsByType` a list of those seven members, all strings, and no
   `errors` member. The poker panel's `id` has the form
   `ari:cloud:ecosystem::extension/{uuid}/{uuid}/static/planning-poker-forge-panel`
   and `appVersion` the form `N.N.N`.
2. **Get a context token.** `POST /rest/internal/2/forge/context/token` with
   `{extension, extensionData, localId, useWorkspaceAri: true}`.
   `extension` is the listing entry (`id`, `type`, `key`, `appId`,
   `environmentId`, `installationId`, `appVersion`). `extensionData` is
   `{type: "jira:issuePanel", issue: {key, id, type, typeId}, project: {id,
   key, type}}`. `localId` is the extension `id`. The response is `{token,
   expiresAt, expiresIn, extensionId}`. Measured 2026-11-24: `expiresAt` is
   Unix milliseconds, `expiresIn` is milliseconds (899,442, with the
   token's `exp - iat` 900 seconds), and `extensionId` is the extension
   `id`. The issue and project values sent were the issue's own; whether
   made-up values are refused was not tested. The token is a JWT whose
   top-level `contextIds` claim holds one workspace ARI,
   `ari:cloud:jira:{uuid}:workspace/{uuid}`, and whose `context` claim
   names the issue, so a token is per issue. The gateway's own signer,
   `forgeApp_signForgeContextTokens`, refuses API tokens.
3. **Invoke.** `POST /gateway/api/graphql`, mutation `invokeExtension` with
   input `{contextIds, extensionId, entryPoint: "resolver", payload}`.
   `contextIds` must equal the `contextIds` claim inside the token (a
   workspace ARI), or the call fails with `FCT_VALIDATION_CLAIM_MISMATCH`.
   `payload` is `{call: {functionKey: "requestConnectBackend", payload:
   {path, method, headers, body}}, context: {cloudId, localId, moduleKey,
   extension: extensionData}, contextToken}`. Without `contextToken` the
   call fails with "Missing Forge Context Token". The backend's HTTP status
   and body come back in `response.body`. Measured 2026-11-24 with the
   document `mutation Invoke($input: InvokeExtensionInput!){ invokeExtension(input:$input){ success errors { message extensions { errorType statusCode } } response { body } } }`,
   operation name `Invoke`, `moduleKey` `planning-poker-forge-panel`, and
   `localId` the extension `id`: on success `success` is true, `errors`
   is null, and `response.body` is an object `{status, statusText,
   headers, body}` whose `body` is the backend's JSON document as a
   string.

Routes, each under the path prefix `/forge/rest/eausm/latest/`:

| Action | Method and path | Body | Result |
|---|---|---|---|
| Read | `GET planningPoker/{issueId}` | none | `{issueId, isRevealed, votes: [{issueId, userId, vote, createdAt}]}` |
| Vote or change vote | `PUT planningPoker/vote` | `{issueId, vote}` | the vote record, `{issueId, userId, vote, createdAt}` |
| Reveal | `PUT planningPoker/reveal/{issueId}` | none | the read shape with `isRevealed: true` |
| Clear all votes | `DELETE planningPoker/votes/{issueId}` | none | 204, empty body |

The read and the three writes were measured on 2026-11-25 on one issue
with one voter, through the invoke above. Every call answered backend
status 200 except the clear, which answered 204 with an empty string as
its body.

- The vote's `body` is sent as the JSON value, an object whose `issueId`
  is a string, as `pokerInvoke` (`internal/tracker/jira/poker.go`) sends
  it. A read after it holds one vote with the caller's `userId` and the
  value sent.
- A second `PUT planningPoker/vote` changes the caller's vote: the read
  still holds one vote, with the new value.
- The reveal answers the read shape, and reads after it have
  `isRevealed: true`.
- The clear removes every vote and resets `isRevealed` to false.
- `issueId` is a string and `vote` a number in every answer. A vote has no
  `id` member (the 2026-11-15 note listed one).

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

The poker backend does not write the issue's estimate, and neither does
the app from a vote: a person sets the agreed estimate as any other edit,
through the normal Jira API.
