# Tracker clients

The typed clients the [Issues pane](../specs/issues.md) drives: Jira Cloud,
Tempo and Linear, the HTTP transport under them, and the Atlassian Document
Format converter. This document fixes the shapes those packages share. API
contracts live as comments on the exported types. Measured tracker facts
are in
[tracker API measurements](../references/tracker-api-measurements.md).

Nothing here touches the store, the app or a provider. The local mirror,
sync, the tracker-neutral core, key files, agent tools and the harness fake
sit on top of these packages; their design is
[tracker backend](tracker-backend.md).

## Packages

| Package | Owns |
|---|---|
| `internal/tracker/trackerapi` | One HTTP client per tracker endpoint: credentials, retries, the rate gate, typed errors, uploads and downloads. |
| `internal/tracker/adf` | Atlassian Document Format to Markdown and back, edits that keep what Markdown cannot show, and the check for an edit that would lose content. |
| `internal/tracker/jira` | Jira Cloud: issues, search, fields and their metadata, values built from field names, create, edit, transition, comments, links, attachments, users, boards, sprints, ranking, changelog and native worklogs. |
| `internal/tracker/tempo` | Tempo: worklogs and batches of worklog writes, the checks before a write, work attributes, accounts, settings, schedules and approvals. |
| `internal/tracker/linear` | Linear: the GraphQL documents and typed calls for teams, workflow states, cycles, issues, comments and ordering. |

