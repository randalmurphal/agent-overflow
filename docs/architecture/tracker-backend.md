# Tracker backend

Status: design. The [tracker clients](tracker-clients.md) exist; nothing in
this document is built until its package exists. The product rulings are in
the [Issues spec](../specs/issues.md).

This document fixes what sits between the tracker clients and the Issues
pane: the tracker-neutral model, connections and key files, the local
mirror, sync, the application service and what agent tools need from it.

## Packages

| Package | Owns |
|---|---|
| `internal/tracker` | The tracker-neutral model, the `Provider` interface and its capability flags. No I/O. |
| `internal/tracker/jiraprovider` | `tracker.Provider` over the `jira` and `tempo` clients. |
| `internal/tracker/linearprovider` | `tracker.Provider` over the `linear` client. |
| `internal/keyfile` | An owner-only file that holds one secret: read, inspect, create. `internal/openaikey` names its file and calls this. |
| `internal/trackerapp` | The application service: connections and their clients, key file sources, the mirror's sync, board pumps, writes. It owns its locks, timers, goroutines and stop gate. |
| `internal/store` | The tracker tables and their accessors. |
| `internal/app` (`app_tracker*.go`) | Bound methods and wire DTOs that call `trackerapp`. No tracker state. |
| `internal/trackertools` | The agent tool contract, served through `internal/threadmcp`. |
| `internal/harness/trackerfake` | Stateful Jira, Tempo and Linear fakes for app tests, the harness and browser tests. |

