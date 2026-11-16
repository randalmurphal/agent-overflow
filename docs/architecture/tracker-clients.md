# Tracker clients

The typed clients the [Issues pane](../specs/issues.md) drives: Jira Cloud,
Tempo and Linear, the HTTP transport under them, and the Atlassian Document
Format converter. This document fixes the shapes those packages share. API
contracts live as comments on the exported types. Measured tracker facts
are in
[tracker API measurements](../references/tracker-api-measurements.md).

Nothing here touches the store, the app or a provider. The local mirror,
sync, the tracker-neutral core, key files, agent tools and the harness fake
sit on top of these packages and are not built yet.

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
  package state, no environment reads, no files outside what a call names.
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
- No site's names: no field or status aliases, no custom field IDs, no
  project keys, no host names. Test data uses `example.atlassian.net`,
  `example.com`, keys such as `WEB-1` and people named `User 1`.

## Transport

```go
// internal/tracker/trackerapi
type Options struct {
    Name        string           // "jira", "tempo", "linear": names the client in errors
    BaseURL     string           // https, or http on a loopback address
    Version     string           // User-Agent "agent-overflow/<version>"; required
    Credentials CredentialSource // read at first use and again after one 401
    Dialect     Dialect          // the tracker's own error, login and limit conventions
    MaxPerSecond float64         // paces every request of this client; zero does not pace
    ReadTimeout  time.Duration   // a call whose Request.Timeout is zero; zero means 45s
    MaxBodyBytes int64           // a body Do and JSON read; zero means 8 MiB
    Proxy        func(*http.Request) (*url.URL, error) // nil connects directly
    OnAnswer     func(status int, header http.Header)  // every answer's status and a header copy, before its body
}

func New(opts Options) (*Client, error) // misconfiguration is a construction error
func (c *Client) Do(ctx context.Context, r Request) (*Response, error)
func (c *Client) JSON(ctx context.Context, r Request, out any) (*Response, error)
func (c *Client) Stream(ctx context.Context, r Request, dst io.Writer, limit int64) (*Response, error)
func (c *Client) Location(ctx context.Context, r Request) (string, error) // a redirect's target, not followed
func (c *Client) ForgetCredential()
func (c *Client) Close()

type Request struct {
    Method       string
    Path         string        // relative to BaseURL; absolute only with Absolute set
    Query        url.Values
    Header       http.Header
    Body         any           // JSON-encoded; a []byte passes through with ContentType
    ContentType  string
    ReadOnly     bool          // a POST that only reads (a search, a GraphQL query): retried like a GET
    Absolute     bool          // Path is a full URL the tracker handed out (an attachment's content)
    Transfer     bool          // an upload or download: no total timeout, a stall timeout instead
    Timeout      time.Duration
    MaxBodyBytes int64
}
```

- **Credentials.** A `Credential` holds its secret in a `secret.Bytes` and
  sets one request's `Authorization`: `Basic(user, token)`,
  `Bearer(token)` and `Key(key)` (the bare key Linear takes). Every
  formatter prints it redacted. The client reads its `CredentialSource` at
  first use, keeps the credential, reads again after one 401 and zeroes the
  bytes it replaces. `ForgetCredential` drops it for a caller that knows
  the key file changed. `Close` zeroes it.
- **Where a secret may go.** A credential is sent only to the origin of
  `BaseURL`. A redirect to another origin, and an absolute URL on another
  origin, are requested without it. Answer text in an error
  (`StatusError` `Body`, `Message` and `Fields`, `AuthError.Message`) has
  the credential the request carried replaced by `<redacted>`. Answer
  text is scrubbed after it is decoded, by whoever decodes it:
  `Response.Scrub`, `StatusError.Scrub` and `AuthError.Scrub` remove that
  credential from text a client decodes from a body or shows. They return
  text unchanged when the request carried no credential, and `<redacted>`
  in place of the text once the client no longer holds the credential (a
  re-read replaced it, `ForgetCredential`, `Close`), since zeroed bytes
  cannot be found. `StatusError.Body` is scrubbed as text only. An error
  from another package whose text repeats the credential is replaced by its
  scrubbed text. No error quotes a URL the tracker supplied beyond its
  scheme, host and path.
- **Redirects.** The package follows redirects itself, for `GET` and
  `HEAD` only, at most ten requests. Every hop is admitted like a first
  request: the gate, the pacing, the allowed-URL rule and the origin rule.
  A write never follows one: a 3xx answering a write is a `*StatusError`
  whose `Location` holds the redacted target and whose outcome is unknown.
- **Reads** (`GET`, `HEAD`, a `ReadOnly` POST) are retried on a connection
  failure and a 503, at most four times, waiting `Retry-After` when the
  tracker names it and a doubling backoff otherwise, each wait capped at
  60 seconds. A rate limit is the gate's to handle, below.
- **Writes are never replayed.** The one exception is a 429 on an
  interactive call, which the tracker answers before it applies anything.
  No request can be resent by `net/http` underneath: a request carries no
  replayable body, and the `Idempotency-Key` headers are refused.
  `WriteNotApplied(err)` reports the failures that prove a write did not
  happen: it was never sent, or the tracker answered it with a 4xx. Every
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
  less; the status stays the answer.
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

A tracker's conventions the transport cannot guess:

```go
type Dialect interface {
    // RejectedLogin reports an answer that marks refused credentials
    // whatever its status. Jira sets a header on a 200.
    RejectedLogin(h http.Header) bool
    // Problem reads an error body: the tracker's message and its per-field
    // messages, by field ID.
    Problem(status int, body []byte) Problem
    // RateLimited reports an answer that is a rate limit without being a
    // 429, and when it lifts.
    RateLimited(status int, h http.Header, body []byte) (until time.Time, limited bool)
}
```

