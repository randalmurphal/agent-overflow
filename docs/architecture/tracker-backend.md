# Tracker backend

Status: built below the app layer: the [tracker clients](tracker-clients.md),
the providers, the store mirror and the `internal/trackerapp` service, with
the connection bound methods. What the app layer still owes is listed under
[Build order](#build-order). The product rulings are in the
[Issues spec](../specs/issues.md).

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

Imports point one way: `trackerapp` imports `tracker`, the two provider
packages and the clients it builds; a provider package imports `tracker`
and its clients; `tracker` imports no client. `internal/app` reaches a tracker only through
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
  canceled. The provider maps the tracker's category or state type.
- **Order and estimate belong to an issue on a board, not to the issue.**
  A Jira board names its own rank field and its own estimate field, so one
  issue on two boards can have two of each. The order is one text key that
  sorts bytewise within the board: Jira's rank value as it is, and Linear's
  float sort order encoded so that text order equals numeric order. The
  pane and the mirror compare keys and never interpret them. The estimate
  is a number with the board's unit: points, time or none, of the field
  the board names (`tracker.Estimate.Field`, opaque; Linear names none). A
  board with no estimate counts issues.
- **A sprint's board is the one it was made on**: Jira's origin board, a
  cycle's team, the same on every board that lists it. A Jira sprint
  shows on each board whose filter holds its issues, and the board a
  structure lists it for is that structure's own.
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
  provider resolves text and reports a value it cannot match. A picked ID
  matches by ID alone: one that names nothing now is refused as stale
  (`ReasonStale`), never read as a name.
- **Capabilities** say what a connection supports beyond what every
  tracker does: issue types, custom fields, moves that take fields, sprint
  writes, the largest attachment it stores (none on Linear), and whether
  an issue's update time moves with
  everything its detail shows. Time tracking is the presence of a time
  tracker, not a flag.

### Provider

A `Provider` is one connection's tracker. Its calls take neutral inputs and
return neutral values. A read fails with the transport's typed errors
unchanged. Each read is one credential's view: `trackerapp` wraps every
provider it syncs, reads and writes through in `tracker.OneCredential`
(the verification's one identity call is not wrapped), so a read whose
credential changes part way starts again once, then fails as
`ErrCredentialChanged` ([walks](tracker-clients.md#transport)); a
`Download` fails at the first change. The interface in `internal/tracker/provider.go` is the contract;
this is its shape.

- Identity: who the credential belongs to.
- Structure: projects, a project's issue types and boards, one board's
  columns, statuses, estimate and sprints, and the site's link types.
- Sync: a page of a board's changed rows from a cursor; the complete set
  of IDs on a board, or a statement that the set is incomplete; a board's
  rows for a set of IDs; a new issue's placement on a set of boards
  (`NewRows`: the row on each board that holds it, the boards that do
  not, and the boards it could not judge); cards for a set of IDs with no
  board; status, sprint, estimate and assignee changes for a set of
  issues.
- Detail: one issue's detail, or several issues' details in one call
  (`Details`: Linear reads 10 a request, Jira one issue at a time, four
  at once), where an issue the tracker does not return is missing rather
  than an error; pages of its comments; the moves its
  workflow allows now; a download of one of its attachments. On Jira a
  comment, a link, an attachment and a worklog each move the issue's
  update time; on Linear a comment does not
  ([measured](../references/tracker-api-measurements.md)). The capability
  `DetailMovesUpdated` says which, and covers only the issue's own
  changes: a linked issue's title and status and a workflow edit move
  nothing. With it, a provider reads nothing a detail holds of the issue
  before the read that gives the card's update time has returned, so
  content newer than that time costs one more read later and never
  passes for current.
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
  connection's is Tempo while its Tempo connection is on, and Jira's own
  worklogs otherwise, a Tempo connection turned off included.
- Planning poker: whether the site has the add-on, an issue's votes, and
  vote, reveal and clear (`tracker.Poker`), or none for a tracker that
  cannot have it (Linear). See [Planning poker](#planning-poker).

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
structure, an author's worklogs of a day. A write does not return any of them: an answer that carries an
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
A tracker can also list a change some seconds after it is made, up to
the provider's settle margin (`Capabilities.HistorySettle`), so a
provider returns the time through which the changes are final: that
margin behind the tracker's own clock on the answer that carried them,
and no time when the answer names none, since this computer's clock
says nothing about the tracker's. `trackerapp` reads the history of an
issue whose update time moved at once, and once more when that read was
not final for the update (see History under Sync).

## Connections and key files

A connection is one credential for one site: a Jira site with the account's
email, Tempo for a Jira connection (its API address, by default the one
that serves every region), or a Linear workspace
named by its URL key. Two sites of one tracker coexist. Connections belong
to the computer and are rows in `tracker_connections`.

- The token is the only secret. It lives alone in
  `<configDir>/credentials/tracker-<connection id>`, an owner-only file
  read through `internal/keyfile`. The site, the email and the region are
  not secret and are columns.
- `trackerapp` gives each client a `trackerapi.CredentialSource` that reads
  the key file. The client reads it at first use and after one 401. The
  source keeps the digest of the token it last read (`tokenDigest`:
  SHA-256 over a fixed domain, the connection ID and the token), never the
  token. A list, a key file open and a verification each read the file
  once, compare its digest with that one and call `ForgetCredential` only
  when the token changed or the file stopped giving one, so a replaced
  token is picked up without a restart and a list never disturbs a call
  in flight.
  A key file whose token is shorter than `trackerapi.MinSecretLen` is
  refused as a setup problem and nothing is sent.
- A connection's problem is the outcome of the latest call whose outcome
  was applied: every call is stamped when it begins, and one function
  applies an outcome unless a call begun later on the same client already
  applied its own. A failure that arrives late cannot replace a newer
  success, and nothing is applied once the service stopped.
- Each outcome is bound to what it is about, where it is applied
  (`settle`): the token every request of the call carried, a key file that
  gave no token, no token (no client, or a failure before a token was
  read), or a token that changed during the call. The connection's
  credential source hands the client every token it sends and counts the
  times the token the client may send stopped being the one it held (a
  list's key file read made it drop the token, or a read handed another);
  a first token on a client that held none is not a change. The stamp
  holds that count when the call began (a verification takes it after its
  key file read), and a different count at the end means either token
  could have answered: a failure is then about neither and a success
  records the identity with no token. A
  verification stores its token's digest beside the identity
  (`tracker_connections.verified_token`). Each read of the key file for a
  list or a status compares the token in the file with that evidence: the
  verification counts while the file holds the token it checked, and a
  failure is current while it is about that token, about no token, or
  about a key file read and the file still holds no token. The same token
  saved again changes nothing; any other token, however the file was
  written, makes the verification and a token failure history.
- Adding a connection creates the row and the empty key file. Opening the
  tracker's token page, opening the key file on that computer and
  verifying (one identity call to the tracker) are separate calls. The row
  records who the token belongs to, when it was verified and the digest of
  the token checked. A Linear client is bound to the
  connection's workspace: every key it reads is checked before any request
  carries it, and a key for another workspace fails every read and write.
  Tempo's API has no read that names the Jira site a token belongs to, so
  a Tempo token from another site is not detected. No tracker reports when
  a token expires, so the
  expiry is the date the user enters from the token page. A status call returns
  the file's state, the last verification and whether the file holds the
  token it checked, and the last failure and whether it is current, never
  the token or its digest.
- Settings > Issues > Connections runs that sequence as steps on the new
  row: open the token page, open the key file, the optional expiry date
  (the start of that day on the screen that entered it) and Check. The
  steps show while the key file holds no token or one no check has
  verified, or the current problem is a setup one. A verification not
  bound to the token in the file (another token, or none recorded) reads
  as who the connection was last checked as, and a
  failure that is not current as history after "Before the change". The
  row compares nothing itself (`trackerConnectionStatus`). The row warns from 14
  days before the expiry and shows a passed one as an error, each with
  Renew, which shows the steps again until a check verifies or Done. Away from the computer the two opens are absent
  and the row names the key file's path to copy.
- Where a client connects is one function from a connection's kind and
  site to an API origin. Its production value is the site, or Linear's
  fixed origin. A test supplies one that answers a loopback fake, through
  the app's isolation configuration (`IsolationConfig.TrackerOrigin`); an
  isolated boot or a test App that sets none refuses every connection, so
  neither has a path to a real tracker.
- Editing the key file and opening the token page need the `host` scope and
  have no remote form. Every other call works from a connected browser or
  another computer and acts on the computer the call is routed to.
- Removing a connection deletes its key file, then its row, which
  detaches its mirror's root in the same transaction; the mirror's rows are
  then deleted in bounded batches by the cleanup, which runs after each
  turn off and removal, at the start and after each linked-issues round,
  and a drain that a
  failure or a crash cuts short is finished by the next one. The Tempo connection that
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
  `ErrTrackerCacheGone`, and so do the reads a run makes before it stores
  (`GetMirrorSync`, `ReadMirrorRefreshTargets`), which send it back for
  the connection's root; every other read is empty. `FinishMirrorCleanup`
  deletes its rows, then the root, in transactions of at most 500. Turning the connection
  on again makes a new root with a new ID, so a result that arrives late
  names the detached root and is refused. No caller checks a flag. A
  run belongs to the mirror generation it began in: one that finds its
  root gone takes the connection's current root only while the
  generation is still the connection's (a restore emptied the mirror);
  after a turn off, or off and on, it ends, since its provider and its
  outcome are the earlier connection's.

## Tables

The tracker tables are created by migration v162, frozen, and v165,
which adds `tracker_cache_open_tickets`, the wall times of the reads an
interval is measured from or a person is shown
(`tracker_cache_boards.structure_wall`, `tracker_cache_sync.read_wall`,
`reconciled_wall` and `failed_wall`), `tracker_cache_issues.sprint_set`,
and the issue record's status finality and ticket body
(`tracker_cache_issues.status_final_for` and `body_*`, with
`tracker_cache_issue_bodies` in place of v162's
`tracker_cache_issue_details`, dropped with its documents), and v167,
which adds the link's last-seen status, the card's title column
(`tracker_cache_issues.title`, which the card blob no longer carries)
and `tracker_cache_issue_missing`, and empties the mirror as a restore
does, so every card is read again whole; a later tracker table or column
is a later migration.

Durable records, restored and kept like other application records:

| Table | Holds |
|---|---|
| `tracker_connections` | Kind, site, account, the Jira connection a Tempo connection belongs to, label, on or off, verified identity, time and token digest (empty for a verification recorded before v166 or one whose token changed during its call), and the token expiry the user entered. |
| `project_tracker_boards` | Which boards an app project lists, in order, and which opens first, with the board's name and tracker project as the picker showed them (v166). Keyed by project, connection and board; the project and the connection cascade. |
| `thread_tickets` | One ticket per thread: tracker kind, site and issue ID with its key, title and status as the link reader's mirror last held them ([Thread links](#thread-links)). Keyed by thread, cascading with it, cleared when a thread retires to a holder. It names the site, not the connection, so a link survives a connection that is removed and added again. |

The mirror is every table named `tracker_cache_*`. The name is the
classification:

- Each row belongs, directly or through its parents, to a
  `tracker_cache_roots` row, with `ON DELETE CASCADE` all the way down. A
  connection has at most one root, detached as described under
  [Connections and key files](#connections-and-key-files). A test walks the schema and fails
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
| `tracker_cache_boards`, `_board_columns`, `_board_column_statuses`, `_board_unexplained` | A board's name, kind and estimate, its columns and the statuses in each, and the statuses, users and sprints its row pages named that the mirror lacked. |
| `tracker_cache_sprints` | Sprints as each board shows them, with the board each was made on (a Jira sprint can show on several boards): state and dates. |
| `tracker_cache_issues` | The issue record, one per issue per mirror: its card, the time its status began and the update a final read decided it for, and the bookkeeping of its ticket body. |
| `tracker_cache_board_issues` | Which issues are on which board, each with that board's order key and estimate. |
| `tracker_cache_issue_sprints` | Which sprints an issue is or was in. |
| `tracker_cache_changes` | Status, sprint, estimate and assignee changes with their times and authors. The issue row holds the time from which they are complete. |
| `tracker_cache_issue_bodies` | The ticket bodies of the open tickets' issues and the most recently opened, 200 a mirror: what a ticket shows besides its card, one JSON document each. |
| `tracker_cache_worklogs` | Logged time by issue, author and day. A Tempo connection has no mirror of its own; its worklogs are in its Jira connection's. |
| `tracker_cache_worklog_days` | The latest read of an author's day, an unchanged or empty day included; a replace from an earlier read leaves the day. |
| `tracker_cache_sync` | Per scope: the provider's cursor, last read, last reconcile, last failure. The linked-issues scope has no cursor: it reads its cards by ID. |
| `tracker_cache_open_tickets` | One row per open ticket's pump, named by the process's session and the pump's number: the issue it keeps in the mirror while the ticket is open. |
| `tracker_cache_issue_missing` | The latest read of a linked issue that did not return it, while no card the mirror holds is from a later read ([Thread links](#thread-links)). |

Metric queries will read versioned views over these tables, never the
tables; no view is built yet.

### What the mirror holds

For each board some app project lists:

- its structure;
- its sprints that are active or future, and those closed inside the
  history window (180 days);
- its issues that are not done, those done inside the window, and those in
  a kept sprint, each with its memberships of the sprints a board keeps:
  a Jira card leaves out the sprints that closed before the window, which
  an issue carried through many sprints still names
  (`jiraprovider.Options.SprintsFrom`), and the store takes any number
  of memberships, and of users and statuses in a write's catalog, a
  fixed number to a statement. A card's sprints are compared as one
  digest (`sprint_set`), so a card read again naming the same sprints
  reads and writes no membership, and a changed one reads its stored
  memberships once and writes only the difference;
- every status, sprint, estimate and assignee change the tracker recorded
  for those issues. What a sprint started with is rebuilt from changes made
  before it started, so changes are not cut at the window; they go when
  their issue does. A query that needs a value from before an issue's
  changes are complete gets none and shows the issue as not counted.

Plus, per connection: the issues linked to threads, the issues open as
tickets, and the ticket bodies of the open tickets' issues and of the
most recently opened, 200 in all. An issue outside every listed board is
read from the tracker when asked for and is kept only while a thread
links it or a person has its ticket open.

The issue record is the one record of an issue: its row holds the card
every read of the issue shares, the time its status began, and the
bookkeeping of its ticket body, the rest of what a ticket shows (its
description, fields, comments, links, attachments, history and moves).
A ticket's read stores its card, its body and the evidence of its status
time in one transaction (`store.StoreMirrorTickets`): the card through
the same card path as a card read by ID, ordered, fenced, named from the
catalog entries the card names, with its sprints and the marks of what it
names that the mirror lacks; the body by the record's order of bodies;
the status result for the version it was read for. Its receipt says, for
each ticket, whether the write changed the card, moved the status time
(told as `Changes.History`, so every view of the card follows), and
stored a body or moved the one held to another version (told as
`Changes.Details`, from which the issue's open ticket builds its view
rather than read again). A view of a ticket
shows the record's card, its URL included (Linear's is a stored card
field; Jira's is the site and the key, `jiraprovider.IssueURL`), with its
body, and says when the body is of an older version than the card
(`DetailView.BodyBehind`). An issue the mirror does not keep (a direct
read's) shows the read's own card and document. A record card older than
the document (a board read of an earlier answer that stored the issue's
first keep after a direct read of it) is not rendered: the document's own
card is, so a view never shows a card older than its body. The mark is judged once,
from the card the view renders, after that card is chosen: a returned
detail holds a body of the rendered card's version or is marked behind,
and no additional full read is made solely for version skew (the sweep
recovery in `againIfSwept` stays). A card stored after the body makes an
open ticket's pump due a read as any card change does.

A body is ordered by its version and then its read: a body of a later
update, or of the same update from a later read, replaces the record's;
one of an earlier update, or of the same update from an earlier read, is
superseded, and the read shows the record's; the same body again moves
its read alone and writes no body bytes (its digest, `body_digest`, is
compared). A body is started only by a person's opening or by a read
under the issue's current open-ticket bind, so any automatic read of an
open ticket fills a body the record lacks. The cap keeps the
bodies of the issues an open ticket keeps, then the most recently
opened; one function writes a body and one evicts it, each moving the
bookkeeping and the bytes in one transaction.

A stored body is current only when the tracker says so in the read that
shows it (`IssueDetail`, or an open ticket's read). Where the issue's
update time moves with its detail, that is one cards read of the issue
alone, stored through the same ticket write with no body, which records
the opening on the body the record holds and returns the record's body
bookkeeping: the body is confirmed when the card's update time equals the
body's version and the record still holds the body checked, from the same
read. The detail is read and stored instead when there is no body to
check, when the record's card is already newer than the body, when the
body's read began more than 30 minutes ago (its moves and field metadata
come from the workflow and screens, which move no update time), on a
person's refresh, and where the update time does
not move with the detail. An open ticket due a read because another read
stored its card is read in full when that card is newer than its body,
with no cards read, and checked otherwise. No body is written outside
such a full read. A view shows its parent's key and title and each linked
issue's key, title and status from the mirror's cards, read in one
`ListMirrorCards` with the issue's own card. A direct read shows the
mirror's card of the issue as that lookup finds it when the card is at
least as new as the body, and the document's own card otherwise; a Jira
check's write commits before the lookup, so a checked read shows the card
the check stored. An open ticket's view, built from the record, takes its
own card from the statement that read its body. A linked issue the mirror
keeps no card of shows as the body holds it, at most 30 minutes old; a
parent it keeps no card of shows its ID alone. A failed check leaves the
stored body as it is. An open ticket's first paint is its view built
from the record with no tracker call, before its first read answers; it
records no opening, which the read's keep and write do.

An issue stays while it is on a listed board, a thread this computer owns
links it on that connection's site, or its ticket is open
(`mirrorIssueKept`, the one rule every read and write uses). It is absent
to every read at once when its last keep goes; `FinishMirrorCleanup`
deletes it, its changes and sprints first, in bounded transactions: an
issue with more than a transaction holds loses a chunk of them a
transaction, so every sweep transaction deletes a row or moves past an
issue, and one that does neither fails. An issue kept again midway has
its history read again and its sprints replaced by its next card write.
`trackerapp.Service.Stop` ends a cleanup in flight before its next
transaction; the next cleanup finishes it. No caller prunes.

An open ticket keeps its issue through one row of
`tracker_cache_open_tickets` per ticket pump, whatever brought the person
to it: a search, a parent or a link opened from another ticket, a done
issue older than the window, an issue a reconcile dropped from its board,
or one on a board not read yet. A read keeps a ticket the mirror does not
keep yet before it reads it (`trackerapp.bindTickets`): a probe of the
issue's keep and an insert of one row, the drop two statements
(`TestOpenTicketKeepCosts`). The row only keeps the issue. Which of the
ticket's reads store its body is its bind's rule, held in the process's
memory (`store.MirrorBind`): a pump retired before its keep starts makes
no row, and the keep issues a pending bind, which becomes the issue's
current one only after the keep committed and while the pump is still
live (`trackerapp.keepTicketLocked`, `store.ActivateMirrorBind`); a keep
whose pump retired meanwhile publishes nothing and ends its bind. A bind
activates at most once, never after its end and never once a newer keep
of its issue issued another. It ends, with no I/O, where the pump retires
(`trackerapp.retireTicketLocked`): its last subscription released, its
connection turned off or removed, its mirror generation ended, a read that
found it ended, and `Stop`. Every read an open ticket makes carries the
bind it began under (`store.MirrorTicket.Bind`), and a ticket write admits
its body, and on a cards-only write its opening and the record's cap
order, only while that bind is the issue's current one, looked up once
per ticket in the write's transaction: a write admitted before the
retirement commits though the retirement came before its commit, and every
write whose lookup follows it is refused, whatever became of the row. So a
read of a closed ticket, held across a close or a close and reopen, stores
no body, and the reopened ticket's first read stores its own. A bind names
its issue and its root; a write of one issue under another's bind fails.
A read no open ticket made (`IssueDetail`'s) carries no bind, is ordered
by (version, read) alone and records its own opening, never a bind's; its
body is the shared cached body: an open ticket's view may show it, and a
Jira check confirms it only while the check's own write finds the record
still holding it, and a higher provider version from a direct
read begun earlier than an open ticket's read wins over that read's. The
pump's goroutine deletes the row as it returns, and every pump's goroutine
runs, one stopped before its launch included (a write's refresh may have
kept its issue during the first paint). When the drop leaves the issue
kept by nothing, or a keep keeps a record the mirror held unkept, every
view that shows it as a parent or a link is told (`Changes.Visibility`). A
pump keeps one issue; a keep of another fails. A keep that fails is the
ticket's failure, a failure outside the tracker that its next read
retries; until a keep succeeds the ticket is not read. A pump's delete
that fails stays owed: each run's start tries it again, and `Stop` makes a
last try whose failure is logged, the next process's first run deleting
what it left. So an issue may have two rows, a closed ticket's owed row
beside the reopened ticket's, and what bounds the table is that cleanup,
not the cap of open tickets (64). Rows a process left when it ended
without deleting them are deleted by this process's first run
(`trackerapp.dropEarlierTickets`): every run passes that step as it
starts and every keep is made in a run, a view read included, so the
delete completes before any row of this process is written; a delete that
fails fails its run, and the next run tries it again. The cleanup then
removes what nothing else keeps. A restore empties the table with the
mirror while pumps live, and a root ID is never another root's (the
roots' sequence survives the restore), so a bind of the emptied root
admits nothing: the run that makes the connection's new root keeps every
live open ticket of its mirror generation again, one whose earlier keep
failed included, and binds each one still live once its keep commits
(`trackerapp.keepTicketsAgainLocked`).

A board whose last listing goes is retired: absent to reads and writes,
its sync state deleted, its rows drained by `FinishMirrorCleanup`. Listed
again before the drain, it stays refused until drained.

Each board row is one lifetime of the board, with an ID never reused. A
caller captures it with the scope's sync state (`MirrorSync.Lifetime`)
before its reads and names it in every write and section continuation, so
nothing read in one lifetime lands in a later one.

A card, body or history written outside a board is fenced by the root's
`sweep`, which advances in every cleanup transaction that deletes an issue
or part of its history and is never repeated. The write names the value
its caller read before the read (`MirrorSync.Sweep`), and one naming an
earlier value fails with `ErrTrackerCacheSwept`, so content read before
its issue was swept is not stored after the issue is kept again.

Each row's `read_at` is when the latest read admitted for it began, whether
it changed the row or found it identical; an absence is admitted the same
way, as the board's `rows_removed_at`, rows removed or not. A write from a
read that began earlier is superseded, an identical read that began later
moves `read_at` alone, and a duplicate from the same read writes nothing. A
new row from a read that began before the board's latest removal is
refused, since that removal may have been of it, and so is an absence a
read by ID found when the read began before the board's newest complete
listing, which may have named the row; each is read again: a delta page
with one stores no cursor, and a read by ID reads it once more
(`MirrorChanges.Refused`). A card is ordered by its update time and, for
one update time, by its `read_at`, the latest read admitted for the card
or its sprints, identical or not: a project key rename changes every key
and moves no update time. Every card write follows that one rule
(`mirrorCardSupersededSQL`): a board's rows, cards read by ID
(`UpsertMirrorCards`: linked issues, a write's refresh) and tickets
(`StoreMirrorTickets`: a ticket's read and a Jira round's check). In the
same transaction each card write marks every board the card is on whose
structure lacks a sprint or status the card names as due a structure
read (`mirrorCardMarkSQL`), whichever writer stored it. A ticket shows
the record's card, so a key or label rename at an unchanged update time
shows on the ticket as on the board whichever read lands last. A read's
order time comes
from its root's one clock (`store.BeginMirrorRead`), the only maker of
the `store.MirrorRead` every write that orders by it takes, and every
writer refuses a read of another root: now, or a millisecond after the
root's last read when that is later, so two reads are ordered as they
began whatever their path, process or wall clock. A process's first read
of a root seeds the clock from the highest read time the root stores
(`mirrorReadTimeColumns`, held to the schema by a test), so a read begun
after a restart, a crash included, orders after every stored read; later
reads touch no row. A person's opening of a ticket is such a read. The
service's elapsed-time decisions, and the clock each reads, are listed
beside `Service.sinceWall`: an interval since a stored read is measured
from the wall time the read began at, stored beside its order time, and
is due when none is stored, when the clock is behind it or when it
passed, so a clock step either way reads the scope once. A time a
person is shown (a scope's read, reconcile and failure, an open
ticket's read) is such a wall time, never an order time. Order times
run ahead of the wall clock only while it is behind them, a millisecond
a read.
A section page carries the board's revision; continuing it at another
revision fails with `ErrTrackerCacheStale`. A board view's first paint
reads structure, totals, first pages and what their cards name (statuses,
users and parents) in one snapshot (`ReadMirrorBoardView`), so its totals,
rows and names are one state; a section page reads its names with its rows
(`ListMirrorSection`).

A card's status time is the issue record's, the same on the board and the
ticket. One rule decides it from the evidence of one read
(`store.MirrorStatusSince`), a ticket's read or a history read, taken
before the ticket's history is bounded: the read's latest status change,
whether its history is complete from the creation, and whether it is
final through the card's update (the tracker's clock on the answer less
its settle margin). From a final read, the latest change into the card's
status gives its time, one into another status gives unknown, and no
change gives the creation when the history is complete from it (Linear's
history folds its opening interval, so it never is) and unknown
otherwise. A read that is not final may lack the newest change, so its
result is provisional: the latest change's time when it entered the
card's status no more than the settle margin before the card's update,
and unknown otherwise, since an older change may be a stay the issue
left and came back to since. The result is written only for the version
it was read for; a final result decides the version
(`status_final_for`), and a provisional one never replaces it. The one
writer orders the evidence by the reads that brought it
(`mirrorStatusWriteSQL`), whichever path brought it (a ticket's read, a
history read in a batch or staged, and whether a body is held). A card
write that moves the version or the status sets a floor at its own read
(`status_floor_at`): evidence from a read that began before it, final or
provisional, may be of a stay the issue left and came back to within one
update time, so it gives no result, and the issue is due a history read
that begins after the write. A provisional result enters only from a read
no older than the one behind the stored provisional result
(`status_read_at`), and a later read of the same time moves that order
alone, which is not told as a change. A final result is ordered by the
floor alone: a history read that began before a ticket's provisional read
but answered after the settle margin still decides, since the history
leaves the issue due nothing more. The service serializes a connection's
history reads (its history lock); ticket reads run beside them, so the
order is kept in the record, not by the caller. A history read is told as
a change only when its changes or the shown status time or its finality
changed. A card
read that finds the same status at a newer update keeps the time it
showed, no longer final, so an issue that left its status and came back
between two card reads shows the earlier stay's time until a read of the
new version decides it; one that finds another status shows unknown. An
issue whose record holds no final time for its update is due a history
read, and a round's history read takes the shown tickets' issues first
(`ListMirrorHistoryDue`). An issue the mirror does not keep shows the
result of its read's own evidence. Status evidence discharges no sprint,
estimate or assignee history.

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
| Card | an issue by ID, through no board: a linked issue no polled board has a row of, the check of a stored detail, a write's kept card no rows read returned | the card |
| Detail | one issue, or a connection's open tickets together | the detail document |
| Structure | one board | columns, statuses, estimate, sprints, users |
| Worklogs | an author's span of days, from the time tracker | that author's worklogs of those days, replaced whole |

- **Delta.** Each tick reads pages of a board's changed rows from its
  cursor. One transaction stores a page's rows and the cursor that page
  returned, so a restart resumes exactly after the last stored page. A
  card is never replaced by a version with an earlier update time, and a
  board row by one from a read that began earlier. A page with no rows
  that returned the stored cursor is not written: the tick's recorded read
  admits the cursor. A page with a new row the board's latest removal
  refused stores no cursor, and the delta stops there with More, so the
  next tick reads the page again after the removal.
- **Reconcile.** A delta sees only issues whose update time moved. An
  issue enters or leaves a board without that: the board's filter is
  edited, an issue is deleted, a board is newly listed while its issues are
  already cards from another board. A reconcile lists the IDs on the board,
  reads the board rows of every listed ID that has no row on this board,
  and reads the same way every row the listing leaves out that a read
  begun no later than the listing stored (`MirrorReconcile.Unsettled`).
  The listing and the reads by ID that confirm it judge membership by one
  rule, the one the listing captured (`tracker.Listing.Rule`): an issue
  that is not done, was updated within the 180-day window, or is in a
  sprint or cycle the listing kept. A completed issue that ages out of
  the window leaves the listing and is Missing to the read by ID, so its
  row goes and no later reconcile reads it
  (`TestACompletedIssueThatAgesOutLosesItsRow`).
  The listing removes no row itself: a list may miss an issue its index
  has not caught up with (Jira's lists come from its search index, which
  showed a new issue from 0.6 to 23 seconds after its create), and how
  late is no interval an order time measures. The read by ID is the
  answer: its rows keep the rows the board holds and its Missing removes
  the rest (`WriteMirrorRows`). It is trusted for that because it reads
  no index that lags: Jira's rows read confirms each issue the list left
  out with `jql/match`, which judges the issue as it is (it answered a
  new issue correctly 0.3 s after its create, and agreed with the board's
  list on every sample), and Linear's reads by ID show an issue as soon as
  it exists. A read by ID that is wrong anyway (a match that judged before
  a change) is corrected by the next pass, which reads the row again, and
  a row it removed that the board still holds comes back with the next
  listing that names it. A pass attempts
  each of its candidates (the listed issues with no row and the unsettled
  rows) once, 1,000 a tick; a board's pump keeps the candidates left
  between its ticks and goes on with them without listing again, so a
  tick reports More only while the pass has candidates it has not
  attempted. A pass is open from the reconcile that begins it until
  every candidate was attempted, and an open pass is due at the pump's
  next tick whatever the interval, so a pass a refresh began, one waiting
  for the delta's walk and one whose read failed go on at once. Only a
  board's pump reconciles. One the rows read keeps answering missing is
  attempted again by the next pass, a reconcile interval later. Jira's rows read
  asks `jql/match` about an issue the list left out and fails when the
  match cannot judge the board (after reading the board's configuration
  again once), so a miss never removes a row on the list's word alone. An
  issue the bulk fetch after a refused list shows does not exist is
  Missing and is not sent to `jql/match`; every Jira read by ID is one
  checked bulk fetch, and an answer holding an issue with no ID, one not
  asked or one twice, or lacking its issues or issueErrors list, fails
  the read. Jira's rows read returns
  `tracker.ErrRuleRefused` when Jira answers 400 to the list or the
  match that carries a rule naming sprints (`jira.QueryRefusedError`,
  set at that request; a failed sign-in check that proves the answer is
  not one), the cause not judged (a sprint deleted since the listing
  answers 400 on the board issue list).
  On it the pass ends, the rows stored before it standing, and the
  refusal is the reconcile's failure, which the board's sync row keeps in
  a slot of its own (`MirrorSyncOutcome.ReconcileFailure`, refused, with
  the wall time the refused call returned): only a reconcile that
  completes clears it, a read's success does not, and success wins a tie
  in either slot. The board does not list again in that tick: its
  reconcile waits a reconcile interval from the refusal
  (`Service.refusalWaits`), the board's view showing the newer of the two
  failures meanwhile and its ticks running the other units without
  failing, so a reopen or a restart inside the wait lists nothing; a
  person's refresh lists at once, and a new board lifetime starts clean.
  A refusal that persists costs one listing a reconcile interval (about 4
  Jira pages at 20,000 issues) and no other request
  (`TestARefusalThatPersistsListsOnceAnInterval`). Every tick records
  the units that finished before it failed or stopped, so a completed
  reconcile clears the refusal whatever ends the tick. Every other failed
  rows read keeps the pass's candidates, a failed bulk read included. Linear refuses no rule: its issue filter answers a cycle ID
  that names nothing with no issues and no error (measured 2026-11-23:
  `issues(filter: {cycle: {id: {in: [...]}}}, first: 1)` with a
  well-formed UUID that never named a cycle answered HTTP 200 with data,
  no nodes and no `errors`; a deleted cycle's ID is unmeasured and
  expected to answer the same), so its rows read judges by the rest of
  the rule. A listing the
  provider reports as incomplete adds and reads no row it leaves out,
  and so does one that began before the newest complete listing the
  board reconciled, which saw the rows it leaves out. A read by ID that
  began before that listing removes no row the listing may have named:
  its absence is read once more, from after the listing
  (`MirrorChanges.Refused`). One statement finds the listed issues with
  no row and the rows to read. The rows a listing leaves out
  cost a rows read by ID (Jira: one match request per 50 left out, and a
  bulk fetch when any is placed; Linear: one request per 250); a listing
  that leaves out one row the board no longer holds is 3 requests on
  each tracker, the listing included (`TestFakeJiraRequestCosts`,
  `TestFakeLinearRequestCosts`, "reconcile gone"). It
  runs at a board's first read, on its interval, while a
  pass is open and on request, and waits while the tick's delta walk
  has more pages to read, so a first sync reads each row once and lists
  the board once.
- **Structure** is read at a board's first read, on its interval, while
  the board is structure-due and on request. A row page that names a status,
  sprint or user the mirror lacks, and that no complete structure read
  marked unexplained, makes the board due in the page's own transaction
  (`MirrorChanges.StructureDue`, durable as `MirrorSync.StructureDueAt`). A
  structure from a read that began later clears it; a failed read leaves
  it due. The estimate definition is the unit and the field the estimates
  are of (`tracker.Estimate.Field`: Jira's field ID, none on Linear), so a
  Jira board that switches points fields changes it. A structure that
  changes the board's estimate definition clears the
  cursor, and reads show each row read before it with an unknown estimate
  until the walk reads it again. Each page of a walk names the estimate
  reset captured with its starting cursor (`MirrorSync.EstimateResetAt`);
  the store refuses the cursor of a walk begun under an earlier
  definition (`MirrorChanges.CursorRefused`), and the delta stops there
  with More. A tick whose structure write reports a reset
  (`MirrorStructureWrite.EstimateResetAt`) walks from the window's start.
  A structure write whose marks fail after the structure committed returns
  what it changed with the error, which the tick tells; the next structure
  read settles the marks.
- **History** is read whole for an issue new to the mirror and again for
  an issue whose update time is later than the one its stored history was
  read for, that gained a board row, or whose row was read again after
  its board's estimate definition changed, limited to the four change kinds.
  Each of these is due at once. Due is per connection: whichever scope's
  tick runs the unit reads the connection's due issues, under one history
  lock per connection that every other scope's history unit waits on. A read is stored with the update time it
  was read for (`MirrorHistory.For`, the listing's
  `MirrorHistoryDue.UpdatedAt`), so a later update is a later version
  whatever second it falls in. The read is final for that version when
  its `CompleteTo` is at or after it (both the tracker's clock), when it
  began at least the settle margin after this process received the
  answer to a read for the same version (elapsed time), or when it
  repeats a final read's version for a gained board. Trusting
  `CompleteTo` assumes the tracker's Date and its update times come from
  one clock, continuous to within the settle margin; a forward step of the
  tracker's clock by more than the margin between an update and the read
  makes a read final that has not settled. A read that is not final waits
  the whole margin from its answer (`MirrorHistory.Wait`), then the issue
  is due once more, so each change costs two reads at any poll interval,
  and a first sync reads each issue once when the answers carry a Date and
  twice when they do not (`mirrorHistoryDue`, one rule for the list and
  `MirrorSync.HistoryDue`). Neither read visits an issue inside its wait:
  the sync state probes for the first due issue, and a unit lists its
  at most 200 in one read, those past their wait first, so each costs
  the page, not how many issues wait or are due. The wait is measured
  on the service's steady clock, the wall clock at its start plus the monotonic time since, so a
  wall-clock step while it runs moves no wait. After a restart a stored
  answer proves no elapsed time: an answer that looks to come after now
  is due at once, and the read after an earlier process's answer is final
  only by `CompleteTo`, so a restart during a wait costs at most one extra
  read and never delays one past a margin. A board's tick learns the wait
  is over from the sync state it reads anyway; it has no timer of its
  own. An issue the tracker answers no history for is stored as empty and
  final for its update time. The caller sends
  `MirrorHistoryDue.BoardsRev` back as `MirrorHistory.BoardsRev`, one
  ahead of the issue's refused, and `MirrorHistoryDue.Receipt` back as
  `MirrorHistory.Receipt`: every history write that changes the issue,
  and the cleanup's reset, advances the receipt, so a write is admitted
  only against the state its read was listed from: of answers listed at
  one receipt the first to land is stored and the rest are superseded,
  whatever their times. It is written in
  batches of up to 50 issues (`ReplaceMirrorHistories`), with the users
  and statuses the read named in the first transaction. A caller sends an
  issue's whole history in one call, however long; one over 500 changes is
  staged in transactions of at most 500 and made the issue's history in
  one more, so a reader sees the old history until the switch, never part
  of either.
- **Linked issues.** The cards unit reads the linked issues no polled
  board has a row of (`ListMirrorOffBoardLinkedIssues`): a board whose
  pump is not over, not paused while hidden and tells no failure. It takes
  the polled boards again for each page, so an issue of a board whose pump
  stops, pauses or starts failing is read by the round it happens in, from
  the next page on, or the next round, `Cadence.Linked` later.
- **A tick's record.** A tick's successes and the failure that followed
  them are recorded by one `RecordMirrorSyncAttempt` upsert, after the
  root and board lifetime check every board write makes, and the upsert
  writes nothing when nothing changes. An idle tick reads its state in one
  statement (`GetMirrorSync`; `ErrTrackerCacheGone` sends it back to
  `GetOrCreateMirrorRoot`) and writes one row, the read time the board's
  "last synced" shows.
- **Cadence.** The intervals and their reasons are `trackerapp/cadence.go`.
  A board reads its delta, and a connection's open tickets their details
  together, every 10 seconds on Jira and 45 on Linear while a client
  shows them; nothing hidden by every handle is read. The delta tick also
  runs the reconcile, the structure and the history when each is due, and
  a board never read runs all four. A failed read retries on a doubling
  delay up to the interval; a rate limit waits for its release. One loop
  (`trackerapp/linked_loop.go`), once `StartLinkedSync` arms it, syncs
  each connection's linked issues and finishes the mirror cleanup while a
  Jira or Linear connection is on and a client is connected.
- **Requests are background** unless a person is waiting on them, so a
  rate limit fails a tick at once and holds the interactive budget for the
  person.
- **Failures reach the subscriber** with a kind (`trackerapp.FailureKind`)
  and a caller-safe sentence. The last rows stay on screen.

Each subscribed board has one pump: one goroutine and one timer for all
its handles, plus a retry timer while its reads fail. A hidden pump's
timer is not re-armed, so it makes no request and no store call of its
own until it is shown again, and then it runs the tick it missed; a
person's refresh is served hidden or not. A tick that reports
More runs again at once. Handles, visibility votes, the pause and the
request path for a person's refresh are `internal/pump`, with the request
type as a parameter and its lock supplied by the owner. The pull request
pumps use the same package, so there is one implementation.

Each open ticket has a pump with a goroutine and no timer. It makes the
reads a person waits on, the ticket's first read and a refresh (always a
full read), which alone record an opening, and builds the ticket's view
from the record with no tracker call when the change funnel tells it an
input of the view moved and no read of it is in flight.
Every other read of a connection's tickets is its round's
(`trackerapp/ticket_round.go`): one goroutine and one timer per
connection, running while one of its tickets is shown, ended before
`SetEnabled(false)` or `Remove` returns and joined by `Stop`. Each ticket
carries when its next read is due. A successful read makes it due with
the round's next regular read, so the shown tickets are read together; a
failed read (the pump's, the round's or a write's refresh) makes it due
on the connection's retry schedule, doubling from the retry base to the
interval, or when a rate limit lifts; only a read that failed for none
of its tickets ends that schedule. A stored change of its card makes
it due at once: read in full when the stored card is newer than its
document, checked otherwise. A read that stored the ticket's document
itself, a person's refresh among them, makes it due no read. The
round's timer is the earliest due
time of the shown tickets, and it reads every shown ticket then due in
one read, so one delta that changes N shown tickets costs one read. A
hidden ticket keeps its due time and is read when shown if it passed.
When the read's sync state reports an issue of the connection due a
history read and a ticket it read shows no time in its status, the round
then runs the linked history unit (`roundHistory`): a ticket whose
document cannot settle the time (an issue never moved, or one that moved
inside the settle margin) shows it from the round after its opening, and
a read that was not final is read again a round after its settle margin.
No history being due costs no statement: the sync state the read takes
says so.

A ticket's view has one source, the issue's record in the mirror: its
card and status time, its body, its rows on the boards the mirror shows
(each board's estimate and unit, `DetailView.Rows`, one statement for a
build's tickets, `store.ListMirrorTicketRows`), the cards of the issues the view shows
(its parent, the issues it links) and whether the mirror keeps them, and
the connection's users and statuses (`trackerapp/ticket_view.go`). A
read stores what it found and settles its outcome; every view a
subscriber is told is built from the record, and `TicketDetail` returns
the view last built with no store call. The change funnel tells each
pump of a stored change to an input of its view once the store call that
made it returned, so after its commit. A pump counts the tells; a build
counts them before its first statement and installs its view only when
no tell came since, and builds again otherwise. While a build is in
flight every card, row and keep change of the connection counts, since
the view it builds may show an issue the installed view does not. A
late or repeated tell costs a build, never a view older than the record.
One build reads the bodies of up to 16 tickets, each with its issue's
card, in one statement (`store.ListMirrorBodies`), then the cards of the
other issues their views show, when any, and their names in one more
each, and encodes and hashes each view before it takes the subscription
lock. Whether the view is loaded is part of what is told.

A change told while a read of the ticket is in flight is left to that
read: its settlement builds the view as it settles the outcome, so one
event carries both and no view shows a document with the failure of the
read that stored it. A write's refresh is such a read from its start. A
hidden ticket's view is built when it is shown again. A view whose body
is behind its card makes the ticket due a read at once, once per card
version, so a tracker whose documents lag its cards costs one read, not
a loop; a ticket whose last read failed is left to the retry schedule. A
record that holds no body leaves the view as it was told (not loaded
when it showed none); a build on a change then makes the ticket due a
read at once, and a read whose body the mirror does not keep (the cap
dropped it in the same write, past 200 open tickets) is the ticket's
failure outside the tracker. A body shows only while its issue is kept,
so a body left for the cleanup to sweep is never shown. A read succeeds
only once the tracker answered and the document it checked is confirmed
by the receipt of the check's own write; one the cap dropped or the
cleanup swept meanwhile is read in full. A read of several tickets settles each with its own outcome: a
ticket whose check confirmed its document, whose document was stored,
or that the tracker left out is settled so whatever happened to the
others, and the read's failure is told only to the tickets it did not
answer, the ones a rate limit holds due when it lifts. A failure is told
to each ticket once and cleared by its next success.

Idle ticks write only read times, and send no event. N is the number of a
connection's shown tickets. A card write also stamps each user and status
its cards name with its read, one statement per kind and one row per
entry whether or not it changed (`TestChangedRoundStatements`), since the
stamp orders the entry's later reads; the table is of cards that name
none (`TestIdleTickStatements`):

| Tick that finds nothing changed | Requests | Store calls | Statements | Rows written |
|---|---|---|---|---|
| Shown board | 1 | 2 | 3 | 1 |
| Shown board, the delta's overlap redelivering one identical row | 1 | 3 | 10 | 4 |
| Jira round of N tickets (N up to 100) | 1 | 2 | 6 | N |
| Linear round of N tickets | one per 10 tickets, plus each read a ticket's states, history or labels need past their first page | 2 | 6 | N |

A Jira round checks every ticket with one cards read, decodes no stored
body, reads none in full and moves the read time of the unchanged
records in one statement; a Linear round reads every ticket in full and
stores them in one transaction, which reads every record's state, its
body's digest included, in one statement and moves the read times in one
more, and writes no body bytes: a body read again is compared by its
digest. Every card write, the redelivered row and the Jira check
included, runs the statement that marks the boards its cards need a
structure read for. A ticket that changed adds its own full read.

| Round of 50 tickets, 10 changed | Requests | Store calls | Statements | Rows written |
|---|---|---|---|---|
| Jira | 21 | 5 | 27 | 60 issue rows (50 read times, then the 10 read in full), 10 bodies, 8 users and 6 statuses (read times, stamped by each of the two writes) |
| Linear | 5 | 4 | 20 | 60 issue rows, 10 bodies, 4 users and 3 statuses (read times) |

On Jira the 20 requests past the check are each changed issue and its
comments; Linear's 5 details reads read the changed tickets already,
cards and bodies in one write. Both rounds' store calls include the
bodies read (each with its card) and the catalog read that build the 10
changed views, which show no other issue. The table is of
tickets whose boards no one shows; a shown board of the connection adds
the change funnel's lookup of the changed cards' boards, one more store
call per write that changed any (`ListMirrorIssueBoards`, derived from
`publish`; `TestServiceAtScale`'s shown rounds make it, and no test pins
it).

An idle round rewrites one page of the issues table per open ticket,
whose record holds the ticket's read times, and one page of each catalog
table for their stamps; it writes no body bytes and no page of the
bodies' index (`mirrorTicketMoveSQL.reads`), pinned at a root of 2,000
issues with each tracker's measured card size
(`TestAnIdleRoundRewritesOnePagePerOpenTicket`: 50, 1 and 1). The WAL an
idle round of 50 tickets wrote, measured by the frames it appended, with
the catalog of the round test, before the issue record (the tickets' read
times in their own document rows) and now:

| Idle round of 50 tickets, root size | Jira before | Jira now | Linear before | Linear now |
|---|---|---|---|---|
| 50 issues | 6 pages | 7 pages, 28,872 B | 2 pages | 7 pages, 28,872 B |
| 2,000 issues | 52 pages, 214,272 B | 52 pages, 214,272 B | 2 pages, 8,272 B | 52 pages, 214,272 B |
| 20,000 issues | 52 pages, 214,272 B | 52 pages, 214,272 B | 2 pages, 8,272 B | 52 pages, 214,272 B |

A page is 4,096 bytes and a frame 4,120. At 50 issues the records share
5 pages; from 2,000 each ticket's record is on a page of its own, and the
other 2 pages are the catalog's stamps. Linear's cost rose from the 2
pages of the document table, whose rows sat together, to the issue
pages, since the read times now live in the one record.

A changed row is redelivered by the delta's overlap of one minute
(`tracker.DeltaOverlap`), so about six Jira ticks after a change take the
redelivery row's cost. The trackerapp cost tests pin the provider and
store calls (`TestIdleRoundCosts`, `TestChangedRoundCosts`), the
providers' cost tests and `TestFakeRoundRequests` the requests, and the
store's `TestIdleTickStatements` and `TestChangedRoundStatements` the
statements and row writes. After a change, a round whose ticket shows
no time in its status adds a history read (`roundHistory`), and an
update inside the settle margin a second one once the margin passes;
their cost is the linked history unit's, derived from the code and
pinned by no round test.

### Costs at scale

Three tests measure one board of 5,000 and of 20,000 issues, and five
boards sharing a root, when `AO_TRACKER_SCALE` names the sizes:
`TestMirrorAtScale` in `internal/store` (the store's statements),
`TestServiceAtScale` in `internal/trackerapp` (the service over the
scripted provider) and `TestFakeTrackersAtScale` (the real providers and
clients against `trackerfake`). The command that runs one is in the
comment above `TestMirrorAtScale`
([`tracker_cache_scale_test.go`](../../internal/store/tracker_cache_scale_test.go)).

Counts are exact; no test pins them, so a run of the three is how they
are checked. Times and heap are one development machine's (Intel Core
Ultra 9 285HX, WSL2, Go 1.27.4), measured while other work held its load
average near 16 on 24 CPUs: the first sync's is the fastest of three
runs, a path's the fastest of three runs of five each. The peak heap is
the sampled heap objects, collected garbage not yet swept included, so
it grows when the collector falls behind.

| First sync, one board | 5,000 | 20,000 |
|---|---|---|
| Ticks | 25 | 100 |
| Provider calls (scripted) | 172 | 682 |
| Requests, Jira through `trackerfake` | 239 | 944 |
| Requests, Linear through `trackerfake` | 166 | 615 |
| Write statements, in transactions | 740 in 179 | 2,915 in 704 |
| Allocated, peak heap | 91 MB, 10.2 to 10.3 MB | 365 MB, 29.0 to 29.5 MB |
| Time (scripted) | 1.51 s | 6.41 s |
| Store file per issue, after a checkpoint | 949 B | 890 B |

Steady paths cost the same at both sizes, except the four that walk the
board: the reconcile compares the whole listing, and the board view, the
counts and a patch read whose totals moved total every section. The
totals read one row a board row, its status from
`idx_tracker_cache_issues_status` and nothing from its card, and one row
a sprint; they sort nothing, so their memory does not grow with the
board. A patch read whose client holds the current totals reads only the
named rows.

| Path (store) | Statements | 5,000 | 20,000 |
|---|---|---|---|
| Idle tick | 3 | 85 µs | 76 µs |
| Delta, 100 changed | 11 | 7.3 ms | 8.9 ms |
| History, 50 due | 6 | 12.0 ms | 12.9 ms |
| Reconcile, complete listing | 3 | 5.1 ms | 23.2 ms |
| Board view, 3 sections | 10 | 13.8 ms | 46.4 ms |
| Section page, middle of a column | 3 | 0.78 ms | 0.76 ms |
| Section counts | 4 | 9.5 ms | 40.9 ms |
| Patch read, 1 or 100 changed, totals moved | 8 | 9.9 to 10.0 ms | 39.2 to 41.7 ms |
| Patch read, 1 or 100 changed, totals held | 5 | 0.07 to 0.81 ms | 0.06 to 0.70 ms |
| Ticket open, stored | 13 | 0.32 ms | 0.37 ms |
| Ticket rows of a view build, 16 tickets | 1 | 50 µs | 66 µs |
| Jira round, 100 tickets, 10 changed | 21 | 5.9 ms | 6.0 ms |
| Linear round, 100 tickets, 10 changed | 22 | 3.2 ms | 3.1 ms |
| `New` | 0 | 11.2 ms | 10.6 ms |

A reconcile whose listing leaves out rows removes none itself: it reads
them by ID, 100 a rows read and 1,000 a tick, and that read's write
removes the rows the board no longer holds; its cost at these sizes has
not been measured since. Five boards
of 4,000 issues on one root view in 11.7 ms. Every
statement of the mirror plans the same at 20,000 issues as on a small
mirror, with no `sqlite_stat1`. Through `trackerfake` at 20,000 an idle
tick, a delta of 100 and a Jira history read of 100 are 1, 1 and 4
requests; a reconcile is 4 Jira requests or 41 Linear ones.

Retention does not grow with a board, except a reconcile pass that
spans ticks: its pump holds the candidates left, at most one listing's,
until the pass ends. Five shown boards and 50 shown
tickets hold 130 to 137 KB of heap and 56 goroutines (each pump and the
round with one timer) at both sizes; once released, 82 to 102 KB stay
(the event log) and no goroutine.

## Writes

Every edit goes to the tracker first, through one path
(`trackerapp/write.go`, `Service.write`), whose comments and those of
`write_refresh.go`, `write_place.go` and `write_account.go` hold the
mechanism. A caller sees this:

1. A connection that is off, removed or Tempo is refused with
   `ErrDisabled`, `ErrNotFound` or `ErrNoProvider`, and a request the
   conversion refuses with `ErrInvalidRequest`, before anything is sent.
   A provider result that fails `tracker.Result.Check` is `ErrBadResult`,
   never a partial success, and nothing is read again.
2. Each resource a part that is applied or unknown names is read again
   through that resource's read and stored, batched by read; a create's
   issue is placed first on its board and the connection's other
   subscribed boards the mirror shows. A write adds no history read. Each resource ends
   refreshed, not shown (nothing to read) or stale with why, exactly once.
3. The last tracker call settles the connection's problem: a failed
   refresh read, else a part that could not reach the tracker. A write
   whose every part was refused before sending settles nothing.
4. What the refresh stored passes through the change funnel once, however
   the refresh stopped, and the open tickets it read are settled by it.
5. The caller gets a `WriteView`: each part's issue, step, state, refusal
   (reason, message, field messages, candidates, lost content) and, for an
   unknown or unsent part, its failure; the issue a create made, the board
   it is not on (`NotOnBoard`), `CreateUnknown`, and the resources a
   refresh read could not refresh with its failure (`Stale`). A failed
   refresh is never a write failure.
6. A create of unknown outcome is reported as that and never retried. The
   board's delta runs at once, through the board's pump when one is live;
   an issue it brings in is the only evidence, and nothing matches it to
   the attempt. When that delta fails, `Stale` names it as a resource of
   kind `delta`.

A connection turned off, removed or stopped during the provider call
answers a write that may have happened with its parts and `Ended`, and
reads nothing; one where nothing happened is refused by name. One whose
mirror ends during the refresh stops it and answers `Ended` the same
way, which stands for every resource, since the mirror holds none of
them any more. A refresh writes under the root, sweep and board
lifetimes the hold captured, so nothing lands in a mirror made after the
write began.

The mirror is never written ahead of the tracker. A client that shows a
drop before the answer owns that preview and replaces it with the event.
A Jira read by ID right after a write carries the new values; only a
search matching `updated >=` lags, by up to about 2 seconds
([measured](../references/tracker-api-measurements.md#jira-cloud)), which
the delta's overlap covers.

A write an agent made is recorded in `tracker_agent_writes` (thread, issue,
what changed, when), which the ticket's history view joins. That table is
added with the agent tools.

## Methods and events

The connection methods (`internal/app/app_tracker.go`), the board
methods (`app_tracker_boards.go`), the ticket and form-read methods
(`app_tracker_ticket.go`), the writes (`app_tracker_writes.go`) and the
planning poker methods (`app_tracker_poker.go`) are
bound: each takes a leading context and carries a scope, `tracker:read`
at the observe tier or `tracker:operate` at the execute tier, and a
route. Their events are `tracker:connections_updated`, sent when a
connection or its problem changed, `tracker:board_updated`
(`trackerapp.BoardEvent`) and `tracker:issue_updated`
(`trackerapp.IssueEvent`), all at `tracker:read`. `EditSprint`,
`EditWorklog`, `DeleteWorklog`, `Worklogs` and `RefreshWorklogs` are
`trackerapp.Service` methods that no bound method wraps yet (see
[Build order](#build-order)).

A connection ID is minted on one computer. Every method whose first
argument is one is generated with the route home and routed by its client
to the computer whose answer named the connection (`methodFamilies.ts`,
family `trackerConnection`, which the caller pins as it pins a
subscription handle); the table says `connection` for those.
`TrackerConnections` and `AddTrackerConnection` name none and go to the
selected computer.

| Bound method | Service method | Scope | Route |
|---|---|---|---|
| `TrackerProjects(connectionID)` | `Projects` | read | connection |
| `TrackerBoards(connectionID, project)` | `Boards` | read | connection |
| `ProjectTrackerBoards(projectID)` | `ProjectBoards` | read | project |
| `TrackerBoardChoices()` | `BoardChoices` | read | all |
| `SetProjectTrackerBoards(projectID, held, boards)` | `SetProjectBoards` | operate | project |
| `SubscribeTrackerBoard(projectID, connectionID, boardID, sections, visible)` | `SubscribeBoard` | read | project, caller-bound |
| `TrackerBoard(subscriptionID, sections)` | `Board` | read | home |
| `TrackerSectionRows(subscriptionID, section, after)` | `SectionRows` | read | home |
| `TrackerBoardPatch(subscriptionID, sections, issues, held)` | `BoardPatch` | read | home |
| `SetTrackerSubscriptionVisible(subscriptionID, visible)` | `SetSubscriptionVisible` | read | home |
| `RefreshTrackerSubscription(subscriptionID)` | `RefreshSubscription` | read | home |
| `ReleaseTrackerSubscription(subscriptionID)` | `ReleaseSubscription` | read | home |
| `SubscribeTrackerTicket(connectionID, issue, visible)` | `SubscribeTicket` | read | connection, caller-bound |
| `TrackerTicket(subscriptionID)` | `TicketDetail` | read | home |
| `MintTrackerAttachmentUploadTicket(connectionID, issue, filename, size)` | `AttachmentCeiling`, then the upload route's `Attach` | operate | connection |
| `MintTrackerAttachmentDownloadTicket(subscriptionID, attachmentID)` | `TicketAttachment`, then the download route's `Download` | read | home |
| `TrackerIssueTypes`, `TrackerCreateFields`, `TrackerMoves`, `TrackerAssignableUsers`, `TrackerChoices`, `TrackerLinkTypes`, `TrackerLookup`, `TrackerSearch`, `TrackerComments` | the form reads of the same names | read | connection |
| `CreateTrackerIssue`, `EditTrackerIssue`, `MoveTrackerIssue`, `SetTrackerEstimate`, `RankTrackerIssues`, `MoveTrackerIssuesToSprint`, `CreateTrackerSprint`, `StartTrackerSprint`, `CompleteTrackerSprint`, `AddTrackerComment`, `EditTrackerComment`, `DeleteTrackerComment`, `LinkTrackerIssues`, `UnlinkTrackerIssues`, `AddTrackerWorklog` | `Create`, `Edit`, `Move`, `SetEstimate`, `Rank`, `MoveToSprint`, `CreateSprint`, `StartSprint`, `CompleteSprint`, `AddComment`, `EditComment`, `DeleteComment`, `Link`, `Unlink`, `AddWorklog` | operate | connection, budget 3m |
| `TrackerPokerSupport(connectionID)` | `PokerSupport` | read | connection |
| `FollowTrackerPoker(subscriptionID, follow)` | `FollowPoker` | read | home |
| `TrackerPoker(subscriptionID)` | `Poker` | read | home |
| `VoteTrackerPoker(connectionID, in)`, `RevealTrackerPoker(connectionID, issue)`, `ClearTrackerPokerVotes(connectionID, issue)` | `PokerVote`, `RevealPoker`, `ClearPoker` | operate | connection, budget 3m |

- **The picker's lists and the form reads** (`TrackerProjects`,
  `TrackerBoards`, `TrackerMoves` and the rest) ask the tracker, each
  request bounded by the tracker client, and the whole call ends at the
  method's default budget (`trackerRun`), so the answer arrives before
  the client stops waiting and says the tracker was too slow and to try
  again. Their context reaches the network, so they are not caller-bound.
- **A write** answers its `WriteView` within its 3 minute budget
  (`trackerRun`). On Jira the provider's call makes 1 to 7 requests
  (`jiraprovider`'s `TestRequestCost` pins each: 7 for a create with two
  images or a sprint completion that moves 125 issues, 1 to 3 for the
  others), and the refresh after it 1 or 2 per board the write changed
  and 2 per open ticket it read. The tracker client bounds each request
  at 45 seconds with its retries (`trackerapi` `ReadTimeout`); an image
  upload is a transfer, bounded by its stall timers instead. The 3
  minutes is four requests at that bound: a tracker that answers in
  seconds finishes far inside it, and one slow enough to reach it has
  the run cut, each part then told as applied, unknown or not sent. A
  part whose answer was lost says the change may or may not have been
  made and where to look before trying again, by what the write was
  (`trackerapp.LostWriteCheck`): a created issue is searched for, since
  its board's filter may leave it out; a sprint is among the board's
  sprints, a worklog in the day's logged time, a rank in the board's
  order, and any other change on its ticket. The budget's sentence and a
  result the provider may not give (`tracker_write_unclear`) say the
  same. A part refused for its credential says what the connection's
  problem says. A write's context is the server's, not the caller's.
- **A ticket subscription** belongs to its client connection as a
  board's does: `SubscribeTrackerTicket` ties its handle inside the
  service's publication, and the board's visibility, refresh and release
  methods take its handle. `TrackerTicket` is the ticket as its
  subscribers were last told, from memory, read after an event that says
  it changed.
- **A file attached to a ticket** crosses a ticketed upload route,
  `PUT /attachments/tracker/upload` (`internal/transport/trackerroutes.go`),
  one file a request. The mint refuses a tracker that stores no files
  (Linear, `tracker_attachments_unsupported`) and a file over the largest
  its tracker stores (`tracker_attachment_too_large`; Jira Cloud's per-file
  ceiling, `jira.MaxAttachmentBytes`, 2 GB), before any byte crosses the
  network. An issue ID over 64 bytes (`tracker.CheckIssueID`; Jira's
  are numbers, Linear's 36-byte UUIDs) and a file name over 1,024 bytes
  (`tracker.CheckFileName`) are refused there too, and a ticket
  subscribe refuses the same issue IDs. The ticket names the connection,
  the issue, the file name and its exact size; the route refuses a
  declared length that differs (`tracker_upload_size_changed`: select
  the file again), caps the body at the size and passes it to
  `Service.Attach` as it arrives, which streams it to Jira under that
  length (`trackerapi.Multipart`). A body that ends short, runs long or
  stops arriving fails the Jira request before it completes: its part is
  `not_sent` with the failure kind `upload`, which is not the
  connection's problem and does not clear one. A client that stops
  sending is not waited on: when the Jira request ends (its 90 second
  stall timer, `Service.Stop`, or the write's bound), the tracker client
  closes the body it did not read to its end (`trackerapi.Part`), which
  ends the route's read of the request by moving its read deadline to
  now. The write runs with the request's values and not its end, so a
  client that leaves after its last byte still has the file attached,
  and its whole run is bounded by the transfer window for the file's
  size plus the answer's allowance (`transport.AnswerAllowance`:
  the 5 minutes Jira has to answer, `trackerapi.AnswerTimeout`, a
  write's 3 minute budget for the refresh, and a minute for the answer
  to cross the relays; `trackerAttachBound`): 14
  minutes up to 10 MiB, about 17 hours for 2 GB. The answer is the
  write's `WriteView` as JSON, written within `transport.UploadWindow`;
  a relay gives that answer the same allowance from the upload's last
  byte, so an answer that comes late still reaches the page
  ([byte route deadlines](transport.md#byte-route-deadlines)). A download that declares a body is
  refused (`tracker_download_has_body`) and its connection closed.
- **An attachment is saved** through `GET /attachments/tracker/{attachmentID}`.
  The mint asks the tracker nothing: the attachment must be on the
  ticket as the subscription last showed it (`tracker_attachment_unknown`
  otherwise), and its name becomes the download's. The route streams
  `Service.Download` as `application/octet-stream` with an attachment
  disposition and no declared length, read up to the largest file the
  tracker stores, and bounded by its progress, not a total
  (`transport.DownloadByProgress`): it ends 90 seconds after its last
  byte moved, here and on each relay, and a relay ends one whose first
  byte has not come `transport.AnswerAllowance` after the request. Its status waits for the first
  byte, so a failure before it answers the refusal's code and sentence
  as JSON; one after it ends the response short, which the relays carry
  as a cut transfer.
- **Limits of these routes:** the Android shell refuses an upload over
  50 MiB and a download over 128 MiB, and says the file is over its
  limit.
- **A project's boards** are the boards its picker shows, in order, and
  the one that opens first (store `project_tracker_boards`), each with
  its connection's kind, label, state and verified account
  (`store.ListTrackerBoardChoices`, one join). A board's name is the
  mirror's once the board has been read and the stored one before that,
  so a reload names a board that was never opened. The list is
  replaced whole, and the change takes effect before the save answers,
  doing only what the list needs. The store's write names, in its
  transaction, the boards whose last listing it removed and the boards it
  added whose retired copy is not yet cleared
  (`store.TrackerBoardListing`). The first have their subscriptions end
  (`Ended`, shown or hidden) and their copies left to the mirror cleanup's
  callers; the second are readied (`store.ReadyTrackerBoards`: that
  board's drain and its next lifetime, without the cleanup's lock), so a
  board removed and added back opens at once. That readying is the only
  wait a save adds: 116 ms for a 20,050-issue board in a live root, in 49
  statements deleting at most 500 rows each. A reorder or a move of the
  board that opens first does neither. A project's delete retires its
  boards in the store and does not end their subscriptions; the
  project's panes close and release them.
- **A board subscription belongs to the client connection that made it.**
  The subscribe ties its handle to that connection inside the service's
  publication of the handle (`trackerapp.Tie`): the connection's end
  releases the handle, and every removal of the handle (a release from any
  caller, a turn off, a removal, `Stop`) unties the connection. The reads
  after a subscribe name the handle, and a client pins them to the
  computer that answered the subscribe (`methodFamilies.ts`, family
  `subscription`); the board's scope comes from the handle
  (`SubscriptionBoard`). A release of an unknown handle is a no-op, since
  the connection's end may have released it first.
- **A refresh's wait is bounded** by its method's budget. A wait cut
  while the board's pump was still busy with an earlier read never ran
  the refresh (`pump.ErrNotTaken`) and answers `tracker_refresh_not_run`,
  which says to refresh again. A wait cut after the pump took the refresh
  answers `tracker_refresh_waiting`; the read goes on and its events
  arrive as it lands.
- **Refusals** a person can cause or see carry a code and a sentence of
  what to do next, with the service's error as their cause
  (`trackerRefusal`), on the connection methods as on the board ones:
  `tracker_connection_invalid` (the site, account, workspace or label
  refused, with the reason), `tracker_connection_exists`,
  `tracker_subscription_unknown`,
  `tracker_subscription_ended`, `tracker_subscriptions_full`,
  `tracker_connection_off`, `tracker_connection_missing`,
  `tracker_connection_tempo`, `tracker_board_not_listed`,
  `tracker_board_not_read`, `tracker_page_stale`,
  `tracker_project_missing`, `tracker_boards_refused`,
  `tracker_boards_changed` (a board list built from one that is no
  longer stored: `held`, the list the save was built from, is compared
  with the stored one, in order with which board opens first, in the
  store's transaction), `tracker_attachment_unknown`,
  `tracker_write_unclear`, `tracker_unsupported` and
  `tracker_request_invalid` (whose sentence quotes what the request got
  wrong). A tracker call that failed is `tracker_failed_<kind>` with
  `trackerapp`'s sentence for its kind (`CallFailure`). The byte routes
  answer the same code and sentence as JSON with a 400.

A board answer is bounded: the subscribe returns the board's structure,
its sprints and the cards of the sections on screen, and a section's
remaining cards are read by page. A board view asked for no sections
answers the Board screen's (`BoardView.Screen`): the active sprints of a
board that plans sprints, else every column, chosen in the same snapshot
as the structure, with the first page of the first 20 and the rest left
to the client to read by page.

### Subscriptions

- `SubscribeBoard(projectID, scope, sections, visible, tie)` answers from
  the mirror alone, before any tracker call: a handle, a sequence number,
  the view of the sections or "not loaded", and the poll interval. It is
  refused for a board the project does not list, a connection turned off,
  removed or Tempo, and past 64 board handles. The handle starts with
  `visible` as its vote. A pump made with every handle hidden makes no
  tracker call until a handle shows it or a person refreshes it
  (`pump.Core.AwaitShown`).
- `SubscribeTicket(connection, issue, visible, tie)` answers the same way
  with the ticket's view built from the record, or the view its
  subscribers were last told when its pump runs already; at most 64
  ticket handles. The handle starts with `visible` as its vote, and a
  ticket pump made with every handle hidden makes its first read once a
  handle shows it or a person refreshes it, as a board's does.
  Both subscribes call their tie after the cap check and before a pump
  is made, and every removal of the handle unties it.
- Both answers carry the failure the subscribers were last told, the
  tracker's or one outside it, so a subscriber that joins while reads fail
  shows the failure.
- `SetSubscriptionVisible`, `RefreshSubscription` and `ReleaseSubscription`
  take any handle; one the service does not hold fails a visibility change
  or a refresh with `ErrUnknownSubscription`. A refresh is a person's read: a board's four
  units or a ticket's detail, interactive. The tracker's failure, and its
  answer that a ticket's issue is gone, reach the subscribers alone, and
  the refresh returns nil; it fails with a failure outside the tracker (a
  ticket view that could not be built, a store error), the read's stop, or
  `ErrSubscriptionEnded` when the subscription ends first.
- `TicketDetail(handle)` is the ticket as its subscribers were last told:
  its failure, and its view or not loaded, from memory.
- `BoardPatch(scope, sections, issues, held)` is the patch read below;
  `held` names the totals the client holds. It takes up to every section
  a board can have (`store.MirrorMaxViewSections`, 301), since it reads
  only the named issues' rows of each.
- `StartLinkedSync` arms the linked-issues loop. `New` starts nothing.
  The app's boot step `tracker.start` (`startTrackers`, with the
  unattended work) arms it, and once the first client's reads settle runs
  `FinishMirrorCleanup`, which finishes a drain the last run left; the
  loop runs one only after each of its rounds. A failure of either is a
  boot failure every client shows. Before clients are served it reads the connections once:
  70 µs with a 20,000-issue mirror and 50 kept tickets. After the first
  reads, the cleanup with nothing pending costs 0.1 ms, and a drain of a
  detached 20,000-issue root 1.06 s in 542 writes of at most 15 ms.

Events go to `Deps.EmitBoard` and `Deps.EmitIssue`, and the loop's
failures go to `Deps.LoopFailed`. `Deps.Settled` and `Deps.ClientConnected`
gate the loop. `Stop` stops and joins every pump and the loop.
`SetEnabled(false)` and `Remove` end the connection's subscriptions before
they return, and each subscriber is told once (`Ended`).

- **Board event:** the scope, a sequence number, the changed and removed
  IDs, `ReadAgain`, the sync view (last read, last reconcile, failure) and
  `Ended`. Past 100 IDs it carries none, with `Truncated` and `ReadAgain`
  set. `ReadAgain` is set when the board's structure or the connection's
  users and statuses changed, since that moves rows no ID names.
- **Issue event:** the connection, the open issue it is about (always
  set), a sequence number, `Changed` when its view changed, the ticket's
  failure and `Ended`. A ticket the tracker no longer returns is told a
  `not_found` failure; a failure outside the tracker (the store, a
  decode) is told as `internal` with a fixed sentence, as a board's is.
- **Sequence.** One sequence numbers every answer and event of the
  service (`trackerapp/subscriptions.go`). An event numbered at or below
  an answer is in that answer. One numbered above may be too, and applying
  it is harmless.
- **Event rate.** A tick that changed nothing sends nothing, so an idle
  shown board sends no event. No event is sent for the read time alone:
  while no failure is reported, the board was read within the poll
  interval the answer carries. A failure is sent when it starts, changes and clears.

Every tick's changes and every open ticket's read pass through one change
funnel (`trackerapp/funnel.go`):

- The tick's own board gets its changed rows and cards and its removed
  rows.
- Every subscribed board of the connection gets its rows of the changed
  cards and histories, and its rows whose parent's card changed or whose
  parent gained or lost a row on any board (a row shows its parent's key
  and title while the mirror keeps the parent; a parent's rank or
  estimate moves neither). These come
  from one indexed read (`ListMirrorIssueBoards`), which also finds the
  children of a parent that has no row left, made only when a board is
  subscribed; when it fails, or no mirror root is known, every board reads
  again.
- A change of the connection's users or statuses, by a row read or a
  structure read, has every board read again.
- An open ticket whose card changed is due a read at once, with its
  round. One whose view depends on another input that moved (its history,
  its row, the mirror's copy of its document, a card or row of an issue
  its view shows, the users and statuses) builds its view from the record
  with no tracker call. A ticket detail read outside a subscription
  (`IssueDetail`) publishes what it stored too.
- Each store write of a run is published as it returns (`run.told`),
  before any later call of the run can wait or fail: a tick's pages,
  history and structure, a write's refresh, a ticket's read and a Jira
  round's check alike, so a later failure holds back no committed change.
  A shown board adds one lookup of the changed cards' boards per write
  that changed any (`ListMirrorIssueBoards`).
- The root a change is published under is the one the run that stored it
  held, so a connection change during the read cannot lose it. A change
  reaches only the subscriptions of that mirror: the connection's mirror
  generation moves when it is turned off or removed, a run reads it with
  its root and a pump when it is made, and the funnel tells only the
  pumps of the run's generation. A change stored under a mirror a turn
  off detached reaches no subscription made after the turn on.
- A write's refresh publishes each board's write as that board's own,
  and settles the open tickets it read itself instead of nudging them. A
  ticket whose bind failed is not refreshed: the write reports its
  detail stale with the bind's failure.

### Writes and form reads

Each write is one `trackerapp.Service` method on a connection ID, with a
JSON request converted in `write_inputs.go` (`Attach` takes the files
themselves), answering a `WriteView` (`write_methods.go`): `Create`,
`Edit`, `Move`, `SetEstimate`, `Rank`, `MoveToSprint`, `CreateSprint`,
`StartSprint`, `CompleteSprint`, `EditSprint`, `AddComment`,
`EditComment`, `DeleteComment`, `Link`, `Unlink`, `Attach`, `AddWorklog`,
`EditWorklog` and `DeleteWorklog`. A field value is given as a ticket
shows one (`FieldValueView`), as typed text, or as Markdown with the
Markdown it was written from; one decoder turns it into the model's edit
and refuses a value its kind cannot hold. A worklog write on a tracker
with no time tracking is a refused part.

`Worklogs(connection, author, from, to)` answers an author's days from
the mirror with no tracker call. `RefreshWorklogs` reads at most 31 days
from the time tracker through the worklogs resource first, on the write
path with no write.

The reads a write form needs go to the tracker, interactive, and settle
the connection's problem with their outcome (`form_reads.go`): `Projects`,
`IssueTypes`, `Boards`, `CreateFields`, `Moves`, `AssignableUsers`,
`Choices`, `LinkTypes`, `Lookup`, `Search`, `Comments` (a later page) and
`Download`. The mirror holds none of them: configuration it never reads,
moves that follow a workflow change no update time shows, matches against
the whole tracker, and pages and files it does not store. Statuses and
users an answer names without names are named from the mirror; a found
card's parent is named by ID.

### Planning poker

Planning poker's votes belong to an add-on, Easy Agile TeamRhythm, held
on the vendor's servers and reached through two undocumented Atlassian
endpoints ([measurements](../references/tracker-api-measurements.md#planning-poker-add-on);
the client's calls in [tracker clients](tracker-clients.md#planning-poker)).
Nothing about them is in the mirror, and **no vote writes the issue's
estimate**: a person sets the agreed estimate as any other edit
(`SetTrackerEstimate`).

- **Whether a site has it** is `TrackerPokerSupport(connectionID)`, which
  a ticket asks when it opens so it can leave the control out. A Linear
  or Tempo connection answers `available: false` with no request. A Jira
  connection's client reads the cloud ID and the site's extension
  listing once, and keeps the listing 30 minutes (`siteAge`), present or
  absent, so a second ask costs nothing; it never invokes the add-on. It
  is a read by connection, not a field of the ticket subscribe's answer,
  because a subscribe answers from the mirror with no tracker call. A
  listing that fails answers `available: false` with its failure, which
  is poker's alone and settles nothing of the connection.
- **The votes follow a ticket while its poker panel is open.**
  `FollowTrackerPoker(subscriptionID, true)` makes that ticket handle a
  follower. While at least one handle both follows and shows the ticket,
  a poker run of the ticket's reads the votes at once and then every 5
  seconds (`trackerapp.PokerEvery`, `Cadence.Poker`), one read for every
  follower. The run is its own goroutine, so a slow add-on never holds
  the ticket's own refresh; when no handle both follows and shows the
  ticket, the run ends, its read in flight is cancelled, and nothing it
  read after is applied. The ticket's pump and `Stop` wait for it. A read
  whose view differs from the last
  sends `tracker:issue_updated` with `poker: true`, and the client reads
  `TrackerPoker(subscriptionID)` from memory; a read that changed nothing
  sends nothing. A hidden ticket, an unfollowed one and a closed one make
  no poker request; showing a followed ticket again reads at once. The
  last follower's unfollow or release drops the votes held. A Linear
  ticket's follow is refused (`tracker_unsupported`).
- **What a read shows** (`trackerapp.PokerView`): whether the votes are
  revealed, and each vote's voter, named from the tracker's users (one
  user read for each voter the client has not kept in 30 minutes), and
  whether it is the caller's own. A voter Jira does not show (a 404, or a
  403 for a credential that may not see users) is shown by account ID,
  the round stands, and the poker read keeps that answer 30 minutes as
  it keeps a name (`jira.Client.PokerVoter`; every other user lookup asks
  each time). Before the reveal only the caller's own vote has a value, whatever the add-on answered (the provider hides the
  rest). A read that fails keeps the last votes, carries its failure, and
  is retried on the round's retry schedule or when a rate limit lifts.
  The failure is never the ticket's or the connection's; a change in the
  shape of the undocumented calls reads "planning poker is unavailable on
  this site right now", with the step and what was wrong, and that the
  rest of the ticket is unaffected.
- **The writes** (`VoteTrackerPoker`, `RevealTrackerPoker`,
  `ClearTrackerPokerVotes`) go through `Service.write` and answer a
  `WriteView` with one `poker` part, as every write does: refused with a
  sentence (a site without the add-on, a vote outside 0 to 1000, the
  add-on's or the gateway's 4xx), unknown with "look at the ticket's
  poker votes before you try again" (a 5xx, or an answer that could not
  be read), or applied. A `VoteTrackerPoker` request whose `vote` is
  missing or null fails with `tracker_request_invalid` before anything is
  sent; a vote of 0 is a vote. A write that may have happened makes the
  issue's following pump read the votes at once, including after a read
  that was in flight when the write landed.
- **The context token** the add-on needs is per issue and lives about 15
  minutes. The Jira client keeps it in memory only (at most 16 issues),
  reuses it until 60 seconds before it expires, and drops it when the
  gateway refuses it. It is never logged, stored, put in an error, or
  sent to a client: an invoke answer that repeats it, however spelled, is
  refused with none of its text (see [tracker clients](tracker-clients.md#planning-poker)).
- **Cost.** A follower's ticket costs one invoke every 5 seconds while
  its panel is shown: 720 an hour per open panel, on top of the ticket's
  own reads. The first read of an issue, and one every 14 minutes after,
  adds the issue read and the token request; the first read on a
  connection adds the cloud ID and the listing, and the listing is read
  again at most every 30 minutes. A write is one invoke, plus the
  token's two requests when none is kept, plus the read it triggers.

### The patch read

A client patches the sections it holds instead of reading them again.
`BoardPatch` (store `ReadMirrorBoardPatch`) reads, in one snapshot, the
board's lifetime and revision, every section total unless the client holds
the board's current ones, and for each section asked the named issues'
rows that are members of it now, with their names. Membership comes from
the same rule fragments that `ListMirrorSection` pages by. The totals are
named by the board's lifetime and totals revision (the board view's
`totals_revision`, a patch's `totals.revision`), which moves with every
change that can change a total and not with a title or rank. The client's
rule for applying a patch is the `BoardPatch` comment
(`trackerapp/patch.go`), and its reference implementation is `applyPatch`
in `trackerapp/patch_model_test.go`. A randomized test checks it against a
full read of the board. A one-row change of a realistic board, with three
sections held, costs a 166-byte event and a 1,174-byte patch as JSON, or
1,487 bytes when the totals moved. The patch read is two store calls:
`GetMirrorRoot`, one statement, and `ReadMirrorBoardPatch`, five
statements for three sections with the totals held and eight with the
totals; with the totals held it reads only the named rows. Tests pin
the bytes, the calls and `ReadMirrorBoardPatch`'s statements. The full read is only for the first paint, a reconnect and
`ReadAgain`.

### Thread links

A thread links to at most one ticket (`thread_tickets`), named by kind,
site and issue ID, so the link means the same ticket on every computer.

- **The link reader.** A site's links are read, written and kept by one
  connection here, the site's link reader: its lowest connection ID
  turned on (`ticketSiteReaderSQL`). The row's state and `connectionId`,
  `TicketConnection`, the copy, the missing mark and the links' keep
  (`mirrorLinksSQL`) all read that one expression, so a second account of
  the site neither writes a link nor keeps or polls an issue only a link
  names. A link the reader cannot see reads not found, whatever another
  account of the site can see.
- **What a row shows.** A thread row computes its link when it is read,
  on the computer that answers, from the link reader's mirror
  (`threadTicketLinkJSON`): while that mirror holds the issue
  (`mirrorIssueHeldSQL`: a card, and no read that missed the issue began
  after the card's latest read) the row shows the card's key, title and
  status, the status's name and group from that root's catalog, and
  `current`; while a read that missed it is in force
  (`mirrorIssueGoneSQL`) it shows `not_found`; otherwise the link's
  last-seen copy and, from the site's connections, `not_checked` (one is
  on), `turned_off` (all are off) or `no_connection`. `connectionId` is
  the link reader. A reader change, a detached root or a restore needs no
  write: the row reads whichever mirror reads the site now, and a lower ID
  turned on reads not checked until its first linked read.
- **The copy.** The link's key, title and status (ID, name, group) are
  what the row shows while the reader's mirror holds no card. One
  statement writes it (`linkCopySQL`), run by the setter and by every
  card write that stores a card or clears an absence
  (`storeMirrorCardsTx`: row pages, card reads, ticket reads; a card the
  mirror keeps from a later read is not stored, so it copies nothing),
  and by a catalog write that renames or regroups a status a linked card
  has (`mirrorStatusLinkCopySQL`). With no link of the site, or on a root
  that is not the reader's, the copy parses no bound list
  (`mirrorReaderIssuesSQL`). A card write that stores cards runs two link
  statements, the absence clear and the copy; one that stores none runs
  the clear alone, which stops at its guard while the root has no
  absence.
- **Absences.** A read that does not return a linked issue records an
  absence at its read (`MarkMirrorIssuesMissing`,
  `tracker_cache_issue_missing`), unless a card is from a later read, and
  names the links it makes not found. A card write deletes the absences
  its read supersedes (`mirrorAbsencesClearSQL`), so a missing issue read
  again is current in the write that stores it. The cleanup deletes the
  absences of issues no link keeps.
- **The row.** `threadColumns` carries the link as one JSON column
  (`threadTicketLinkColumn`), guarded by one `EXISTS` on
  `thread_tickets` that runs once per statement, and each row probes
  its link by thread. The whole-catalog lists (`ListThreads`,
  `ListThreadsWithItems`, the sidebar's) compute every link once in a
  materialized CTE (`threadListSelect`), which each thread probes by key
  (`BenchmarkListThreadsLinked`); a list of part of the catalog (a
  project's, the archive's) reads the column per row, so its link work is
  its own rows' (`TestSelectiveThreadListsProjectTheirOwnLinks`,
  `TestThreadLinkColumnReadsByKey`).
- **Writes.** `SetThreadTicket` (bound, `threads:operate`, and
  `tracker:read` since it reads the ticket) links through
  `trackerapp.Service.LinkThread`: the connection `TicketConnection`
  names, the card the mirror holds with no request when it holds the
  issue, otherwise one card read stored with the link in one transaction.
  The store's transaction checks that the root's connection is still the
  link reader (`ErrThreadTicketNotReader`, error code
  `thread_ticket_connection_changed`) and that the mirror holds the issue
  after the read card is stored (`ErrThreadTicketNotHeld`; for a read,
  error code `thread_ticket_not_found_since`), so a link through an
  account that lost the site, and a cached link of an issue a later read
  missed, are refused. A refusal leaves the link and the mirror as they
  were. `ClearThreadTicket` unlinks;
  `ThreadsForTicket` (`threads:read`, route selected) lists a ticket's
  threads by the issue index, as search lists threads: an ask's copy and
  a side chat before Keep, which carry their source's link, are left out.
- **Frames.** A method announces its own thread's row. Every other
  thread whose link a write moved (a stored card of its issue, whatever
  of the card changed, a cleared or recorded absence, or a renamed or
  regrouped status of its card), and every thread linked to a site
  whose connection was added, turned on or off, or removed
  (`ListThreadsLinkedToSite`), is told to `Deps.LinksChanged`, which the
  app announces on one serial queue (`announceThreadLinks`), one job per
  call; the announcer has no batching, so it is one `thread:updated`
  frame per thread.
- **Keep.** A link keeps its issue in the mirror (`mirrorIssueKept`).
  When links end (an unlink, a link replaced, a thread's delete commit,
  a move away), `LinksEnded` tells the issues no longer kept as
  `Changes.Visibility`, as an open ticket's drop does.
- **Fork and transfer.** `CreatePointerFork` copies the link in its
  transaction. A transfer's manifest carries `store.Thread`, link
  included; `CommitIncomingThreadTransfer` replaces the receiver's link
  with the carried ticket, key, title, status, origin and time, never a
  carried state or connection; the row shows the card a mirror here
  holds, if any, with no write. A receiver too old to read the link
  drops it; a sender too old to carry one leaves the thread unlinked.

A delete claims its family before it reads the family's links, and every
link write checks the claim in its transaction, so a link set during a
delete is refused and the links the delete read are the ones it ends.

## Agent tools

The tools read, search, create, edit, move, comment, move between sprints
and list and add worklogs, through `trackerapp`. A worklog an agent adds is
an entry in a time draft. Only a draft's posting path sends a draft's time
to a tracker, and it sends exactly the entries it is given: the entries
the user approved (one entry, a day or the whole draft), an entry the user
logged by hand, or the entries of a day with no open question when the
project's setting posts such days. An entry an agent made is never posted
on the agent's call. No tool reaches the provider's worklog write or
`trackerapp`'s worklog writes, which send time for any caller.

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

Built: `internal/tracker` and the two providers, `internal/keyfile`, the
tracker tables and their accessors, `trackerfake`, and `trackerapp` with
its sync, pumps, reads and writes; the connection, board, ticket, write,
form-read and planning poker bound methods, the ticket file routes,
`tracker:board_updated` and `tracker:issue_updated`, the app's calls at
start, the harness's fake trackers (`HarnessTrackerSeed`), and the Issues
settings screen (Settings > Issues: Connections and Boards), with the
`bindings.ts` wrappers of the connection methods, the two picker lists,
`ProjectTrackerBoards` and `SetProjectTrackerBoards`, and their stores
(`stores/trackerConnections.svelte.ts`,
`stores/trackerProjectBoards.svelte.ts`), and the Issues pane's Board
screen, with the `bindings.ts` wrappers of `TrackerBoardChoices` and the
board subscription's methods and `tracker:board_updated`'s TypeScript
type, and its ticket screen, with the wrappers of the ticket
subscription's methods, `TrackerComments`, the issue and comment writes,
`TrackerAssignableUsers` and `TrackerChoices`, and
`tracker:issue_updated`'s TypeScript type (`components/issues/AGENTS.md`).
A failure of the linked-issues
loop after boot reaches only the log (`Deps.LoopFailed`): its failures
are outside the tracker (a store error, the cleanup), and a tracker
failure of the loop's sync shows on its connection's row as its problem.
Not built, owed by the app layer:

- The ticket screen's attachments, links, planning poker and Log time,
  and a ticket in a pane of its own: the `bindings.ts` wrappers of the
  attachment mints, `TrackerLinkTypes`, `TrackerLookup`, `TrackerSearch`,
  `LinkTrackerIssues`, `UnlinkTrackerIssues`, `AddTrackerWorklog` and the
  planning poker methods ship with them.
- The Time screen's bound methods: `Worklogs`, `RefreshWorklogs`,
  `EditWorklog` and `DeleteWorklog`. `EditSprint` is bound when a screen
  edits a sprint.
- Sidebar search by a linked ticket's key and title: the frontend's
  `threadMatchesQuery` (`components/sidebar/threadSearch.ts`) adds
  `ticketLink.key` and `ticketLink.title` to what it matches.
- A status renamed or moved to another group in the tracker reaches a
  link's copy at the ticket's next card change, not at the catalog write
  ([Thread links](#thread-links)).
- A link on a thread with nothing sent does not keep the thread: clearing
  its composer deletes the thread, link included. The slice that first
  lets a person link such a thread decides it.
- The project's tracker setup: one file in the project's app directory
  (`project.ConfigDir`), beside its workflow profile, that `trackerapp`
  reads and replaces whole, holding status-group overrides (a status's
  group, or left out of counts), pinned fields and sections, card marks,
  Start work defaults, draft settings and what agents may do. Anything a
  query needs from it is copied into the mirror, never the other way.
- The time-draft posting path (Agent tools).
- Start work and the agent tools (`internal/trackertools`).