Imports point one way: `trackerapp` imports `tracker` and the two provider
packages; a provider package imports `tracker` and its clients; `tracker`
imports no client. `internal/app` reaches a tracker only through
`trackerapp`. `trackerapp` receives its dependencies by capability (a store
interface, an emit function, the credentials folder, a lifetime context),
never `*App`, as
[Application composition](root-decomposition.md#ownership) requires.

## The neutral model

`internal/tracker` describes what the pane draws and the mirror stores.
Both trackers shape it: a concept only one tracker has is a capability, not
a field that is empty for the other.

| Type | Jira | Linear |
|---|---|---|
| `Project` | project | team |
| `Board` | board (scrum or kanban) | the team's issues; one per team |
| `Column` | board column with its statuses | one workflow state |
| `Status` | status with its category | workflow state with its type |
| `Sprint` | sprint | cycle |
| `Issue` | issue | issue |
| `User` | user by account ID | user |

- **IDs are the tracker's own, as text**, unique within a connection. A
  key (`WEB-1`) is display and lookup text, never identity: Jira changes it
  when an issue moves project.
- **A status belongs to one group**: backlog, to do, in progress, done or
  canceled. The provider maps the tracker's category or state type; project
  setup may override a status's group or leave it out of counts.
- **Order and estimate belong to an issue on a board, not to the issue.**
  A Jira board names its own rank field and its own estimate field, so one
  issue on two boards can have two of each. The order is one text key that
  sorts bytewise within the board: Jira's rank value as it is, and Linear's
  float sort order encoded so that text order equals numeric order. The
  pane and the mirror compare keys and never interpret them. The estimate
  is a number with the board's unit: points, time or none. A board with no
  estimate counts issues.
- **An issue as a card** carries what a board or backlog row draws and
  does not depend on the board: type, key, title, status, assignee,
  priority, labels, parent, the sprints its read names and its created,
  updated and resolved times. When it entered its status comes from its
  status changes, not the card. A board row is a card with that board's
  order key and estimate. **An issue's detail** adds the description as
  Markdown, every other field with its metadata, the first page of
  comments, links, attachments, history and the moves its workflow allows.
  Detail is read when a ticket is opened. Logged time is not part of it:
  worklogs are read by author and day through the connection's time
  tracker.
- **A field value is typed**: text, rich text as Markdown, number, date,
  date-time, user, option, a list of one of these, or a value the app can
  show and not edit. An edit names an option or user by ID when it was
  picked from a list and by text when a person or an agent typed it; the
  provider resolves text and reports a value it cannot match.
- **Capabilities** say what a connection supports beyond what every
  tracker does: issue types, custom fields, moves that take fields, sprint
  writes and attachments. Time tracking is the presence of a time tracker,
  not a flag.

### Provider

A `Provider` is one connection's tracker. Its calls take neutral inputs and
return neutral values. A read fails with the transport's typed errors
unchanged. The interface in `internal/tracker/provider.go` is the contract;
this is its shape.

- Identity: who the credential belongs to.
- Structure: projects, a project's issue types and boards, one board's
  columns, statuses, estimate and sprints, and the site's link types.
- Sync: a page of a board's changed rows from a cursor; the complete set
  of IDs on a board, or a statement that the set is incomplete; a board's
  rows for a set of IDs; cards for a set of IDs with no board; status,
  sprint, estimate and assignee changes for a set of issues.
- Detail: one issue's detail; pages of its comments; the moves its
  workflow allows now; a download of one of its attachments.
- Finding: issues by key, and a page of cards matching text.
- Writes: create, edit fields, move to a status (with the fields the move
  takes), set an estimate on a board, rank between neighbors, move to a
  sprint or the backlog, sprint create, start, complete and edit, comment
  add, edit and delete, link and unlink, attach.
- People and values: the fields a create takes, users who can be assigned,
  and the allowed values of a field, each narrowed by typed text. A pick
  list is bounded and says when the tracker may hold matches it leaves
  out, so a missing value is never read as one that does not exist, within
  what the tracker can show. Jira's assignable search never shows that its
  list is whole, so that list always says there may be more.
- Time: a time tracker with worklogs by author and day and worklog add,
  edit and delete, or none for a tracker with no time tracking. A Jira
  connection's is Tempo when it has a Tempo connection and Jira's own
  worklogs otherwise.

**A sync cursor is the provider's own bytes.** `trackerapp` stores what a
page returns and hands it back; it never builds or adjusts one. A page's
cursor is the place to resume after that page, including inside a run of
issues that share an update time. A tracker can list a change some time
after it is made, so a walk's last cursor carries an overlap, decided by
one rule both providers call (`tracker.DeltaResume`): every update made
more than `tracker.DeltaOverlap` before the walk's first page was read,
by the tracker's own clock on that answer, was visible to the whole walk.
When the newest row delivered is older than that, the next walk resumes
exactly after it, so a board nobody changed delivers no row; otherwise the
next walk starts at that time and delivers the rows since again. This
computer's clock has no part in it. A stored cursor of an older encoding
is refused as invalid input, and `trackerapp` starts that board's walk
again.

**A write returns an outcome for each thing it touched, not one error.** One
operation can be several requests: a create followed by an image upload, a
rank of 120 issues in three groups. Each issue or step is applied, refused,
unknown or not sent. `trackerapi.WriteNotApplied` judges one request inside
a provider; it never judges an operation. What an earlier request stored
is reported from what the client says it stored, never from the error of a
later one: an image already attached is an applied upload whatever became
of the next.

**A write checks what ties the things it names.** Tracker IDs are
site-wide, so a request does not always tie a comment, a link, a sprint or
a neighbor to the issue or board the input names. Before its first request
a provider reads that tie where the request does not carry it (a comment's
issue, a link's two issues, a sprint's board and state, the issues a board
lists now) and refuses what does not hold; in a rank or a sprint move the
other issues proceed. A rank names its neighbors by issue alone: the
provider reads where they stand now, and refuses a rank whose neighbors no
longer stand in the order named. Neither tracker takes a precondition, so this
narrows the window and does not close it.

The result also names what the operation may have changed, by resource: an
issue's card, an issue's row on a named board, an issue's detail, a board's
structure. A write does not return any of them: an answer that carries an
issue carries only the fields the tracker chose to send. Which kinds of
resource each step leaves stale is one table in `internal/tracker`, and a
result that names fewer is refused, so a provider cannot leave a board or
an open ticket showing the old value.

A create whose answer was lost has an outcome of its own: the tracker may
hold a new issue and nobody knows its ID. The result says so and names no
issue. Nothing retries it.

**History says where it is complete, at both ends.** Jira records every
change from creation. Linear folds the changes of an issue's first minutes
into its creation and records none of them
([measured](../references/tracker-api-measurements.md)). A provider returns
an issue's changes with the time from which they are complete, and a value
before that time is unknown, never inferred from the absence of a change.
A tracker can also list a change some seconds after it is made, so a
provider returns the time through which the changes are final, taken
behind the tracker's own clock on the answer that carried them (this
computer's clock only when the answer names none). `trackerapp` reads an
issue's history again
while the issue's update time is later than that.

## Connections and key files

A connection is one credential for one site: a Jira site with the account's
email, a Tempo region tied to a Jira connection, or a Linear workspace
named by its URL key. Two sites of one tracker coexist. Connections belong
to the computer and are rows in `tracker_connections`.

- The token is the only secret. It lives alone in
  `<configDir>/credentials/tracker-<connection id>`, an owner-only file
  read through `internal/keyfile`. The site, the email and the region are
  not secret and are columns.
- `trackerapp` gives each client a `trackerapi.CredentialSource` that reads
  the key file. The client reads it at first use and after one 401. The
  source keeps a keyed digest of the token it last read, never the token.
  A list, a key file open and a verification each read the file once,
  compare it with that digest and call `ForgetCredential` only when the
  token changed or the file stopped giving one, so a replaced token is
  picked up without a restart and a list never disturbs a call in flight.
  A key file whose token is shorter than `trackerapi.MinSecretLen` is
  refused as a setup problem and nothing is sent.
- A connection's problem is the outcome of the latest call whose outcome
  was applied: every call is stamped when it begins, and one function
  applies an outcome unless a call begun later on the same client already
  applied its own. A failure that arrives late cannot replace a newer
  success, and nothing is applied once the service stopped.
- Adding a connection creates the row and the empty key file, opens the
  tracker's token page and the key file on that computer, then verifies:
  one identity call to the tracker. The row records who the token belongs
  to and when it was verified. A Linear client is bound to the
  connection's workspace: every key it reads is checked before any request
  carries it, and a key for another workspace fails every read and write.
  Tempo's API has no read that names the Jira site a token belongs to, so
  a Tempo token from another site is not detected. No tracker reports when
  a token expires, so the
  expiry is the date the user enters from the token page, and it is the
  only source of the warning before a token lapses. A status call returns
  the file's state, the last verification and the last failure, never the
  token.
- Where a client connects is one function from a connection's kind and
  site to an API origin. Its production value is the site, or Linear's
  fixed origin. Tests and the harness supply one that answers a loopback
  fake, through the app's isolation configuration, so a test App has no
  path to a real tracker.
- Editing the key file and opening the token page need the `host` scope and
  have no remote form. Every other call works from a connected browser or
  another computer and acts on the computer the call is routed to.
- Removing a connection deletes its key file, then its row, which
  detaches its mirror's root in the same transaction; the mirror's rows are
  then deleted in bounded batches, and a drain that a failure or a crash
  cuts short is finished at the next start. The Tempo connection that
  belongs to it goes with it. A failure part way leaves a row with no key
  file, never a key file with no row. Turning a connection off keeps the row and the key
  file, stops its pumps, closes its clients and clears its mirror. A Jira
  connection turned off also closes the client of the Tempo connection
  that belongs to it, whose row stays as it is.
- Work in flight cannot write into a mirror that was cleared. A mirror is
  one `tracker_cache_roots` row with its own ID, and every mirror row
  references that ID. Each read or write captures the root ID when it
  starts and every row it stores names it. Clearing detaches the root
  (`DetachMirror`): every write naming it fails with
  `ErrTrackerCacheGone`, every read is empty, and `FinishMirrorCleanup`
  deletes its rows in transactions of at most 500. Turning the connection
  on again makes a new root with a new ID, so a result that arrives late
  names the detached root and is refused. No caller checks a flag.

## Tables

All tracker tables are created by one migration while the feature is
unshipped, so a renumbering after a merge changes one version constant and
one frozen hash.

Durable records, restored and kept like other application records:

| Table | Holds |
|---|---|
| `tracker_connections` | Kind, site, account, the Jira connection a Tempo connection belongs to, label, on or off, verified identity and time, and the token expiry the user entered. |
| `project_tracker_boards` | Which boards an app project lists, in order, and which opens first. Keyed by project and connection; both cascade. |
| `thread_tickets` | One ticket per thread: tracker kind, site and issue ID with its key and title as last seen. Keyed by thread, cascading with it, cleared when a thread retires to a holder. It names the site, not the connection, so a link survives a connection that is removed and added again. A fork starts with its source's link. A conversation moved or copied to another computer carries its link in the transfer header, and the receiving side replaces whatever link it held for that thread, so a move away and back never brings an old link back; a receiver too old to read the field drops the link. |

The rest of a project's tracker setup (status groups, pinned fields and
sections, card marks, Start work defaults, draft settings, what agents may
do) is one file in the app's directory for that project
(`project.ConfigDir`), beside its workflow profile. `trackerapp` reads and
replaces it whole. Anything a query needs from it is copied into the
mirror, never the other way.