### Errors

| Observation | Error |
|---|---|
| The request never left: unusable client, bad request, gate closed, the call ended while waiting to send, or any failure before the transport held a connection (dial, name lookup, proxy, TLS) | `*NotSentError` wrapping the cause |
| 401 after the credential was read again, or an answer the dialect marks as a refused login | `*AuthError` |
| 429, or the dialect's limit, once retries are spent or for a background call | `*RateLimitedError{Until}` |
| Any other status of 400 or above, or a 3xx answering a write | `*StatusError{Method, URL, Status, Message, Fields, Body, Location}`: the URL redacted, the body bounded to 4 KiB, `Fields` the per-field messages. A 404 matches `errors.Is(err, ErrNotFound)`. |
| Timeout, reset, a body cut short | `*TransientError` wrapping the cause. A write's 4xx whose body is cut short stays the status's error, with the body that arrived. |
| A body over its bound | `*BodyTooLargeError` |

Jira answers a request with bad credentials with a 404 on some paths. The
Jira client, not the transport, turns such a 404 into an `*AuthError` by
asking who it is signed in as.

## Converter

`adf` is copied from the `atl` project with its hand-written fixtures. Its
golden files pin the Markdown each document renders to and the document
each Markdown parses to; the corpus checks that every document survives a
round trip and stays valid against Atlassian's schema.

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
to read and write each one.

- **Fields.** The catalog lists every field with its schema. Create and
  edit metadata say which fields a screen offers, which are required and
  which values are allowed. `Value` builds the API value for a field from
  text: an option by name, a user by name or email, a sprint by name, a
  date, a number, rich text from Markdown. A value that matches several
  options or none is an error that lists them. A field is named by its
  exact name or ID; there are no aliases.
- **Writes.** Create, edit, transition (with the fields its screen takes),
  assign, comment (add, edit, delete, with a service desk comment's
  audience), link and unlink issues, remote links, attach and download.
  A Markdown body may name users to mention and local images to embed; an
  image is uploaded only after the write has passed every check that can
  fail before it is sent.
- **Boards.** Boards of a project, a board's configuration (columns and
  their statuses, the estimation field, the rank field), its sprints, and
  the issues of a board, a backlog and a sprint on the
  `/rest/software/1.0` paths with token paging.
- **Ranking and sprints.** Rank before or after an issue, move into a
  sprint and to the backlog, each sent in groups of at most 50 with a 207's
  per-issue results returned. Create a sprint, start it, and complete it;
  completing moves the issues that are not done to the sprint or backlog
  the caller names, then closes the sprint.
- **History.** An issue's changelog as typed entries, and the bulk fetch of
  changelogs for up to 1000 issues and 10 fields.
- **Search.** `search/jql` with token paging, the approximate count, and
  the bulk fetch of issues by key or ID.
- **Worklogs.** Jira's own worklogs: list, add, change, delete.
- **People.** Who the token belongs to, a user by account ID, and the
  users who can be assigned to an issue or in a project.

## Tempo

One `tempo.Client` per site and region. Worklogs are read by author and
date range and written with the numeric issue ID, which the client gets
from an `IssueIDs` its caller supplies. Tempo replaces the whole record on
an update, so a change starts from the record as read: fields it does not
set are sent as read, except that a fully billable record's billable time
follows a new time spent.

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
updated but not moved to the issue asked for. The user's required hours
per day, the approval state of a period, accounts and the site's
timesheet settings are reads.

## Linear

One `linear.Client` per workspace. Every GraphQL document is a package
variable made by `newQuery` or `newMutation`, which register it; a test
fails on an operation built any other way. A test checks each registered
document against Linear's public schema, embedded for tests and refreshed
by `make linear-schema` as the forge schemas are, and fails on a field the
schema marks deprecated and on a document no test sends. The test server
checks every request's variables against the schema as well.

A 200 that carries GraphQL errors is an error. A read fails on any of
them, since a typed result cannot show which fields were left out. A
mutation is refused only by a 4xx, by user errors with no payload, or by a
payload whose `success` is false; a missing `success` is an unknown
outcome. Not found is matched only at the root field the call asked for.

Reads page with Relay cursors. Issues are polled oldest update first from
a stored `PollCursor` that carries Linear's cursor with the time, so a run
of issues sharing one timestamp larger than a page drains across
restarts; a refused cursor restarts the walk at the last fully delivered
time, inclusive. Polling cannot see a hard delete. Manual order is one
`issueUpdate` per issue with a `sortOrder` between its neighbors. Every
page reports its request's cost and the remaining budgets, and
`Client.Usage` holds the latest answer's, failed answers included.

`live_test.go`, behind the `linearlive` build tag, is a manual check
against a blank workspace with a key read from the file a variable names.
No test run includes it.

## Left in atl

Not copied, because they serve a terminal tool or one site: the command
tree and its flags, dry-run printing, prompts, the disk cache, config and
repository context (project and issue keys read from a branch name),
field and status aliases, reports, the Confluence and Outlook packages,
text extraction from attachments, and every recorded response.

## Tests

Each package is tested against an `httptest` server on loopback, inside the
[isolated network](../specs/testing-harness.md#network-isolation). No test
reaches a tracker. A test server asserts the method, path, query and body
it expects and fails the test on a request it does not know, so a client
cannot pass by sending something else.
