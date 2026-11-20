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
- Measured the same day. `POST /rest/api/3/changelog/bulkfetch` gives each
  history's `created` as a number of epoch milliseconds (13 digits) and the
  issue as a string `issueId`, with no key. The spec declares `created` a
  string and its example shows 10 digits. `GET /issue/{key}/changelog`
  gives `created` as an ISO 8601 string with milliseconds and an offset.
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