The mirror is every table named `tracker_cache_*`. The name is the
classification:

- Each row belongs, directly or through its parents, to a
  `tracker_cache_roots` row, with `ON DELETE CASCADE` all the way down. A
  connection has at most one root. A root whose connection is cleared or
  removed is detached first: every write naming it is refused, the
  connection can take a new root, and the detached root's rows are deleted
  in bounded batches and then the root. A test walks the schema and fails
  for a `tracker_cache_*` table that does not reach a root by
  `ON DELETE CASCADE` keys, for a `SET NULL` key other than the root's link
  to its connection, and for any other table that references one.
- A restore empties these tables.
- Their shape changes by dropping and recreating them in a migration; the
  next sync fills them. They are never altered in place.
- Rows hold columns for whatever a board, a backlog or a metric query
  filters or sorts by, and one opaque column for the rest of a card, which
  `trackerapp` encodes.

| Table | Holds |
|---|---|
| `tracker_cache_roots` | One row per mirror, with its own ID and its connection. |
| `tracker_cache_users`, `_statuses` | The users and statuses a mirror's reads name. |
| `tracker_cache_boards`, `_board_columns`, `_board_column_statuses` | A board's name, kind and estimate, its columns and the statuses in each. |
| `tracker_cache_sprints` | Sprints as each board shows them, with the board each was made on (a Jira sprint can show on several boards): state and dates. |
| `tracker_cache_issues` | Cards, one per issue per mirror. |
| `tracker_cache_board_issues` | Which issues are on which board, each with that board's order key and estimate. |
| `tracker_cache_issue_sprints` | Which sprints an issue is or was in. |
| `tracker_cache_changes` | Status, sprint, estimate and assignee changes with their times and authors. The issue row holds the time from which they are complete. |
| `tracker_cache_issue_details` | The detail of recently opened issues, one JSON document each. |
| `tracker_cache_worklogs` | Logged time by issue, author and day. A Tempo connection has no mirror of its own; its worklogs are in its Jira connection's. |
| `tracker_cache_worklog_days` | The read that last changed an author's day; a replace from an earlier read leaves the day. |
| `tracker_cache_sync` | Per scope: the provider's cursor, last read, last reconcile, last failure. The linked-issues scope has no cursor: it reads its cards by ID. |