Imports point one way: a tracker package imports `trackerapi`, and `jira`
imports `adf`. `tempo` and `linear` import neither `jira` nor each other;
what Tempo needs from Jira (an issue's numeric ID for its key) it takes as
an interface. No package imports `forgeapi`: the forge transport is bound
to GitHub and GitLab hosts and CLI logins, and the two share rules by
following the same contract, not code.

## Rules for every package

- A client is a value built by its caller and safe for concurrent use. No
  package state but what is built once (the transport's pool of cleared
  read buffers, Linear's operation registry), no environment reads, no
  files outside what a call names.
- No package does work when the program starts: the app links these
  packages whether or not a tracker is connected. Tables, regular
  expressions and Linear's sized documents are built on first use;
  `TestInitCost` in `trackerapp` holds each package and `trackerapp` to
  its stated init cost.
- Cost is pinned. Each provider method's request count and the hot paths'
  allocations are asserted in the packages' cost tests, so a change that
  adds a request or an allocation fails a test.
- Nothing prints. A call returns values and errors; there is no dry run, no
  prompt and no progress line.
- Every exported call takes a leading `context.Context` and stops when it
  ends.
- A cache has an owner, a bound and a way to drop it. Nothing is cached on
  disk.
- A write takes a typed input and returns what the tracker stored. A write
  that needs several requests (move the unfinished issues, then close the
  sprint) reports how far it got when it fails.
- Tracker text is data. Ticket text, comments and field values are never
  formatted into a request path or query without escaping.
- A call refused for what its caller passed, before any request, fails
  with an error that matches `trackerapi.ErrInvalidInput`, in every
  client, so a caller tells its own mistake from the tracker's answer. The
  transport's refusal of a malformed `Request` is a bare `*NotSentError`.
- No site's names: no field or status aliases, no custom field IDs, no
  project keys, no host names. Test data uses `example.atlassian.net`,
  `example.com`, keys such as `WEB-1` and people named `User 1`.

## Transport

`trackerapi.New` builds a client from `trackerapi.Options`, and every call
takes a `trackerapi.Request`; the comments on those types, and on the
`Client` methods, are the contract.

- **Credentials.** A `Credential` holds its secret in a `secret.Bytes` and
  sets one request's `Authorization`: `Basic(user, token)`,
  `Bearer(token)` and `Key(key)` (the bare key Linear takes). Every
  formatter prints it redacted. The client reads its `CredentialSource` at
  first use, keeps the credential, reads again after one 401 and zeroes the
  bytes it replaces. When that read fails, the refused credential is
  dropped, so nothing more is sent with it and the next call reads the
  source again. A secret no longer than `<redacted>` is refused as too
  short to be a token: it could not be told from ordinary answer text.
  `ForgetCredential` drops it for a caller that knows
  the key file changed. `Close` zeroes it. Equal `CredentialMark` values
  mean two requests carried the credential from the same read of the
  source on the same client: a re-read that returns an equal credential
  keeps its mark, one that replaces it or follows `ForgetCredential` gives
  a new one, and a request off the base origin has the zero mark. A mark
  holds no secret.
- **Walks.** One answer is one credential's view. A read that composes it
  from several requests (a listing's pages, the batches of a bulk read, a
  document's continuations) runs as one `trackerapi.Walk`: the transport
  checks each answer's mark against the first one each client gave in the
  walk, and a cached answer joins with the mark it was drawn under. An
  error answer is checked like any other, since a read reports a 404 as a
  fact (a missing issue). When one differs, the read starts again from its
  first request under the current credential, once, whatever it returned:
  the walk records the change itself, so a read that keeps a request's
  failure in a partial result cannot hide it. A second change fails the
  read with an error
  matching `trackerapi.ErrCredentialChanged`, a sentinel the transport
  never retries and that is no `*TransientError`. A read
  that cannot start again (Tempo's page-at-a-time worklogs, a download)
  uses `WalkOnce` and fails at the first change. A write inside a walk is
  refused. `tracker.OneCredential`, which `trackerapp` puts on every
  provider, makes each provider read one walk; separate provider calls
  are not, and identity verification governs those.
- **Where a secret may go.** A credential is sent only to the origin of
  `BaseURL`. A redirect to another origin, and an absolute URL on another
  origin, are requested without it. Answer text in an error
  (`StatusError` `Body`, `Message` and `Fields`, `AuthError.Message`) has
  the credential the request carried replaced by `<redacted>`. So does
  `Response.Body`: the transport replaces the credential in every JSON
  string, object keys included, as the string decodes, and as bytes in any
  other body, and does not keep the answer as sent, so no client can decode
  an answer that repeats its credential. `StatusError.Body` is scrubbed the
  same way. A body that holds no form of the credential is returned as
  read, without a copy or an allocation. That check is exact: a body is
  decoded only when it holds a form as bytes, an escape that decodes to a
  byte some form holds, invalid UTF-8 while a form holds a byte of the
  replacement character, or a malformed escape. The scrub covers the text
  one JSON decode of the body produces, read as a stream, so every member
  of a repeated name is covered, not only the one a map would keep. No
  client decodes a string value of an answer as JSON a second time, and a
  test refuses the `string` tag option under `internal/tracker` on any
  field but a built-in number or bool, since it would. Text a decoder
  builds about a body, such as an error naming a path of keys, is not the
  body and goes through `Scrub`. A streamed download is the file's bytes and is not
  scrubbed. `Response.Scrub`, `StatusError.Scrub` and `AuthError.Scrub`
  remove the credential from other text a client builds about an answer,
  such as a decode error. They return text unchanged when the request
  carried no credential, and `<redacted>` in place of the text once the
  client no longer holds the credential (a re-read replaced it,
  `ForgetCredential`, `Close`), since zeroed bytes cannot be found. An
  answer with a body whose credential was dropped before the body could
  be checked is withheld, whatever the body holds: see
  `ErrCredentialChanged` under Errors. An error from another package whose text repeats the credential
  is replaced by its scrubbed text. No error quotes a URL the tracker
  supplied beyond its scheme, host and path.
- **Redirects.** The package follows redirects itself, for `GET` and
  `HEAD` only, at most ten requests. Every hop is admitted like a first
  request: the gate, the pacing, the allowed-URL rule and the origin rule.
  A write never follows one: a 3xx answering a write is a `*StatusError`
  whose `Location` holds the redacted target and whose outcome is unknown.
- **Reads** (`GET`, `HEAD`, a `ReadOnly` POST) are retried on a connection
  failure and a 503, at most four times, waiting `Retry-After` when the
  tracker names it and a doubling backoff otherwise, each wait capped at
  60 seconds. A rate limit is the gate's to handle, below.
- **Writes are never replayed**, but for two answers the tracker gives
  before it applies anything: a 429 on an interactive call, and a 4xx
  refusing the credential when reading the credential again replaced it,
  which sends the write once more. A write whose upload reads a part's
  content once (a stream, not a seekable reader) is sent once and gets
  neither resend.
  No request can be resent by `net/http` underneath: a request carries no
  replayable body, and the `Idempotency-Key` headers are refused.
  `WriteNotApplied(err)` reports the failures that prove a write did not
  happen: it was never sent, the tracker answered it with a 4xx, or its
  upload's content failed or did not hold its declared size, so the
  request stopped short of its `Content-Length` and could not complete. Every
  other failure of a write (a timeout or reset after the transport held a
  connection for it, a 3xx, a 5xx, a 2xx whose body was cut short or does
  not decode) leaves the outcome unknown, and the caller reads the ticket
  again before it offers the write again. The outermost `*NotSentError`
  or answer error decides.
- **Rate gate.** A 429 closes the client's gate until the time the tracker
  names, as soon as its headers arrive. A background call made while it is
  closed fails at once with `*RateLimitedError{Until}`, before any
  credential is read, as does the background call that drew the 429. A
  call marked `WithInteractive(ctx)` waits for the gate at most 10 seconds
  in all across its attempts and is sent again when the gate opens within
  what is left; otherwise it fails the same way. `MaxPerSecond` spaces
  requests for a tracker that limits by address. A limit its status and
  headers show settles the call that drew it at once; its body is read for
  at most 2 seconds, only for a dialect to name a later time.
- **Environment.** The package reads none. A caller that honors the
  user's proxy settings passes `http.ProxyFromEnvironment` as `Proxy`.
- **Bodies are bounded.** `Do` and `JSON` refuse a body over the bound
  instead of reading it. `Stream` copies to its writer up to its limit.
  An error answer's body is read up to the bound or 64 KiB, whichever is
  less; the status stays the answer. A body that declares its length is
  read into one buffer of that length. A compressed one, whose length is
  unknown, grows in a pooled buffer and the caller gets a copy of exactly
  its size; the pooled buffer is cleared before reuse, since it held text
  the scrub had not seen, and one grown past the reuse cap is dropped.
- **Connections.** Each client has its own pool: HTTP/2, at most 8
  connections a host, closed after 90 seconds idle, with a TLS session
  cache so the next connection resumes its session. An idle client holds
  no goroutine or timer of its own. `NewSharing` builds a second client on
  an open peer's connections for two clients of one owner that call one
  origin (a client and the client that checks a key for it): credentials,
  marks, pacing, the rate gate, `OnAnswer`, timeouts and the body bound
  stay separate, closing either ends only its own calls, and the idle
  connections close with the last client sharing them. It refuses another
  origin than the peer's, a `Proxy` (the connections take the peer's) and
  a closed peer.
- **Uploads.** `Multipart` builds an `Upload` body from parts of known
  size. `mime/multipart` writes its framing once; each part's content is
  read only as the request is sent, so an upload holds none of it, and
  its length is declared as the `Content-Length`. A content read once
  that is an `io.Closer` is closed when its attempt ends before reading
  it to its end (the call's context, a stall, `Close`, or an early
  answer), because net/http's HTTP/1 transport neither closes a body on
  a cancel nor returns before its write loop stops reading it; closing
  must end a read in progress. One read to its end is never closed.
- **Transfers.** A `Transfer` request has no total timeout. It fails when
  no byte moves for the stall timeout, or when the answer's headers take
  longer than the header timeout after the request is sent. The timers
  run per hop from its admission: waiting for the gate, the pacing or the
  credential does not count.
- **Isolation.** `New` refuses a `BaseURL` that is not https unless its host
  is a loopback IP address, so a test server is reachable and a mistyped
  scheme is not. The same rule holds for an absolute URL and every
  redirect hop. Routing an isolated boot to a fake tracker is the app
  wiring's job and is not built.

### Dialect

A tracker's conventions the transport cannot guess (a refused login, the
error body's message and per-field messages, a rate limit that is not a
429) are a `trackerapi.Dialect`, whose comment is the contract.

### Errors

| Observation | Error |
|---|---|
| The request never left: unusable client, bad request, a credential source that failed (a key file missing or unreadable, Linear's `*WrongWorkspaceError`), gate closed, the call ended while waiting to send, or any failure before the transport held a connection (dial, name lookup, proxy, TLS) | `*NotSentError` wrapping the cause |
| 401 after the credential was read again, or an answer the dialect marks as a refused login | `*AuthError` |
| 429, or the dialect's limit, once retries are spent or for a background call | `*RateLimitedError{Until}` |
| Any other status of 400 or above, or a 3xx answering a write | `*StatusError{Method, URL, Status, Message, Fields, Body, Location, Header}`: the URL redacted, the body bounded to 4 KiB, `Fields` the per-field messages, `Header` the answer headers `Options.ErrorHeaders` names. A 404 matches `errors.Is(err, ErrNotFound)`. |
| An upload part's content failed, ended short of its size or ran past it | `*SourceError` wrapping the cause; the write was not applied |
| An upload's round trip failed before the transport was handed its whole body (the connection broke, the call ended or the client closed while it went out) | `*CutError` wrapping the cause; the write was not applied. One that fails after the whole body was handed over is unknown. |
| Timeout, reset, a body cut short | `*TransientError` wrapping the cause. A write's 4xx whose body is cut short stays the status's error, with the body that arrived. |
| A body over its bound | `*BodyTooLargeError` |
| A 2xx whose credential was dropped while its body was read, so the body cannot be checked for it | `ErrCredentialChanged`, returned with a `Response` that has the status, header and `CredentialMark` and no body. A write it answered has an unknown outcome. |

Jira answers a request with bad credentials with a 404 on some paths. The
Jira client, not the transport, turns such a 404 into an `*AuthError`; see
Identity under Jira.

## Converter

`adf`'s golden files pin the Markdown each document renders to and the
document each Markdown parses to; the corpus checks that every document
survives a round trip and stays valid against Atlassian's schema, except
the shapes the product writes and the schema refuses (`knownDeviation`:
a one-column layout, a breakout mark on a rule or panel), which only
`Validate` checks.

An edit made in Markdown is applied to the stored document, so content
Markdown cannot show (a macro, a panel, a mention, a merged cell) survives
when the user did not touch it. `adf.CheckLoss` turns the warnings of an
edit that would lose such content into a `*LossError` that names what would
be lost and how to keep it. A caller passes the user's explicit acceptance
to apply the edit anyway. Jira's edit and comment calls run the check; no
caller writes rich text without it.

## Jira

One `jira.Client` per site. The value side of a field is open-ended (a
site adds custom fields), so an issue carries its raw field values by field
ID beside the typed fields every site has, and the field catalog says how
to read and write each one. A field of a record that cannot be read is a
problem on that record, which stays usable.

- **Fields.** The catalog lists every field with its schema. Create and
  edit metadata say which fields a screen offers, which are required and
  which values are allowed. `Value` builds the API value for a field from
  text: an option by name, a user by name or email, a sprint by name, a
  date, a number, rich text from Markdown. A value that matches several
  options or none is an error that lists them. `IDs` is a value picked
  from choices a read offered: it matches an option, user, issue or sprint
  by ID alone, and an ID that names none now is a `MatchError` with
  `Picked` set, never another choice by name. A field is named by its
  exact name or ID; an exact ID wins over any name, and there are no
  aliases. A required rich text field is not emptied, by Markdown or by a
  raw document that shows nothing. The catalog is read once and kept for
  30 minutes, so a field renamed since shows its new name by then;
  `FieldsNaming` reads it again sooner when it lacks a field its caller
  names, once per such field (one still missing is remembered), so an
  idle client sends nothing and a field added since costs one read.
- **Writes.** Create, edit, transition (with the fields its screen takes),
  assign, comment (add, edit, delete, with a service desk comment's
  audience), link and unlink issues, remote links, attach and download.
  A Markdown body may name users to mention and local images to embed.
  Images are read from an absolute folder the caller names, through a root
  that refuses a path or symlink leading outside it, and checked when the
  write is planned. Before the first request of the write they are read
  again, refused if changed, and uploaded only after every check that can
  fail first has passed. Each image is one attach request, then a read of
  its media ID. A failure names its step: `StepUpload` for an attach
  request, `StepMediaID` for the read, which leaves the write that follows
  not sent. Every result reports in `Uploaded` the attachments Jira
  stored, on failure too.
- **Plans.** Create and edit are planned, then applied. A plan is opaque:
  it shows a preview and is applied once, by the client that made it, to
  the issue it was checked for. Apply reads the rich text the plan was
  computed from again and refuses when it changed, a clear included.
  That recheck is the last read before the first write; it narrows the
  window and is not atomic, since Jira offers no precondition.
- **Last read.** A write that rests on a read (an edit, a comment edit, a
  transition, a worklog edit, a sprint move) sends no other request
  between that read and the write. When something must be sent after it
  (an image upload, a user lookup), the record is read again before the
  write. A caller that passes a record as just read (`Current`) has sent
  nothing since reading it.
- **Caches.** Every cached answer (users, the signed-in user, the field
  catalog, board configurations, sprint lists) is kept with the
  `CredentialMark` it was drawn under and is handed out only while the
  transport's own mark equals it, so a credential that is read again,
  forgotten or replaced retires them all, whichever call caused it. A
  board's configuration and sprint list are dropped by `ForgetBoard`,
  which the provider calls before each structure read. Users and the
  field catalog, which no read drops and Jira changes with no signal, are
  read again once 30 minutes old.
- **Write answers.** A write is applied only on a success its spec
  documents that names what was written, by the rule in `jira/writes.go`;
  any other 2xx is an unknown outcome. An edit that asks for nothing is
  refused, and one that asks only for stored values sends nothing and
  says so.
- **Identity.** Jira answers some paths with a 404 when the credential is
  bad, and answers a search sent with no credential 200 with no issues.
  Every answer made for an account names it in `X-AAccountId`
  ([measured](../references/tracker-api-measurements.md#jira-cloud)), so
  a 404, or an answer that leaves out what was asked for (an empty
  search, a bulk read missing issues), is the user's when it carries the
  header, with nothing more sent. One without it gets a sign-in check
  that starts after the answer arrived: a refused credential is
  `*trackerapi.AuthError`. The check vouches for the answer only when
  both carry the same `CredentialMark`; otherwise a 404 is an
  `*UnconfirmedError`, an answer that left something out an
  `*UnprovenError`, and the caller runs the call again. Concurrent
  answers share checks; nothing is reused over time.
- **Rate budget.** `Options.OnAnswer` receives, for every answer, the rate
  limit headers Atlassian documents for Jira Cloud and nothing else.
- **Boards.** Boards by project, type and name, a board's configuration
  (columns and their statuses, the done column, the estimation field, the
  rank field), its sprints, and the issues of a board, a backlog, a sprint
  or an epic on the `/rest/software/1.0` paths with token paging. Jira
  counts only a board's issues and its backlog. An estimate is read and
  set through the board's estimation field; setting one succeeds only on
  an answer that names that field.
- **Ranking and sprints.** Rank before or after an issue, move into a
  future or active sprint, and move to the backlog, with a position only
  on a board's backlog. Each is sent in groups of 50 chained after the
  last issue ranked, stops at the first group refused as a whole or not
  answered in full, and
  reports every issue as done, refused, unknown or not sent. A group's
  answer is read only as a 204 or, for a rank and a board's backlog, a 207
  whose every entry names exactly one issue of the group, by its ID and
  key together, once, with a status; anything else, conflicting or
  repeated identities included, leaves the whole group unknown. Create, update, start and
  delete a sprint. Completing reads the sprint, the target and every issue
  first, moves the issues outside the board's done column to the sprint or
  backlog the caller names, lists the sprint again, and closes it only
  when every move succeeded and no unfinished issue is left; one added
  meanwhile is reported, by ID and key, and the sprint stays open. An issue someone else
  moved between the listing and the move is still moved. A sprint is named for a field value
  among the active and future sprints of the project's scrum boards.
- **History.** An issue's changelog as typed entries, and the bulk fetch
  split across 1000 issues and 10 fields with entries joined again. Sprint
  changes parse to sprint IDs. Bulk change times are epoch milliseconds
  and per-issue ones timestamp strings; both are read.
- **Search.** `search/jql` with token paging, the approximate count, and
  the bulk fetch of issues by key or ID. Reading a search to its end
  returns its warnings and whether it is complete; a bulk fetch and a
  project listing say the same. Either bulk fetch fails as a malformed
  answer when Jira's answer lacks its `issues` or `issueErrors` list
  (measured, Jira answers both, as arrays, even when no issue asked
  exists), so an empty object or an error envelope never reads as every
  issue absent. `BulkFetchByID` fetches IDs alone and fails on an answer
  holding an issue with no ID, one not asked or one twice, before
  anything is collapsed. Text a person or a tracker wrote enters
  a JQL string only through `QuoteJQL`, `QuoteJQLText` (after `~`) or
  `QuoteJQLList` (an `in` list). `MatchJQL` asks
  `jql/match` which of up to 50 issue IDs each of up to 10 queries
  matches, judged from the issues as they are rather than the search
  index; an answer that matches fewer than asked with no error is proven
  to be for the credential's account before it is read as "not matched".
- **Statuses.** Every status of the site with its category, for a board
  whose columns name statuses no listed issue is in.
- **Worklogs.** Jira's own worklogs: list, read by ID, add, change (based
  on the worklog as read, or explicitly unchecked, with the loss check;
  a remaining estimate set alone is a change), delete, with the
  remaining-estimate adjustments each write allows, and the updated and
  deleted feeds for sync.
- **People.** Who the token belongs to, a user by account ID, a user found
  by account ID, exact name or email (active and not an app; a name that
  cannot be proven unique within Jira's search window is an error), and
  the users who can be assigned to an issue or in a project.

### Planning poker

`jira/poker.go` reaches the Easy Agile TeamRhythm add-on the way its issue
panel does, through two undocumented Atlassian endpoints, with the site's
Basic credential and the shapes measured on 2026-11-24
([measurements](../references/tracker-api-measurements.md#planning-poker-add-on)).
It sends the measured GraphQL documents exactly.

- **Whether the site has it.** `PokerInstalled` reads the cloud ID
  (`GET /_edge/tenant_info`, kept while the credential holds) and the
  site's issue panel listing (`Ext` on `/gateway/api/graphql`), and keeps
  the listing 30 minutes, present or absent. A site whose listing names no
  `planning-poker-forge-panel` fails every other poker call with
  `ErrPokerAbsent` before any token or invoke.
- **The context token.** A read or a write first reads the issue's ID,
  key, type and project (`GetIssue`), then asks
  `POST /rest/internal/2/forge/context/token` for that issue's token. The
  token is kept per issue (16 at most) in memory, as a `secret.Bytes`
  with the credential mark it was drawn under, until 60 seconds before
  `expiresIn` ends, and dropped when the gateway refuses it. The invoke's
  `contextIds` is the token's own claim, read from it as is.
- **Calls.** `PokerVotes` reads `planningPoker/{issueId}`;
  `CastPokerVote`, `RevealPokerVotes` and `ClearPokerVotes` are the three
  writes, each one `Invoke` of `requestConnectBackend`. A write's body is
  sent as the JSON value itself, at one place in `pokerInvoke`, since its
  encoding is the one part not measured. `PokerInstalled` and `PokerVotes` are walks
  (`trackerapi.Walk`), so a credential change part way starts them again.
- **Errors.** An answer the measured shapes do not cover is a
  `*PokerError` naming its step (cloud ID, listing, token, gateway, poker
  app) and what was wrong, never a ticket's error. A write is applied only
  on the poker app's 2xx; its 4xx, and a gateway refusal with a 4xx
  `statusCode`, refuse it; any other answer leaves it unknown. The invoke
  returns nothing of its answer that could repeat the token: the poker
  app's body and status text and a gateway message (cut to 200 bytes)
  have it replaced by `<redacted>`, an HTTP status the gateway answers is
  a `*PokerError` that wraps no `*trackerapi.StatusError`, and a refused
  credential's message has it replaced.
- **The fake.** `trackerfake` serves the same three routes
  (`jira_poker.go`): it matches the two documents exactly, signs a token
  with the measured claim names, refuses a missing, expired or
  mismatched token, and keeps each issue's votes. The listing and a GET
  invoke are reads (`readsWhen`), so a poll saves no copy of the site. Unverified: the three
  writes' request encoding, and the answers it had to choose for a failed
  invoke, an issue the token does not name, a vote out of range and the
  round after a clear. A site's `Members` are other people who sign in to
  it, so a test can vote as a teammate.

## Tempo

One `tempo.Client` per site and region. Worklogs are read by author and
date range and written with the numeric issue ID, which the client gets
from an `IssueIDs` its caller supplies. Tempo replaces the whole record on
an update, so a change starts from the record as read: fields it does not
set are sent as read. Billable time is the exception when the change sets
a new time spent and no billable time: a fully billable record stays fully
billable, and a record billed below its time is never left billed above
the new time. Tempo bills the whole time when an update names none, so a
billable time that was read is always sent. An update reads the record
again as its last request before the write and is refused (`Changed`) when
it no longer holds what the change started from, since Tempo takes no
precondition and the checks sit between the caller's read and the write.

Checks run before a write. Those decidable from the op alone (issue,
author, start date, durations, each attribute given once with a key and a
value) always run. The site checks need reads: issue keys `IssueIDs`
knows, known attributes with allowed values and required ones present,
account values that name usable accounts, and a start date not in an
approved or submitted timesheet. A check whose read the token may not make
(Tempo answers 403) is skipped, the outcome lists it, and the write is
sent for Tempo to judge. A read that fails otherwise refuses, unsent, only
the ops it would judge; the others are checked in full and sent.

A batch reports each item as applied, refused, unknown, not attempted, or
updated but not moved to the issue asked for. A write is applied only on
an answer that confirms it, by the rule in `tempo/batch.go`; any other
2xx is unknown, whatever record it carries. The user's required hours
per day, the approval state of a period, accounts and the site's
timesheet settings are reads.

## Linear

One `linear.Client` per workspace. `Options.Workspace`, the workspace's URL
key, binds it: each key the client reads (first use, after
`ForgetCredential`, after a 401) is first asked which workspace it belongs
to, and a key for another one is refused with `*WrongWorkspaceError` before
any request carries it, until `ForgetCredential`. A call that met the
refusal was not sent. Every GraphQL document is registered: a package
variable made by `newQuery` or `newMutation`, or a family of one document
per size made on first use (`newMutationFamily`); a test fails on an
operation built any other way. A test checks each registered
document against Linear's public schema, embedded in `linearschema` and refreshed
by `make linear-schema` as the forge schemas are, and fails on a field the
schema marks deprecated and on a document no test sends. The test server
checks every request's variables against the schema as well.

A 200 that carries GraphQL errors is an error. A read fails on any of
them, since a typed result cannot show which fields were left out. A
mutation is refused only by a 4xx, by user errors with no payload, or by a
payload whose `success` is false; a missing `success` is an unknown
outcome. A `success: true` is applied only when the payload names the
write: a created entity has an ID, a write by ID or identifier answers
with that entity (an issue by any identifier it had), and a comment or
relation delete names the deleted ID; otherwise the outcome is unknown.
An issue delete answers with the trashed issue, which must be the one
named; a permanent delete that answers with no entity is decided by its
`success` (unmeasured). Not found is matched only at the root field the
call asked for, by the message `Entity not found: <Type>`. A write whose
input names, by ID, an entity that no longer exists is refused naming the
input member (`Error.MissingReference`), which the provider reports as a
stale write: the value was picked from a list that has changed.

Issues are listed by a filter (team, state, cycle, update time, IDs or
identifiers), found by text with `SearchIssues`, and read in full, every
relation each way read to its end: one at a time with `Issue`, or up to
`MaxIssueDetails` (10) by ID in one request with `IssueDetails`, which
leaves out an ID that names no issue the user can see (Linear charged the
same points for 1 and for 10,
[measured](../references/tracker-api-measurements.md)). `ListIssuePositions` lists
only each issue's ID, manual order and team. A comment, relation, cycle or team
is read by its ID, and labels by a set of IDs. Label names come nested
with each issue; the read by ID covers only the labels that list leaves
out. A ticket's history (`Issue`, `IssueDetails`, `IssueHistory`) carries
with each entry the users, cycles, projects, teams, parent issues and
labels it names, in the same request; one Linear answers as null leaves
its ID alone. A trashed issue is still answered
([measured](../references/tracker-api-measurements.md#linear-trashed-history));
the other kinds (an
archived label, cycle, project or team, a deactivated user) are
unverified. Each nested entity costs points on every page
([measured](../references/tracker-api-measurements.md#linear-history-names)),
so the reads the mirror makes (`IssueHistories`) select IDs alone. `TeamBoard` reads a team, its states, the
cycles a window keeps and the workspace's users in one request, at the
points of those four reads apart less three requests; only a list longer
than its first page sends more, continued by that list's own read.
`IssueHistories` reads the status, cycle, estimate and assignee
changes of many issues in one request. Linear records none of the changes
made in an issue's first minutes and can list a change seconds after it is
made ([measured](../references/tracker-api-measurements.md)); the client
returns what Linear lists and the provider states where it is complete.

A Linear page cursor follows its issue: it means "after where that issue
is now" ([measured](../references/tracker-api-measurements.md)). So a walk
pages by cursor only in an order nothing changes (creation, or a fixed
list). A walk ordered by update time or by manual order never does. Issues
are polled oldest update first from a stored `Position` no update can
move: the update time of the newest issue delivered and the IDs delivered
at exactly that time (at most 100), which the next page leaves out unless
they were updated since. A larger run of issues sharing one timestamp is
drained by a walk of exactly that timestamp in creation order, paged by
cursor, and the poll then continues strictly past it, so no run is too
large to pass and every stored position resumes. A page hands up
Linear's own clock (its `Date` header) so the provider can decide where
the next walk starts. Polling cannot see a hard delete. The same cycle is
set on up to 50 issues by one `issueBatchUpdate`, and manual order by one
document of aliased `issueUpdate` fields for up to 50 issues, each with a
`sortOrder` between its neighbors. A document that fails proves nothing
about its parts, so the issues are read back in one request and each
part is reported applied or not from what was read. Every
page reports its request's cost and the remaining budgets, and
`Client.Usage` holds the latest answer's, failed answers included.

The `live_*_test.go` files, behind the `linearlive` build tag, are manual
checks and measurements against a blank workspace with a key read from the
file a variable names. No test run includes them. What they measured is in
[tracker API measurements](../references/tracker-api-measurements.md).

## Tests

Each package is tested against an `httptest` server on loopback, inside the
[isolated network](../specs/testing-harness.md#network-isolation). No test
reaches a tracker. A test server asserts the method, path, query and body
it expects and fails the test on a request it does not know, so a client
cannot pass by sending something else. A test that fails every read on
purpose names one handler for unrouted requests
(`jiratest.Server.Otherwise`).

Each client's test server is its own package (`jiratest`, `tempotest`,
`lineartest`), used by the client's tests and by the tests of everything
built on the client, so a provider is tested against the same server its
client is. `linearschema` holds Linear's schema and the checks of a
request's document and variables against it, with no dependency on
`testing`, so the fake Linear in `internal/harness/trackerfake` runs the
same checks; `lineartest`'s server runs them on every request.
