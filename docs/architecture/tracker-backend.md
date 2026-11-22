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
  writes and attachments, and whether an issue's update time moves with
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
- Adding a connection creates the row and the empty key file. Opening the
  tracker's token page, opening the key file on that computer and
  verifying (one identity call to the tracker) are separate calls. The row
  records who the token belongs to and when it was verified. A Linear client is bound to the
  connection's workspace: every key it reads is checked before any request
  carries it, and a key for another workspace fails every read and write.
  Tempo's API has no read that names the Jira site a token belongs to, so
  a Tempo token from another site is not detected. No tracker reports when
  a token expires, so the
  expiry is the date the user enters from the token page. A status call returns
  the file's state, the last verification and the last failure, never the
  token.
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
  turn off and removal and in the linked-issues loop, and a drain that a
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

All tracker tables are created by one migration while the feature is
unshipped, so a renumbering after a merge changes one version constant and
one frozen hash.

Durable records, restored and kept like other application records:

| Table | Holds |
|---|---|
| `tracker_connections` | Kind, site, account, the Jira connection a Tempo connection belongs to, label, on or off, verified identity and time, and the token expiry the user entered. |
| `project_tracker_boards` | Which boards an app project lists, in order, and which opens first. Keyed by project, connection and board; the project and the connection cascade. |
| `thread_tickets` | One ticket per thread: tracker kind, site and issue ID with its key and title as last seen. Keyed by thread, cascading with it, cleared when a thread retires to a holder. It names the site, not the connection, so a link survives a connection that is removed and added again. |

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
| `tracker_cache_issues` | Cards, one per issue per mirror. |
| `tracker_cache_board_issues` | Which issues are on which board, each with that board's order key and estimate. |
| `tracker_cache_issue_sprints` | Which sprints an issue is or was in. |
| `tracker_cache_changes` | Status, sprint, estimate and assignee changes with their times and authors. The issue row holds the time from which they are complete. |
| `tracker_cache_issue_details` | The detail of recently opened issues, one JSON document each. |
| `tracker_cache_worklogs` | Logged time by issue, author and day. A Tempo connection has no mirror of its own; its worklogs are in its Jira connection's. |
| `tracker_cache_worklog_days` | The latest read of an author's day, an unchanged or empty day included; a replace from an earlier read leaves the day. |
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