Metric queries will read versioned views over these tables, never the
tables; no view is built yet.

### What the mirror holds

For each board some app project lists:

- its structure;
- its sprints that are active or future, and those closed inside the
  history window (180 days);
- its issues that are not done, those done inside the window, and those in
  a kept sprint;
- every status, sprint, estimate and assignee change the tracker recorded
  for those issues. What a sprint started with is rebuilt from changes made
  before it started, so changes are not cut at the window; they go when
  their issue does. A query that needs a value from before an issue's
  changes are complete gets none and shows the issue as not counted.

Plus, per connection: the issues linked to threads, and the detail of the
200 most recently opened issues the mirror holds. An issue outside every
listed board is read from the tracker when asked for and is not kept unless
a thread links it.

An issue stays while it is on a listed board or a thread this computer owns
links it on that connection's site. It is absent to every read at once
when its last row on a shown board goes and no link keeps it;
`FinishMirrorCleanup` deletes it, its changes first, in bounded
transactions. No caller prunes.

A board whose last listing goes is retired: absent to reads and writes,
its sync state deleted, its rows drained by `FinishMirrorCleanup`. Listed
again before the drain, it stays refused until drained.

Each board row is one lifetime of the board, with an ID never reused. A
caller captures it with the scope's sync state (`MirrorSync.Lifetime`)
before its reads and names it in every write and section continuation, so
nothing read in one lifetime lands in a later one.

A card, detail or history written outside a board is fenced by the root's
`sweep`, which advances in every cleanup transaction that deletes an issue
or part of its history and is never repeated. The write names the value
its caller read before the read (`MirrorSync.Sweep`), and one naming an
earlier value fails with `ErrTrackerCacheSwept`, so content read before
its issue was swept is not stored after the issue is kept again. Only an
opening starts a detail.

Each row's `read_at` is when the latest read admitted for it began, whether
it changed the row or found it identical; an absence is admitted the same
way, as the board's `rows_removed_at`, rows removed or not. A write from a
read that began earlier is superseded, an identical read that began later
moves `read_at` alone, and a duplicate from the same read writes nothing. A
section page carries the board's revision; continuing it at another
revision fails with `ErrTrackerCacheStale`. A card's status time is derived
from the card and the stored history by every write of either.

A board's backlog is its rows in no future or active sprint of that board
whose status is not in the done or canceled group, by the group the mirror
holds. A status the mirror does not know yet counts as not done. A board
that plans no sprints shows every row that is not done.

## Sync

Sync is polling, per scope. A scope is one board, or a connection's linked
issues.

The mirror is filled by resource, and each resource has one read that sync
and writes share:

| Resource | Read | Stores |
|---|---|---|
| Board row | an issue through a board | the card, and that board's order key and estimate |
| Card | an issue with no board (a linked issue outside every listed board) | the card |
| Detail | one issue | the detail document |
| Structure | one board | columns, statuses, estimate, sprints, users |

- **Delta.** Each tick reads pages of a board's changed rows from its
  cursor. One transaction stores a page's rows and the cursor that page
  returned, so a restart resumes exactly after the last stored page. A
  card is never replaced by a version with an earlier update time; a board
  row is stored whenever it is read through its board. A page with no rows
  that returned the stored cursor is not written: the tick's recorded read
  admits the cursor.
- **Reconcile.** A delta sees only issues whose update time moved. An
  issue enters or leaves a board without that: the board's filter is
  edited, an issue is deleted, a board is newly listed while its issues are
  already cards from another board. A reconcile lists the IDs on the board,
  reads the board rows of every listed ID that has no row on this board,
  and removes the rows no longer listed. A listing the provider reports as
  incomplete adds and never removes. It runs when a board is subscribed, on
  a slower cadence, and on request.
- **Structure** is read when a board is subscribed, on a slower cadence,
  and whenever a delta names a status, sprint or user the mirror lacks.
- **History** is read whole for an issue new to the mirror and again for
  an issue whose update time is later than the time its stored history is
  final through, limited to the four change kinds. It is written in
  batches of up to 50 issues (`ReplaceMirrorHistories`). A caller sends an
  issue's whole history in one call, however long; one over 500 changes is
  staged in transactions of at most 500 and made the issue's history in
  one more, so a reader sees the old history until the switch, never part
  of either.
- **Linked issues.** The cards unit reads the linked issues no shown board
  has a row of (`ListMirrorOffBoardLinkedIssues`).
- **A tick's record.** A tick's successes and the failure that followed
  them are recorded by one `RecordMirrorSyncAttempt` statement, which
  writes nothing when nothing changes. An idle tick reads its state in one
  statement (`GetMirrorSync`; `ErrTrackerCacheGone` sends it back to
  `GetOrCreateMirrorRoot`) and writes one row, the read time the board's
  "last synced" shows.
- **Cadence.** A board polls every 45 seconds while a client shows it and
  not at all otherwise. A failed read retries on the doubling delay the
  pull request pumps use; a rate limit waits for its release. Linked issues
  poll every five minutes while any client is connected, and wait for the
  first client's reads to settle at boot.
- **Requests are background** unless a person is waiting on them, so a
  rate limit fails a tick at once and holds the interactive budget for the
  person.
- **Failures reach the subscriber** with a kind (transient, rate limited,
  setup, forbidden, tracker) and a caller-safe sentence. The last rows stay
  on screen.