A stored detail is current only when the tracker says so in the read that
shows it (`IssueDetail`, or an open ticket's read). Where the issue's
update time moves with its detail, that is one cards read of the issue
alone, stored as the card resource, in which the issue's update time
equals that of the document's card. The detail is read and stored instead
when there is no document to check, when the mirror's card is already
newer than the document's, when the document's read began more than 30
minutes ago (its moves and field metadata come from the workflow and
screens, which move no update time), on a person's refresh
(`RefreshIssueDetail`), and where the update time does not move with the
detail. No document is written outside such a full read. A view shows its
parent's key and title and each linked issue's key, title and status from
the mirror's cards, read with the issue's own card in one
`ListMirrorCards`. A linked issue the mirror keeps no card of shows as the
document holds it, at most 30 minutes old; a parent it keeps no card of
shows its ID alone, since the document names the parent by ID. The opening is
recorded only while the mirror still holds the document checked, the same
row from the same read with no sweep since (`ConfirmMirrorDetail`);
otherwise the detail is read again. A failed check leaves the stored
detail as it is. `MirroredDetail` returns the stored detail and when it
was read, with no tracker call, for a first paint before the check
answers; a detail another read rewrote between its read and its opening
is read again, so it answers not mirrored only when the mirror holds
none.

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
new row from a read that began before the board's latest removal is
refused, since that removal may have been of it, and so is an absence a
read by ID found when the read began before the board's newest complete
listing, which may have named the row; each is read again: a delta page
with one stores no cursor, and a read by ID reads it once more
(`MirrorChanges.Refused`). A card is ordered by its update time and, for
one update time, by its `read_at`, the latest read admitted for the card
or its sprints, identical or not: a project key rename changes every key
and moves no update time. An issue's row on a board records whether a
listing has named it (`listed`): a delta page, which is a search of the
same index, or a listing a reconcile was given. A
section page carries the board's revision; continuing it at another
revision fails with `ErrTrackerCacheStale`. A board view's first paint
reads structure, totals, first pages and what their cards name (statuses,
users and parents) in one snapshot (`ReadMirrorBoardView`), so its totals,
rows and names are one state; a section page reads its names with its rows
(`ListMirrorSection`). A card's
status time is derived from the card and the stored history by every write
of either. While no final history read for the card's update is stored,
the time stands as long as the card keeps its status and is unknown once
the status changes: an edit to another field does not blank it. An issue
that left its status and came back between two reads of its card shows
the earlier stay's time until that history is stored. A detail's read time starts at its opening, so a document read
before the opening is superseded.

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
  and removes the rows no longer listed that a listing has named and that
  were read at least `tracker.DeltaOverlap` before the listing began. A
  list may miss an issue its index has not caught up with (Jira, after a
  create), so an unlisted row read more recently, or one no listing has
  named (a row only a read by ID placed, such as a create's), is not
  judged by the listing: the store reports it
  (`MirrorReconcile.Unsettled`) and the pass reads it through the board's
  rows, which keeps or removes it. A pass attempts
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
  again once), so a miss never removes a row on the list's word alone. A listing the
  provider reports as incomplete adds and never removes, and so does one
  that began before the newest complete listing the board reconciled,
  which saw the rows it leaves out (checked in every removal
  transaction). A read by ID that began before that listing removes no
  row the listing may have named: its absence is read once more, from
  after the listing (`MirrorChanges.Refused`). One statement finds the
  rows to remove; they go in transactions of at most 500, each judging
  them again. It runs at a board's first read, on its interval, while a
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
full read), which alone record an opening, and rebuilds the ticket's view
with no tracker call when an input of it other than the document moved.
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
it due at once, for a full read. The round's timer is the earliest due
time of the shown tickets, and it reads every shown ticket then due in
one read, so one delta that changes N shown tickets costs one read. A
hidden ticket keeps its due time and is read when shown if it passed.

A ticket's view is built from its document and from what the mirror
holds of other things: the mirror's card of the issue and its status
time, the cards of the issues the view shows (its parent, the issues it
links) and whether the mirror keeps them, and the connection's users and
statuses. A full read stores its card before its document
(`run.details`), so the mirror's card of an issue is never behind a
document read of it: the store alone decides the status time, and the
issue's history is due for the card the ticket shows. A document the
ticket settled last, or of the version the round's check just stored the
card of, brings no card to store. Reads settle documents in the order they began; the funnel
orders changes to the other inputs. A view is installed only when it was
built from the document the pump holds now, after the last change to the
other inputs; one that misses that is built again, so a delayed read or
build never replaces a newer view. Whether the view is loaded is part of
what is told: a ticket whose content neither the mirror nor the pump
holds any more is told not loaded and read at once, and told loaded when
the read holds it again, the same content included. A subscriber answered
not loaded is told the same way; finding it not loaded again leaves the
read in flight standing. When another read stores the issue's document in
the mirror while the pump holds one, the view is rebuilt from whichever
read began later, and the pump lets go of an older one; while a read of
the ticket is in flight, that read settles the document with its outcome
and the view is rebuilt when it ends, so no view shows a document with
the failure of the read that stored it. A read succeeds only once the tracker
answered and the document it checked is confirmed where its check says
(the mirror's copy of the same read, or the pump's while the mirror holds
none); one the cap dropped or the cleanup swept meanwhile is read in
full. A read of several tickets settles each with its own outcome: a
ticket whose check confirmed its document, whose document was stored or
held, or that the tracker left out is settled so whatever happened to the
others, and the read's failure is told only to the tickets it did not
answer, the ones a rate limit holds due when it lifts. A failure is told
to each ticket once and cleared by its next success.

Idle ticks write only read times, and send no event. N is the number of a
connection's shown tickets:

| Tick that finds nothing changed | Requests | Store calls | Statements | Rows written |
|---|---|---|---|---|
| Shown board | 1 | 2 | 3 | 1 |
| Shown board, the delta's overlap redelivering one identical row | 1 | 3 | 11 | 4 |
| Jira round of N tickets (N up to 100) | 1 | 3 | 9 | N |
| Linear round of N tickets | one per 10 tickets, plus each read a ticket's states, history or labels need past their first page | 2 | 4 | N |

A Jira round checks every ticket with one cards read, decodes no stored
document, reads none in full and moves the read time of the unchanged
cards in one statement; a Linear round reads every ticket in
full, stores them in one transaction, which reads every document's state
in one statement and moves the read times of the unchanged ones in one
more. A ticket that changed adds its own full read, and on Linear the
store call that writes its card.

A changed row is redelivered by the delta's overlap of one minute
(`tracker.DeltaOverlap`), so about six Jira ticks after a change take the
redelivery row's cost. The trackerapp cost tests pin the provider and
store calls, the providers' cost tests the requests, and the store's
`TestIdleTickStatements` the statements and row writes.

### Costs at scale

Three tests measure one board of 5,000 and of 20,000 issues, and five
boards sharing a root, when `AO_TRACKER_SCALE` names the sizes:
`TestMirrorAtScale` in `internal/store` (the store's statements),
`TestServiceAtScale` in `internal/trackerapp` (the service over the
scripted provider) and `TestFakeTrackersAtScale` (the real providers and
clients against `trackerfake`). The command that runs one is in the
comment above `TestMirrorAtScale`
([`tracker_cache_scale_test.go`](../../internal/store/tracker_cache_scale_test.go)).

Counts are exact. Times are one development machine's (Intel Core Ultra 9
285HX, WSL2, Go 1.27.4): the first sync's is the fastest of three runs, a
path's the fastest of three runs of five each.

| First sync, one board | 5,000 | 20,000 |
|---|---|---|
| Ticks | 25 | 100 |
| Provider calls (scripted) | 172 | 682 |
| Requests, Jira through `trackerfake` | 239 | 944 |
| Requests, Linear through `trackerfake` | 166 | 615 |
| Write statements, in transactions | 739 in 178 | 2,914 in 703 |
| Allocated, peak heap | 90 MB, 9.8 to 10.3 MB | 358 MB, 29.0 to 29.4 MB |
| Time (scripted) | 1.28 s | 5.25 s |
| Store file per issue, after a checkpoint | 925 B | 867 B |

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
| Idle tick | 3 | 60 µs | 63 µs |
| Delta, 100 changed | 11 | 6.7 ms | 9.2 ms |
| History, 50 due | 6 | 11.2 ms | 11.3 ms |
| Reconcile, complete listing | 3 | 4.3 ms | 22.4 ms |
| Board view, 3 sections | 10 | 11.2 ms | 44.8 ms |
| Section page, middle of a column | 3 | 0.70 ms | 0.72 ms |
| Section counts | 4 | 8.4 ms | 39.2 ms |
| Patch read, 1 or 100 changed, totals moved | 8 | 8.7 to 9.9 ms | 40.6 to 41.2 ms |
| Patch read, 1 or 100 changed, totals held | 5 | 0.07 to 0.73 ms | 0.07 to 0.76 ms |
| Ticket open, stored | 14 | 0.29 ms | 0.33 ms |
| Jira round, 100 tickets, 10 changed | 21 | 5.4 ms | 5.5 ms |
| Linear round, 100 tickets, 10 changed | 20 | 4.2 ms | 4.2 ms |
| `New` | 0 | 13.8 ms | 15.2 ms |

A reconcile whose listing leaves out a quarter of the board removes
those rows in 8 statements at 5,000 issues and 22 at 20,000. Five boards
of 4,000 issues on one root view in 10.6 ms. Every
statement of the mirror plans the same at 20,000 issues as on a small
mirror, with no `sqlite_stat1`. Through `trackerfake` at 20,000 an idle
tick, a delta of 100 and a Jira history read of 100 are 1, 1 and 4
requests; a reconcile is 4 Jira requests or 41 Linear ones.