A board pump has handles, visibility votes, a pause while every handle is
hidden, and a request path for a person's refresh. That mechanism is
`internal/pump`, with the request type as a parameter and its lock supplied
by the owner. The pull request pumps use the same package, so there is one
implementation.

## Writes

Every edit goes to the tracker first.

1. `trackerapp` calls the provider and gets the operation's outcomes and
   the resources it may have changed.
2. Each named resource whose outcome is applied or unknown is read again
   through that resource's read and stored: a ranked issue's row on the
   board it was ranked on, a commented issue's detail when it is cached or
   open, a board's structure after a sprint is created. An issue's card is
   refreshed through every board the mirror has it on.
3. The caller gets the outcomes as they are: all applied, all refused with
   the tracker's reason, or a mix that names which issues and steps
   landed. An operation that created an issue and then failed a later step
   reports the issue as created.
4. A create of unknown outcome is reported as that, with a warning that a
   second attempt may make a second issue. The board's delta runs at once;
   an issue it brings in is the only evidence, and nothing matches it to
   the attempt.
5. Subscribers get the stored changes.

The mirror is never written ahead of the tracker. A client that shows a
drop before the answer owns that preview and replaces it with the event.

A write an agent made is recorded in `tracker_agent_writes` (thread, issue,
what changed, when), which the ticket's history view joins. That table is
added with the agent tools.

## Methods and events

Bound methods take a leading context and carry a scope and a route.

- Two scopes are added: `tracker:read` at the observe tier and
  `tracker:operate` at the execute tier.
- A method whose first parameter is `projectID` routes by project. Every
  other method routes `selected`, and the client pins calls on a
  subscription to the computer that answered the subscribe.
- A subscription is a handle released through the connection's cleanup.
- Events: `tracker:connections_updated`, `tracker:board_updated` (changed
  cards, removed IDs, a sequence number, the pump's failure) and
  `tracker:issue_updated` for an open ticket. Each has a channel policy
  with the read scope.

A board answer is bounded: the subscribe returns the board's structure,
its sprints and the cards of the sections on screen, and a section's
remaining cards are read by page.

## Agent tools

The tools read, search, create, edit, move, comment, move between sprints
and list and add worklogs, through `trackerapp`. A worklog an agent adds is
an entry in a time draft. Only the posting path in `trackerapp` sends time
to a tracker, and it sends exactly the entries it is given: the entries
the user approved (one entry, a day or the whole draft), an entry the user
logged by hand, or the entries of a day with no open question when the
project's setting posts such days. An entry an agent made is never posted
on the agent's call. No tool reaches the provider's worklog write.

Three mechanisms the tools need do not exist for any built-in tool server
yet and are part of that work:

- **Read-only threads.** The handler refuses a writing tool when the
  calling thread's live runtime mode is read-only. The provider cannot
  enforce it, because app tools are admitted in every mode.
- **Approval.** A write naming more than one ticket parks the call, raises
  an app-owned prompt in the thread and resolves on the user's answer or
  the call's end.
- **Timeline.** An applied write adds its own timeline row to the calling
  thread, separate from the tool row.

Ticket text in a tool result is data: results are JSON, and the server's
guide and each read tool say so.

## Tests

- `tracker` providers are tested against the clients' fake servers.
- `trackerapp` is tested with fake providers: a restart between pages of
  issues that share an update time, reconciles that add and remove, a
  board newly listed over issues another board already mirrored, an
  incomplete listing, cascades, each mix of write outcomes including a
  create of unknown outcome, the refresh of each resource a write names,
  history that is not complete from creation, pump lifetimes,
  repeated subscribe and release, and a connection turned off, and off then
  on, while a read and a write are in flight.
- `trackerfake` answers the requests the clients send and fails a request
  it does not handle. The clients' own tests run against it as well, so
  the fake and the clients cannot drift apart.
- App tests and browser tests reach trackers only through `trackerfake` on
  a loopback address.

## Build order

1. `internal/tracker` and the two providers.
2. `internal/keyfile`, `tracker_connections`, connection methods.
3. The mirror tables and accessors.
4. `trackerfake`.
5. Sync, the pump package, board and ticket reads.
6. Writes.
7. Thread links, project setup, Start work.
8. Agent tools.
9. Harness and browser test wiring.