Retention does not grow with a board, except a reconcile pass that
spans ticks: its pump holds the candidates left, at most one listing's,
until the pass ends. Five shown boards and 50 shown
tickets hold 123 to 145 KB of heap and 56 goroutines (each pump and the
round with one timer) at both sizes; once released, 65 to 88 KB stay
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

The connection methods are bound (`internal/app/app_tracker.go`): each
takes a leading context and carries a scope, `tracker:read` at the
observe tier or `tracker:operate` at the execute tier, and a route, and
their event is `tracker:connections_updated`. Every other method below is
a `trackerapp.Service` method that no bound method wraps
(see [Build order](#build-order)).

A board answer is bounded: the subscribe returns the board's structure,
its sprints and the cards of the sections on screen, and a section's
remaining cards are read by page.

### Subscriptions

- `SubscribeBoard(projectID, scope, sections)` answers from the mirror
  alone, before any tracker call: a handle, a sequence number, the view of
  the sections or "not loaded", and the poll interval. It is refused for a
  board the project does not list, a connection turned off, removed or
  Tempo, and past 64 board handles.
- `SubscribeTicket(connection, issue)` answers the same way with the
  ticket's detail when the mirror or the ticket's pump holds one; at most
  64 ticket handles.
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
  its failure, and its view or not loaded.
- `BoardPatch(scope, sections, issues, held)` is the patch read below;
  `held` names the totals the client holds.
- `StartLinkedSync` arms the linked-issues loop. `New` starts nothing.

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
  its view shows, the users and statuses) rebuilds its view with no
  tracker call. A ticket detail read outside a subscription
  (`IssueDetail`) publishes what it stored too.
- The root a change is published under is the one the run that stored it
  held, so a connection change during the read cannot lose it. A change
  reaches only the subscriptions of that mirror: the connection's mirror
  generation moves when it is turned off or removed, a run reads it with
  its root and a pump when it is made, and the funnel tells only the
  pumps of the run's generation. A change stored under a mirror a turn
  off detached reaches no subscription made after the turn on.
- A write's refresh publishes once, with the changes of each board it read
  through as that board's own, and settles the open tickets it read
  itself instead of nudging them.

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
its sync, pumps, reads and writes, and the connection bound methods. Not
built, owed by the app layer:

- The app's calls at start: nothing calls `StartLinkedSync` or
  `FinishMirrorCleanup`, so the linked-issues loop does not run, and the
  rows of a drain a crash cut short, a retired board or an issue an unlink
  left unkept stay until the next turn off or removal.
- The board, ticket and write bound methods, routed by project when the
  first parameter is `projectID` and `selected` otherwise, a client
  pinning a subscription's calls to the computer that answered the
  subscribe and releasing its handles when it goes; and the
  `tracker:board_updated` (`trackerapp.BoardEvent`) and
  `tracker:issue_updated` (`trackerapp.IssueEvent`) channels, with the
  read scope's policy. `Deps.EmitBoard` and `Deps.EmitIssue` only log.
- The sequence that follows adding a connection: open the token page,
  open the key file, verify. The three bound methods exist; the sequence
  is the frontend's.
- The warning before a token lapses, from the expiry the user entered,
  its only source.
- A thread's ticket link carried by a fork, which starts with its
  source's link, and by a transfer, whose header carries it and whose
  receiver replaces the link it held (a receiver too old to read it drops
  it). The table and its accessors exist; nothing writes it.
- The project's tracker setup: one file in the project's app directory
  (`project.ConfigDir`), beside its workflow profile, that `trackerapp`
  reads and replaces whole, holding status-group overrides (a status's
  group, or left out of counts), pinned fields and sections, card marks,
  Start work defaults, draft settings and what agents may do. Anything a
  query needs from it is copied into the mirror, never the other way.
- The time-draft posting path (Agent tools).
- Start work, the agent tools (`internal/trackertools`), and the harness
  and browser test wiring.
