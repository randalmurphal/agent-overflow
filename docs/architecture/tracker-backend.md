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
| `internal/trackerdash` | A board's dashboards: the metric contract, the sandboxed evaluation over the board's export, the reductions, ranges and drills, and the built-in dashboard ([Dashboards](#dashboards)). |
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
- Development: the pull requests a tracker's forge integrations link to an
  issue (`PullRequests`), from Jira's undocumented development data, each
  integration read on its own: the rows read come back beside
  `errors.Join` of each failed read. A Jira site whose summary says it
  keeps no such data fails wrapping `tracker.ErrNoDevelopmentData`;
  Linear fails as unsupported before any request.
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
  worklogs otherwise, a Tempo connection turned off included. The mirror
  keeps the worklogs of one source. A change of source (the Tempo
  connection added while on, turned on or off, or removed while on)
  advances the Jira row's `time_source_gen` and empties its worklogs and
  read days in the same transaction (store triggers, v168); adding or
  removing a Tempo connection that is off changes nothing. A run captures
  the generation with the row its provider is built from
  (`providerLocked`), and a replace under an earlier generation is
  refused in its transaction, however many changes came between, so the
  refresh answers that the time tracking changed and to refresh again.
  An edit or a delete is sent only while the worklog is the copy the
  caller read, at the tracker revision the copy carries (`updated`,
  compared at the millisecond); one that changed since, in a field the
  copy shows or not, is refused as stale with nothing sent.
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
narrows the window and does not close it. On Linear a sprint move with a
place reads the moving issues' cycle and order and the neighbors' in one
request, checks every part, and then writes the cycle and the order in
one family of aliased updates (`linearprovider` `MoveToSprint`,
`linear.RenumberIssues`): a refused part leaves nothing written.

A create lands at its section's foot. The service passes the last row the
mirror holds of the section (`CreateInput.After`, store
`LastMirrorPlanRow`, which walks the board's order index back from its
end); Linear's create carries an order just after it, where Linear's
default puts a new issue above every other
([measured](../references/tracker-api-measurements.md#linear)), and Jira
needs none, since a new issue ranks last.

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

## Project setup

A project's tracker setup is one file, `tracker.json` in the project's
app directory (`project.ConfigDir`), which `trackerapp` reads and
replaces whole (the comment at the top of `trackerapp/setup.go` is
the contract). The file is the only copy: nothing of it is in SQLite, and
no step runs at root creation, restore or start.

- **Sections** are the keys of one JSON object, each with its own
  `version`: `status_groups` (by tracker site, the group a status counts
  in: one of the five tracker groups, or `left_out`), `ticket_pins` (by
  tracker site, the fields and the sections a ticket shows first; a
  Linear site pins no fields, and a site entry that pins nothing is
  dropped), `start_work` (by tracker site and
  tracker project, named by its key prefix, `WEB`: the status Start work
  moves to, whether it assigns, the branch template with `{key}` and
  `{title}`, whether the branch is named after the ticket
  (`name_branch`, false for an empty branch box, stored only when
  false), and the repository, an app project of this computer; each field
  optional, and an entry that sets nothing dropped), `agents`
  (`change_tickets` and `ask_several`, each true when absent and stored
  only when false, and the section dropped when it turns neither off;
  [Writes by agents](#writes-by-agents)) and `time` (`meetings` and
  `time_off`, each a Jira ticket by site and key, `WEB-1`, never an issue
  ID, so a client saves what a finder answers with no read; and
  `never_epics`, true when absent and stored only when false; the
  section dropped when it sets nothing; it applies only while the
  project lists a Jira board, and `ProjectTimeSettings` answers the
  built-in settings otherwise, with the file keeping the section). The writer (`validateSetup`)
  owns these defaults: a client sends the value it shows. Beside them,
  `dashboards` (by tracker site and board, the dashboards the project
  keeps) is written only by its own edits ([Own dashboards and
  proposals](#own-dashboards-and-proposals)); a replace keeps it. A section this build does not know, or one
  at a newer version, is kept byte for byte by a replace and named in the
  read's `newer`; a replace that sets it is refused.
- **A site** is named as its connection names it, by the rules a
  connection's site is read by (`jiraSite`, `linearSite`): a read and a
  save store `https://example.atlassian.net` for `Example.atlassian.net/`
  and `example` for `https://linear.app/example/`, so the entry is found
  by the board handles of every connection to the site. A site that
  neither rule accepts, or one an entry names twice in any spelling, is
  refused, and a file holding one cannot be read. A file written by hand
  in another spelling keeps the revision of its bytes; the next save
  writes the site as its connection names it.
- **A replace's setup argument** is read whole or refused (`Setup`'s
  `UnmarshalJSON`): null, anything but an object, a section this build
  does not read and a field it does not know are refused as an argument
  the computer cannot read (`bad_params`, before the method runs), as the
  file's reader refuses a field of a section it reads; `{}` is the empty
  setup.
- **A read** answers `{revision, setup, newer}` (`SetupView`, also a
  replace's `stored`): `setup` is the value a replace takes, so a screen
  saves the setup it read, changed, with the revision beside it; the
  revision and the newer sections are not part of it, and a replace that
  sends them is refused as an argument the computer cannot read.
- **A replace** names the revision it was built from, the SHA-256 of the
  file's bytes (empty for no file). When the file moved since, nothing is
  written and the answer carries the setup stored now. An empty setup
  removes the file. The write is atomic (`atomicfile`, 0600), and both a
  write and a removal sync the directory. A write or a removal that fails
  fails as `tracker_setup_unsure`, which says the change may or may not be
  stored, names the file and why, and says to read the setup again before
  changing it again: a failed directory sync follows the rename
  or the unlink, and `atomicfile` does not tell it from an earlier step's
  failure.
- **A file that cannot be read** or does not validate is refused with its
  path and why, never read as empty and never overwritten. It refuses
  that project's reads, saves and board subscribes, and no other
  project's; the next use reads it again.
- **The service holds a project's setup** from its first use, by the
  project's slug, under `setupMu`: the file as the service last saw it. A
  save holds what it saw: the file it read, refused or not, or the setup
  it wrote. A save that cannot read the file, or whose write may or may
  not have happened, holds nothing, so the next use reads the file; the
  project's boards read again and the change is told, as for any change
  of what is held (`setupMoved`). A file edited by hand while the app
  runs is read by the next save or the next start.
- **Status groups apply to a board read** as a bound argument
  (`store.MirrorRegroup`), resolved from the board handle's project and
  its connection's site, so two projects that list one board each read it
  with their own groups. A save that changes a site's groups sends
  `ReadAgain` to the boards the project has open on that site; the event
  is the board's, so every subscriber of such a board reads again. Each set of groups has a
  version, which the board view, its section cursors and the totals a
  patch names carry: a page or a patch read with another version is
  refused as `ErrStalePage`, so no section mixes two versions. The board
  view also carries those groups (`regrouped`: each status the project
  puts in another group than the tracker's, with that group). A count the
  client makes from rows it holds, such as a sprint board's columns
  (`screenColumns`) or an active sprint's done tickets
  (`sectionNumbers`), takes each row's group from `groupOf` in
  `frontend/src/lib/stores/trackerBoardModel.ts`. A status mark draws
  `markGroup` (`frontend/src/lib/stores/statusMarkGroup.ts`) over the same
  `regrouped` on a board, and over the project's held setup elsewhere
  (`trackerRegroupsOf`): no view carries a project's group for a status,
  since a ticket's view is every subscriber's whatever its project, and a
  thread's link is derived in SQL. The Complete sprint form counts by
  Jira's rule for its completion instead (`completionNumbers`), over the
  board view's columns.
- **The statuses the Status groups page lists** (`ProjectStatuses`,
  `trackerapp/setup_statuses.go`) are read from this computer's mirror
  and setup, with no tracker request: by tracker site, whatever
  connection reaches it, each listed board's column statuses in board
  order, each once, the statuses the setup regroups that no listed board
  shows, and the listed boards the mirror has not read, each board once
  by name. Beside the sites it lists the places Starting work sets
  defaults for (`places`): each tracker project a listed board names, in
  picker order, then each the setup holds defaults for, with the IDs of
  the statuses Move the ticket offers it and the unread boards that
  would add to them. A Linear team offers its own boards' statuses (two
  teams share a site); a Jira status is the site's and a Jira board can
  show several projects' tickets, so a Jira project offers every status
  the project's boards on its site show, those of its own boards (the
  boards whose tracker project it is) first, so the first in progress is
  of its own workflow, and Start work checks the ticket's own moves. A board
  is one statement of its column statuses (`GetMirrorBoardStatuses`, at
  most 200 rows), not its structure: no sprints or board data. Names
  come from the root's catalog, looked up 500 at a time. With 20 boards of
  12 columns of 5 statuses and 250 sprints each, a catalog of 1,000
  statuses and 30 regrouped, one read took 1.2 to 1.5 ms, 0.8 MB and
  11,700 allocations over three runs, against 10 to 24 ms, 4.9 MB and 82,300
  through the boards' whole structure
  (`BenchmarkProjectStatuses20Boards`). A client reads it only while the
  page shows a project: on the page's show, after `tracker:setup_updated`,
  on reconnect and on Check again.
- **The fields the Fields page lists** (`SiteFields`,
  `trackerapp/project_fields.go`) are one Jira site's, one read per
  site: the page works out the sites itself, from the project's boards,
  the computer's connections and the setup's `ticket_pins` it already
  holds, so a pin saved elsewhere or a connection removed shows with no
  read. A site's read goes through the first connection to it that is
  on; a site whose connections are all off is `ErrDisabled`, and one no
  connection reaches is `NoSiteConnectionError`, which the app words as
  `tracker_site_unreached`. Each read has its own deadline, error line
  and Try again, so a site that fails or stalls holds up no other. A
  site's fields are the provider's `Fields` as `{id, name, kind}`, the
  site's field catalog by the rule a ticket's detail lists fields by
  (`jiraprovider` `site.listed`), which the client holds after its first
  read. A Linear site lists none. A real site's catalog holds several
  hundred fields; the page draws the pinned ones and, only while a name is
  typed, at most 20 of its matches, ranked on the name. A client reads a
  site only while the page shows it: on the page's show, on reconnect, on
  Try again and when the connections that reach the site change.
- **The tickets the Time page shows** are checked with `TrackerLookup`,
  no method of their own: each ticket set through one connection that is
  on and reaches its site (a connection of the project's boards first),
  every ticket of a connection in one lookup, so a page with both tickets
  set on one site is one request, and a key no issue answers to is one
  more (the lookup reads it alone to tell an old key from a gone one). The
  lookup gives the title, and its `unmatched` is a ticket that is gone or
  that the connection cannot see, which Jira does not tell apart. A
  client checks only while the page shows the tickets: on the page's
  show, on reconnect, on Try again and when a ticket changes.

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
such a full read. A view shows its parent's key and title from the
parent's name (below), and each linked issue's key, title and status from
the mirror's cards, read in one `ListMirrorCards` with the issue's own
card. A direct read shows the
mirror's card of the issue as that lookup finds it when the card is at
least as new as the body, and the document's own card otherwise; a Jira
check's write commits before the lookup, so a checked read shows the card
the check stored. An open ticket's view, built from the record, takes its
own card from the statement that read its body. A linked issue the mirror
keeps no card of shows as the body holds it, at most 30 minutes old; a
parent with no stored name shows the name the document's read gave, and
its ID alone when that read named it by ID alone. A failed check leaves the
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

A parent's name, its key, title and type level, has one owner:
`tracker_cache_parents`, one row per parent a stored card names, whether
or not the mirror keeps the parent. A board's cards, a ticket's parent,
`ticket_read` and the dashboard export's epics all read it; a ticket's
view whose parent has no row shows the name its own read gave. The card
write writes it in the transaction of the cards, from every card it is
given, whether or not the mirror keeps the issue: each card names its
own issue, and a card whose read named its parent names the parent. One
write names an issue once, by its own card when it holds one, else by
the first child's naming, and writes a row only for an issue a stored
issue names as parent. The newest read wins, a later write of the same
read with other content corrects it, and a read that names what the row
holds writes nothing. A kept parent's own card and its name are written
together by a read that carries both; a child's later read can name the
parent otherwise, and the two differ until the parent's own next read.
Jira's board lists, searches and bulk reads and Linear's pages, searches
and details name the parent
([measurements](../references/tracker-api-measurements.md)); a card that
names its parent by ID alone writes no row and leaves the one there. A
renamed parent shows its old name until one of its children or the parent
itself is read again. `FinishMirrorCleanup` deletes a row no stored issue
names, a chunk a transaction, after the sweep of unkept issues.

A mirror that v172 upgraded holds issues whose parents no read named:
before it, no row kept a parent the mirror did not keep. Such a root owes
those names (`tracker_cache_roots.names_after`, a cursor from the first
parent ID); a root made since names its parents with the cards it stores
and owes nothing. The linked tick's names unit reads the owed parents by
their own cards, by ID, 100 a read (`ListMirrorUnnamedParents`, one walk
of `idx_tracker_cache_issues_parent` per batch in a read slot), and
passes the cursor after each read is stored, so a parent the tracker does
not return is asked for once; the worklist ends at the last parent. A
tracker failure leaves the cursor, and the next tick asks again. The
names owed after an upgrade are read while a client is connected, as
every tick is.

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
view that shows it as a link is told (`Changes.Visibility`). A
pump keeps one issue; a keep of another fails. A keep that fails is the
ticket's failure, a failure outside the tracker that its next read
retries; until a keep succeeds the ticket is not read. A pump's delete
that fails stays owed: each run's start tries it again, and `Stop` makes a
last try whose failure is logged, the next process's first run deleting
what it left. So an issue may have two rows, a closed ticket's owed row
beside the reopened ticket's, and what bounds the table is that cleanup,
not the cap of open tickets (64). Rows a process left when it ended
without deleting them are deleted by this process's first run
(`trackerapp.recoverEarlier`): every run passes that step as it
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
holds or the one the reading project's status groups give it. A status the
mirror does not know yet counts as not done. A board that plans no sprints
shows every row that is not done. A status the project leaves out of
counts (`left_out`) is in no backlog and adds to no total; its rows still
show in their columns and sprints. The rule is one set of SQL fragments
in `tracker_cache_sections.go`: `mirrorNotDoneSQL` with no regrouping,
which probes a row's status by its key, and `mirrorRegroupedNotDone` and
`mirrorLeftOut` under one, which list the statuses that count as done
once a statement, from the root's catalog and the bound groups
(`MirrorSection.statement` picks the form).

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
  the next page on, or the next round, `Cadence.Linked` later. A linked
  tick runs the cards unit, then the names unit of an upgraded mirror
  (above), then the history unit; the names unit makes at most 10 reads a
  tick, so the linked issues' cards and history keep their tick, and the
  loop ticks again until the worklist ends.
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
build's tickets, `store.ListMirrorTicketRows`), the name of its parent, the cards of the issues it links and whether the
mirror keeps them, and
the connection's users and statuses (`trackerapp/ticket_view.go`). A
read stores what it found and settles its outcome; every view a
subscriber is told is built from the record, and `TicketDetail` returns
the view last built with no store call. Which boards a ticket has a row
on, and each board's unit, are the mirror's; each row's estimate is the
ticket's own read's when that read carried the board's field
(`tracker.Detail.EstimateBy`, the one rule, read only by
`trackerapp.issueRows`) and the view shows the card of the read's
version, and the board row's otherwise. A Jira detail carries every
number field its read returned, null as none, and a field outside the
issue's context is absent from the read
([measured](../references/tracker-api-measurements.md#jira-cloud)), so
that board shows its row's estimate; a Linear detail carries the issue's
one estimate when its team estimates. So after an open or a Refresh the
ticket's points and, on Jira, the rail's estimate field agree, while the
Board, the Backlog and their totals keep the board's last read until the
board reads the issue again. A card a later read brought, newer than the
body, shows with the board rows' estimates. The change funnel tells each
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
runs. A path's time is the median of one run of five, with the load
average between 3 and 8. The peak heap is
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
| Idle tick | 3 | 100 µs | 76 µs |
| Delta, 100 changed | 12 | 7.1 ms | 8.6 ms |
| History, 50 due | 7 | 6.8 ms | 6.6 ms |
| Reconcile, complete listing | 3 | 4.1 ms | 17.1 ms |
| Board view, 3 sections | 11 | 10.5 ms | 41.1 ms |
| Section page, middle of a column | 3 | 0.51 ms | 0.54 ms |
| Section counts, the pace included | 5 | 8.0 ms | 35.8 ms |
| Patch read, 1 or 100 changed, totals moved | 9 | 8.3 to 8.8 ms | 37.1 to 39.1 ms |
| Patch read, 1 or 100 changed, totals held | 5 | 0.13 to 0.59 ms | 0.07 to 0.67 ms |
| Ticket open, stored | 10 | 0.37 ms | 0.62 ms |
| Ticket rows of a view build, 16 tickets | 1 | 38 µs | 60 µs |
| Jira round, 100 tickets, 10 changed | 24 | 7.0 ms | 5.8 ms |
| Linear round, 100 tickets, 10 changed | 18 | 6.3 ms | 5.9 ms |
| `New` | 0 | 9.6 ms | 9.4 ms |

A project's status groups ([Project setup](#project-setup)) add one
list of the done statuses to the backlog, count and patch statements. No
setup file, and a file with no entry for the board's site, run the plain
statements, which probe a row's status by its key: the backlog
statement of a one-issue patch runs 99 VM steps at 5 and at 1,000
catalog statuses, and its regrouped form 168 and 12,114, counted by the
sqlite3 shell (`TestABoardWithNoRegroupingReadsNoCatalog` pins that the
plain reads do not grow with the catalog). At 20,000
issues, the fastest of three runs alternated with the branch's base,
thirty regrouped statuses the board does not use took 46.4 ms for the
board view and 39.5 ms for the counts, against 42.1 and 37.8 on the
base. A regrouping that counts a fifth of
the board's rows as done and leaves another fifth out of counts took
56.5 ms, 49.3 ms, and 48.1 ms for a patch whose totals moved: the left-out
rows' IDs reach the sprint totals as one bound list.

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
   unknown or unsent part, its failure; the issue a create made, its key
   when a later step of the create was not applied (`CreatedKey`), the
   board it is not on (`NotOnBoard`), `CreateUnknown`, and the resources a
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

A write an agent's tool call makes carries the thread and call as a
context value (`trackerapp.ByAgent`, read only by `Service.write`); a
person's writes carry none and record nothing. `Service.write` records it
in `tracker_agent_writes` (`trackerapp/agent_writes.go`): a comment or a
create is written in state `sent` before the provider call, with a digest
of its content, and every step's outcome (`applied` or `unknown`) with the
card's update times before and after is written after the refresh and
before the views are built, so the view the change funnel's event builds
already carries the mark, a create's with the key the tracker gave it. A
refused step, and an edit the tracker answers as changing nothing,
leave no row. A ticket's detail
marks a comment whose ID a row holds with its thread (`CommentView.Thread`)
and a history event by the writing connection's account inside a row's
window (`EventView.Agent`); the newest 200 rows of an issue are read. The
table is an application record: kept through restore, never pruned with
history, and a deleted thread's rows stay with an empty title.

## Methods and events

The connection methods (`internal/app/app_tracker.go`), the board
methods (`app_tracker_boards.go`), the ticket and form-read methods
(`app_tracker_ticket.go`), the writes (`app_tracker_writes.go`), the
planning poker methods (`app_tracker_poker.go`) and the project setup
methods (`app_tracker_setup.go`) are bound: each takes a leading context
and carries a scope, `tracker:read` at the observe tier or
`tracker:operate` at the execute tier, and a route. Their events are
`tracker:connections_updated`, sent when a connection or its problem
changed, `tracker:board_updated` (`trackerapp.BoardEvent`),
`tracker:issue_updated` (`trackerapp.IssueEvent`) and
`tracker:setup_updated`, a latest-only nudge with no payload sent when a
project's stored setup changed, after which a client reads again the
setups it holds, and `tracker:time_entries_updated`
(`trackerapp.TimeEntriesEvent`: the connection, author, days and drafting
threads of a change to the time draft, or every day a post's check
stored, from the first to the last of each read, after which a client
reads those days or that thread's entries again), all at `tracker:read`.

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
| `ProjectTrackerSetup(projectID)` | `ProjectSetup` | read | project |
| `SetProjectTrackerSetup(projectID, revision, setup)` | `SetProjectSetup` | operate | project |
| `ProjectTrackerStatuses(projectID)` | `ProjectStatuses` | read | project |
| `ProjectTrackerSiteFields(projectID, tracker, site)` | `SiteFields` | read | project |
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
| `TrackerIssueTypes`, `TrackerCreateFields`, `TrackerBoardProjects`, `TrackerMoves`, `TrackerAssignableUsers`, `TrackerChoices`, `TrackerLinkTypes`, `TrackerLookup`, `TrackerSearch`, `TrackerComments` | the form reads of the same names | read | connection |
| `CreateTrackerIssue`, `EditTrackerIssue`, `MoveTrackerIssue`, `SetTrackerEstimate`, `RankTrackerIssues`, `MoveTrackerIssuesToSprint`, `CreateTrackerSprint`, `StartTrackerSprint`, `CompleteTrackerSprint`, `EditTrackerSprint`, `AddTrackerComment`, `EditTrackerComment`, `DeleteTrackerComment`, `LinkTrackerIssues`, `UnlinkTrackerIssues`, `AddTrackerWorklog`, `EditTrackerWorklog`, `DeleteTrackerWorklog` | `Create`, `Edit`, `Move`, `SetEstimate`, `Rank`, `MoveToSprint`, `CreateSprint`, `StartSprint`, `CompleteSprint`, `EditSprint`, `AddComment`, `EditComment`, `DeleteComment`, `Link`, `Unlink`, `AddWorklog`, `EditWorklog`, `DeleteWorklog` | operate | connection, budget 3m |
| `TrackerWorklogs(connectionID, author, from, to)` | `Worklogs` | read | connection |
| `RefreshTrackerWorklogs(connectionID, author, from, to)` | `RefreshWorklogs` | read | connection |
| `ThreadTimeEntries(threadID, callID)` | `ThreadTimeEntries` | read | thread |
| `EditTrackerTimeEntry(connectionID, ref, edit)`, `RemoveTrackerTimeEntries(connectionID, refs)` | `EditTimeEntry`, `RemoveTimeEntries` | operate | connection |
| `PostTrackerTimeEntries(connectionID, in)` | `PostTimeEntries` | operate | connection, budget 3m |
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
- **The worklog reads** take the author's account ID (a screen reads its
  own from the connection's `verifiedAccountId`). `TrackerWorklogs`
  answers at most 366 days from the mirror with no tracker call, with
  `unread`, the days of the range no read replaced; the connection's row,
  its root, the read days and the worklogs are read in one snapshot (four
  statements, at most 366 day rows and 1,001 worklog rows). A read day
  with no worklogs is 0h only without `more`: with it, the answer holds
  the first 1,000 worklogs by day and ID, so the last worklog's day and
  the days after it are read again from that day before a screen totals
  them. It refuses
  what the refresh refuses: a Tempo connection, one turned off or
  removed, and a tracker with no time tracking (Linear, decided by kind,
  which `TestTheMirrorReadOfWorklogsRefusesWhatTheRefreshRefuses` holds
  to the providers' answer). `RefreshTrackerWorklogs` reads at most 31
  days at the default budget, as a form read does. Each worklog carries
  its issue's key and title (`key`, `title`) as that read found them,
  stored with it (`tracker_cache_worklogs.issue_key`, `issue_title`),
  both empty for an issue the tracker did not return. Its requests grow
  with the time logged: Tempo answers in pages, and Jira names their
  issues in one bulk read of the summary (500 issues a request), since
  a Tempo worklog names its issue by ID alone; Jira looks the author up
  unless its client has the user cached, pages the issues the author
  logged time on with the worklogs each embeds (up to 20) and each one's
  summary, and reads an issue with more in pages of its own. On
  `jiraprovider`'s cost fixture, a month of 20 worklogs on 20 issues,
  that is 2 requests of about 15.4 KB through Tempo (1 Tempo, 1 Jira; it
  was 1 Tempo request of 11.8 KB before the issues were named) or 2 Jira
  requests of about 29.1 KB (`TestRequestCost`,
  `TestRequestCostJiraWorklogs`). A read that names the issues and fails
  fails the refresh with Jira's sentence, the worklogs stored as they
  were. A longer range is
  `tracker_request_invalid` before anything is sent, a tracker with no
  time tracking is `tracker_unsupported` ("this tracker has no time
  tracking"), and a read the tracker fails is the answer's `failure`,
  beside the worklogs as the mirror held them.
- **A worklog read the tracker refused for what it asked** is the
  answer's failure with a sentence that names the tracker, what it
  refused and what to do: a person Jira does not have or will not show
  (its 404 for the author's lookup), a lookup refused (403, the Browse
  users and groups permission), a person whose profile hides the time
  zone that places their time on days (`tracker.ErrNoTimeZone`, which Jira
  answers with 200), Jira's 400 for the account ID or for the search
  (the provider marks the step, `tracker.ErrWorklogAuthor` and
  `ErrWorklogSearch`, and the lookup's refusal wins over a search
  refused at the same time; a 400 for an issue's own worklogs is no
  refusal of the person or the days), Tempo's 403
  for another person's time and its 400, and more than 500 worklogs in
  the range, more than one read stores. Such a refusal is never the
  connection's problem, since the tracker answered (`failedRead`); a
  refused token, a rate limit, the network and a 5xx still are. A Tempo
  that answers another person's worklogs with an empty list rather than
  403 cannot be told apart from no time logged.
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
  store's transaction), `tracker_setup_unreadable` (with the file's
  path and why), `tracker_setup_newer`, `tracker_setup_refused` (with the
  reason), `tracker_setup_unsure` (a write or removal of the setup file
  that may or may not have happened, with the file and why),
  `tracker_attachment_unknown`,
  `tracker_write_unclear`, `tracker_unsupported` and
  `tracker_request_invalid` (whose sentence quotes what the request got
  wrong). A tracker call that failed is `tracker_failed_<kind>` with
  `trackerapp`'s sentence for its kind (`CallFailure`). The byte routes
  answer the same code and sentence as JSON with a 400. A setup saved
  over a revision that moved is not an error: `SetupSave` answers
  `saved: false` with the sentence and the setup stored now.

A board answer is bounded: the subscribe returns the board's structure,
its sprints and the cards of the sections on screen, and a section's
remaining cards are read by page. A board view asked for no sections
answers the Board screen's (`BoardView.Screen`): the active sprints of a
board that plans sprints, else every column, chosen in the same snapshot
as the structure, with the first page of the first 20 and the rest left
to the client to read by page.

A card's web address costs a Jira board one string: the board view
carries the address its cards' keys complete (`BoardView.IssueURL`, from
`jiraprovider.IssueURLPrefix`), and a Jira card carries none. A Linear
card carries the address the API gave it (`CardView.URL`), since its path
holds the workspace and the title's slug. The client answers a card's
address in one place (`trackerBoardModel.ts#cardURL`).

### Subscriptions

- `SubscribeBoard(projectID, scope, sections, visible, tie)` answers from
  the mirror alone, before any tracker call: a handle, a sequence number,
  the view of the sections or "not loaded", and the poll interval. It is
  refused for a board the project does not list, a connection turned off,
  removed or Tempo, a project whose setup file cannot be read, and past
  64 board handles. The handle starts with
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
- A ticket's answer names its connection's kind and site, both fixed for
  the connection's life, and its view (the answer
  and every `TicketDetail`) the person's account there as the
  connection's row holds it, empty before a verification records one. A
  verification that records another account tells each open ticket of
  the connection as changed (`tellTicketAccount`); a pump made after it
  starts from the new row. Both happen under `opMu`, so no ticket misses
  the change.
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
  cards and histories, and its rows whose parent's name changed
  (`Changes.Parents`; a parent's keep, rank or estimate moves no row).
  These come from one indexed read (`ListMirrorIssueBoards`), made only
  when a board is subscribed; when it fails, or no mirror root is known,
  every board reads again.
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
  that changed any (`ListMirrorIssueBoards`). An open ticket is told at
  each write. The tick's own board is sent one event, with the tick's
  failure, and every other board one event as the tick ends
  (`tickOrigin.others`). Another sender of a board's event during the
  tick sends what the tick left pending for it sooner: a write's
  refresh, the board's own tick, or a setup change that has the board
  read again (`readBoardsAgain`). An event names at most 100 rows. The
  tick's own board past that bound is sent an event that names none and
  says to read the board again (`boardPending.add`). Another board is
  sent what it holds before a write would take its event past the bound
  (`boardPending.overflows`), so of the bound only a single write of
  more than 100 of its rows makes its event say to read again. A catalog
  change, a structure change of the board, or a failed lookup of the
  changed issues' boards says to read again whatever the count. So a
  history read of several batches moves another board's totals once per
  event a patch reads. A write's refresh, which no tick runs, sends each
  board its event as it is published.
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
the mirror with no tracker call, and the days no read replaced
(`Unread`). `RefreshWorklogs` reads at most 31 days
from the time tracker through the worklogs resource first, on the write
path with no write.

The reads a write form needs go to the tracker, interactive, and settle
the connection's problem with their outcome (`form_reads.go`): `Projects`,
`IssueTypes`, `Boards`, `BoardProjects` (the projects a board's filter
shows; a Linear team's own), `CreateFields` (each field with the values
its screen lists and whether the tracker fills it, `tracker.ScreenField`,
as a move's fields are; a Jira screen read within the minute is the
client's kept one, so the create it plans reads it once),
`Moves`, `AssignableUsers`,
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
  is poker's alone and settles nothing of the connection: the ticket
  shows it in its poker section with Retry, and the client holds only an
  answer, so the next ticket opened on the connection asks again.
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
change that can change a total and not with a title or rank. The totals
carry the board's pace, so a history write of an issue in one of the
board's closed sprints moves them too (store `mirrorPaceAdvanceSQL`). The client
sends the status groups version its sections were read with
(`TotalsAt.Groups`); another version is refused as stale, and the
client reads the board again (`readPatch` in
`frontend/src/lib/stores/trackerBoard.svelte.ts`). The client's
rule for applying a patch is the `BoardPatch` comment
(`trackerapp/patch.go`), and its reference implementation is `applyPatch`
in `trackerapp/patch_model_test.go`. A randomized test checks it against a
full read of the board. A one-row change of a realistic board, with three
sections held, costs a 166-byte event and a 1,174-byte patch as JSON, or
1,487 bytes when the totals moved. The patch read is two store calls:
`GetMirrorRoot`, one statement, and `ReadMirrorBoardPatch`, five
statements for three sections with the totals held and nine with the
totals and the pace; with the totals held it reads only the named rows. Tests pin
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
  `current` (the group by its name, `store.MirrorGroupNames`, as a board
  read names it); while a read that missed it is in force
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
  a move away, a draft's copy to another computer), `LinksEnded` tells
  the issues no longer kept as `Changes.Visibility`, as an open ticket's
  drop does.
- **A thread with nothing sent.** A link makes it real
  (`threadRealClause`, which states the rule): `SetThreadTicket` and
  `ClearThreadTicket` report the listing change they make
  (`ThreadTicketSet.Listing`, `ThreadTicketClear.Listing`) and announce
  it, and the empty-draft delete refuses a linked thread. A draft moved
  to another project here takes its link in `MoveThreadDraft`'s
  transaction; a draft's copy to another computer carries it, and the
  transaction that completes the copy (`consumeTransferredDraft`) ends
  the source's with its draft.
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

### Ticket keys, Work, Ask and Start work

- **A key** is `[A-Z][A-Z0-9_]+-[1-9][0-9]*`, matched ASCII
  case-insensitively and bounded by a character that is not a letter or
  digit (`trackerapp.TicketKeys`), in a pull request's title and head
  branch and in a branch's name. It counts when this computer's mirror
  of an enabled connection holds an issue of its project
  (`store.TicketsByKey`, one statement for any number of keys, on the
  partial index `idx_tracker_cache_issues_key`), which also answers the
  issue ID and title it holds. Which mirrors hold a project is read once
  per project, and a key is looked up only in those, each one seek of
  one root: 200 keys over a 20,000-issue mirror cost about 21,700 SQLite
  VM steps with one connection and with a second
  (`TestTicketsByKeyCostsTheSameWithASecondConnection`). When two sites'
  mirrors hold a key, the one holding the issue answers, then a
  connection whose boards the asker's project lists, then the lowest
  connection ID.
- **The pull request rows' tickets** (`PRListItem.Tickets`) are read when
  a repository's list lands, before it is stored and published, and on the
  stored list's prefetch (`prRowTickets`): one statement per landing, and
  one read of the boards of the project the list is under, whose
  connections pick between two sites. A connection added or turned off
  shows in the chips at the next read of each repository.
- **`GetTicketWork`** (home route, `git:operate` and `tracker:read`)
  answers a ticket's open pull requests from the stored lists, scanning
  each stored list's bytes for the key and decoding only the lists that
  hold it; the merged and closed pull requests its linked threads were
  last seen with; and the branches that name the key in the projects
  listing the connection's boards, the pull requests' projects and the
  linked threads' projects. A row names the project it was listed
  under: the stored list's, the linked thread's, or the one whose branch
  the forge's lookup found it by. A branch of project P folds into a
  listed pull request whose head it is: their names match, and the pull
  request is of P's origin's repository or the search's lookup by the
  branch's name in P's checkout found it (a fork's branch heads a pull
  request of its parent). The project a row was listed under takes no
  part, and one ticket's branches can share a name across repositories.
  So in this answer a fork's branch stands beside the parent's pull
  request it heads until `SearchTicketWork`'s answer folds it. A stored
  list's project that was removed since (the list stays until the Pull
  requests page's next read prunes it) keeps its rows, which name no
  project, and has no branches read. Each pull request names the project
  that can show it in the app (`ReviewProjectID`, `reviewProjects`): a
  project of the computer's, archived or not, whose origin is the pull
  request's repository, since a review fetches the pull request's refs
  from origin (`git.Core.EnsurePRObjects`), so a fork's checkout cannot
  show its parent's pull request; none when no project can, and the row
  opens on the forge. The row's own project comes first, then the
  projects Work covers, whose origins the read has; the computer's other
  projects' origins are read, in name order, only for a row those leave
  without one, until every such row's repository has a clone. Each of
  the two methods walks on its own, so a ticket's open walks them up to
  twice. An origin read is cached for five minutes once it has
  succeeded, so a walk that starts after another's read finished reads
  again only the origins that failed; the two requests of one open
  start together, so their walks can overlap and each read an origin
  neither has cached. A read
  is up to two git processes (`rev-parse --git-common-dir`, then
  `remote get-url origin`). With 10 other projects, a ticket with no
  rows and one whose rows the covered projects clone read none of their
  origins over a ticket's open, and a row none of them clones reads
  each project named before its clone once and none after it
  (`TestTicketWorkReadsOtherProjectsOriginsOnlyForARowNoWorkProjectClones`).
  A repository whose branches cannot be read is a problem beside the
  rest. At 2,000 stored rows over 10 repositories, a 500-branch project
  and 30 other projects it costs about 30 ms and 4 MB an open, 32 ms and
  4 MB with the other projects' origins not cached, and 50 ms and 5 MB
  when every list names the key (`TestTicketWorkCostAtRealSize`); a
  200-row landing's tickets cost 0.7 ms. It sends no forge or tracker
  request.
- **`SearchTicketWork`** (home route, the same scopes, budget 2m15s from
  entry: every forge request runs within it, and no project's local read
  starts after it, so a row whose project would be found among the other
  projects after the budget opens on the forge) is
  `GetTicketWork`'s answer with the forge searched by the key
  ([Ticket Work search](forge-transport.md#ticket-work-search)): once per
  distinct repository the projects above clone, then a lookup by name, in
  every state, of each branch of the ticket, the forge's or this
  computer's. A found
  pull request or branch counts only when its title or name names the key
  by the same rule (`namesTicketKey`). A pull request is listed once
  (`ticketWorkRead.addFound`): the forge's row, the newest read of it,
  replaces a stored row or a linked thread's record and keeps only the
  stored list's CI and project (a stored list is as old as its last
  read, which runs only while the Pull requests page is open or at
  boot). A repository's failure is a `TicketForgeProblem` carrying the
  fields a Pull requests page project carries for a failed read
  (`newRepoForgeFailure`), beside the rows its search and lookups read
  before they failed (a GitHub answer keeps the searches no error of it
  names), and a search that stopped at its page bound is
  named in `SearchFull`. The client calls both methods at once and shows
  the stored answer until the search's lands (`stores/ticketWork.svelte.ts`);
  a stored answer or failure after it is not shown; nothing is stored.
  It repeats `GetTicketWork`'s local read, the 18 ms above. Beside the
  forge's search it reads the pull requests the tracker's forge
  integrations link to the ticket (`trackerapp.Service.IssuePullRequests`,
  the provider's `PullRequests`: on Jira the development data's summary,
  then one detail per integration whose count is above zero, at once; on
  Linear nothing, with no request), waits for it at most
  `ticketDevelopmentWait` (10 s, an interactive call's gate wait; the
  transport may still retry within it), and lists them after the forge's rows
  (`ticketWorkRead.addDevelopment`): a pull request listed already stays
  as listed, except one only a linked thread's record gave, which takes
  the tracker's title, author and state; the tracker's link is the
  evidence, so the key rule does not apply; a link whose URL is no pull
  request `gitops.ParsePRURL` can address is counted in
  `DevelopmentUnlisted`; a status not measured is shown as the tracker's
  word. The service returns each failed read as a
  `trackerapp.DevelopmentFailure` beside the rows the others answered;
  `DevelopmentProblem` says each with its endpoint and integration and,
  for a refused credential or a rate limit that endpoint answered, the
  HTTP status; when the wait passed, it says that, beside each read that
  failed otherwise. A site that keeps no data says
  nothing and its cause is logged. No failure settles the connection's
  problem, since the endpoints are undocumented
  ([measured](../references/tracker-api-measurements.md#development-data)).
- **`StartTicketThread`** (project route, `threads:operate`, plus
  `tracker:read`, and `git:operate` for Start work) makes the thread of
  an Ask or a Start work in one rollback: the thread titled with the key
  and title, for Start work in a worktree on exactly the branch named
  (the worktree that has it checked out, else a new one; a name only a
  tag has makes a branch of that name and checks it out, never the tag;
  never a name git's tidying changed; the project's own checkout and an
  empty name are refused), the link with its origin, the
  ticket file `KEY.md` attached to the unsent draft, and for Start work
  the draft's text. A failure removes what it made, ends the link and
  tells the tracker service. A Start work for a ticket, project and
  branch that has a Start work thread not archived answers that thread
  and makes nothing, and one ticket's Start works in a project run one
  at a time, so a call repeated after a lost reply, or two at once, make
  one thread. The
  ticket file (`trackerapp.TicketFileText`, from the stored body, else
  one ticket read) says first that it is the tracker's copy and data,
  holds the ticket's facts, long texts and newest 10 comments, and is cut
  at 200 KB at a line saying where the rest is: about 125 tokens for a
  typical ticket, about 51,000 at the bound.
- **The move and the assignment** of Start work are the ticket screen's
  writes (`stores/trackerTicketWrites.svelte.ts`); the run's order (the
  move, then the assignment decided from the ticket as the move left it),
  "already", which rests on a read that succeeded after the step before
  it, the assignment left to Try again after a move whose outcome is
  unknown, and Try again are `stores/ticketWork.svelte.ts`'s. A call that
  timed out counts as one whose answer was lost, for the thread and for
  each write: it may have been made.
- **A setup section a newer version saved** is never replaced by a save:
  the encoder refuses to write any section named in `Newer`
  (`ErrSetupNewer`), whatever its name, so Save as defaults over a newer
  `start_work` saves nothing.

## Dashboards

A dashboard counts one board from the computer's copy of it. Each call
(`TrackerDashboards`, `EvaluateTrackerDashboard`,
`TrackerDashboardDrill`) names the board's subscription, so a dashboard
reads the board its pane shows, with the project's status groups and
the connection's verified account (`trackerapp/dashboards.go`). Nothing
is retained between calls: each one opens, fills, seals, counts and
closes a database of its own.

| Rule or state | Owner |
|---|---|
| Which rows a board's export holds, the regrouping, the pace's done-at-completion rule | `store` (`tracker_cache_export.go`; the pace's fragment is shared with `tracker_cache_pace.go`) |
| The sandbox, the metric contract and its validator, the reductions, ranges, narrowing, drills and the built-in dashboard | `trackerdash` (its package comment is the sandbox's contract) |
| The board, its status groups and the viewer of an evaluation | `trackerapp` |
| The dashboards a project keeps (the setup file's `dashboards` section) and the widgets agents proposed | `trackerapp` (`dashboard_setup.go`, `dashboards.go`) |
| Bound methods, scopes and the older-computer surface | `app` (`app_tracker_dashboards.go`) |
| The answer on screen, when it is counted again, the dashboard picked, the range, Team or Mine and the drill | `frontend/src/lib/stores/trackerDashboard.svelte.ts` |

### Own dashboards and proposals

A board lists its built-in dashboard first, then the dashboards its
project keeps, in the order they were made. A kept dashboard is a title
and its widgets; each widget carries its own metric (no metric is shared
between widgets), checked when it is counted, so a widget whose metric no
longer validates shows its failure and the rest of the file stays
readable. They live in the project's setup file, section `dashboards`, by
tracker site and board ([Project setup](#project-setup)). The section is
not part of `Setup`: a save of the setup screen keeps it as held, so a
paired computer on an older release cannot delete it, and only its own
edits write it, under the setup's lock, reading the file again first. A
Keep that would take the file past its 256 KiB bound is refused, saying
to remove a widget.

A proposal is a widget an agent showed on a board (`dashboard_widget`
with `show`, [Agent tools](#dashboards-by-agents)). It is held in memory
by project and board, at most 5, and is gone when the app stops. The
list of a board (`TrackerDashboards`) answers its dashboards, its
proposals with their threads, and why Ask your agent cannot start a
thread there, counting nothing and taking no counting slot, so the
screen lays out every widget and control (Keep, Dismiss, Edit, Remove)
from it whatever a count does; values come from an answer of the
dashboard shown, by widget ID. Every evaluation of any of the board's
dashboards counts the proposals after it, and the screen shows them
above the grid with Keep and Dismiss.
Keep moves a proposal onto a kept dashboard or a new one, Dismiss drops
it; nothing joins a dashboard without Keep, and the built-in dashboard
never takes a widget. Edit mode on a kept dashboard removes widgets, and
the dashboard goes with its last one. Keep, Dismiss and Remove
(`KeepTrackerWidget`, `DismissTrackerWidget`, `RemoveTrackerWidget`)
need `tracker:operate`, run on the board's computer and are not
caller-bound. A Keep finds the proposal, writes the setup file and drops
the proposal under the proposals' lock, and every reader of both (the
list, a count, a drill) reads them under that lock too, so it sees the
widget proposed or kept and never both or neither, and a failed write
leaves it proposed.
Each change, from any client, emits the payload-less
`tracker:dashboards_updated` once, on which an open screen of that
computer lists the board's dashboards and counts again; a hidden screen
lists them when it shows.

### The export

`store.ExportMirrorBoard` fills a private in-memory database with the
relations of one board a count reads (`MirrorExportSchema`,
`MirrorExportTables`): the board, its tickets (each one's start and
finish from its stored changes, whether its history shows its start,
whether it is open and whether it counts, by the project's status
groups), the statuses its columns, tickets and copied changes name,
people (the assignees, and the people the changes name when those are
copied), epics, sprints and their members, the tickets' changes and what
each sprint finished. A count names the relations it reads, and each
fill names the relations it reads from the copy, so a fill's needs are
filled first; `board` and `sprints` are always filled, since the range
reads them, and an unknown relation is an error. It attaches the app
file read-only, reads by the board's keys
(`TestMirrorExportSearchesByTheBoard` holds each statement's query plan
to that, and `TestMirrorExportReadsEachRowOnce` the steps it takes per
row it copies), and detaches before it returns. Only the store knows the
app file's path and its pragmas. `MirrorExport.FirstReadDone` says
whether the board's first sync has reconciled
(`tracker_cache_sync.reconciled_at`): until it has, the answer and every
drill carry `reading`, and the range before is not compared. A board
unlisted and listed again starts over.

The relations a count reads are its metrics': each metric's prepare
records the relations it reads (`How.Reads`), once per dashboard kind
on an empty sealed session, and a count asks for those of the widgets it
counts and the tickets, statuses, people and epics its answer names. No
built-in metric reads `changes`, so none copies them.

### Metrics

A metric is one SELECT over the export whose rows are contributions:
a `ticket` and any of `value`, `bucket`, `series`, `person`, `epic`, `at`
and `note` (`trackerdash/metric.go`). Each row is checked as it comes
in: its ticket is in the copy, its series is one the metric names, its
value is finite and within ±1e12, and its `at` is an instant before the
year 2286. The app does the rest: it places rows in the range's periods
by `at` (or by the sprint a `bucket` names), reduces them (count, sum,
median, average, or the average of the periods' sums), narrows them to
a person or an epic, and lists them in a drill. A row whose value is
NULL is not counted: a number says how many tickets it could not count
(`Num.NC`, which leaves out a ticket another row of its series counts),
and a number that counted nothing is not known (`V` absent) rather than
zero. A row of a range metric with no `at` could have happened from its
ticket's creation to its last update: it belongs to each range that
span meets, the range before included, is counted in none and in no
period, and the range lists it in its numbers, their drills and a
chart's not counted. A row not counted is listed as "Not counted: " and its note, so
a note says only why. A metric with `at` or a sprint bucket counts the
range and is compared with the range before; any other counts now and
has no change mark. The host binds `:now` and `:since` (where the
range's periods, the previous range's included, begin), so a metric of
the range reads only the rows it can place; a done ticket whose finish
is not known is read when it changed since `:since`.

A drill names its widget and one of three targets
(`trackerdash.DrillRequest`), and any other is refused: the whole widget
(what a number, a list's More and a chart's not counted open); a bar,
point or table cell, by its bucket's key (empty for Unassigned and No
epic) and its series (empty for the whole bucket); or the rest, the
buckets a bars or table widget of people, epics or text left out past
its 12 bars or 50 rows (what its More opens), ranked by the same
function as the widget. It evaluates its metric again, so
a number and its rows always come from one evaluation, and lists one
row per contribution, with the period it counts in, its value (none for
a count) and the drill's count of rows and of tickets. A later page that
finds a different count starts again at the first. A list and a drill
page read their tickets' titles, statuses and points by ID, eight to a
statement.

### Bounds

- The gate: after the fill, the connection must list only `main` and
  `temp`, its attached-database limit is set to 0 and read back, and
  `query_only`, the limits and the authorizer go on. A failure closes the
  connection and fails the evaluation.
- The authorizer allows a SELECT, reads of the export's relations and
  the allowed functions; it denies the rest and the widget names what it
  tried.
- A metric is one statement of at most 16 KiB, binding at most 8
  parameters, with LIKE and GLOB patterns of at most 256 bytes. It runs
  inside a wrapper that produces the whole result in SQLite's first
  step, under a 1 s deadline counted from before its prepare, at most
  20,000 rows and 4 MiB of encoded rows. An evaluation has 4 s. A
  count of a dashboard is two, one after the other: its own widgets, on
  a copy of what they read, with their titles, then the board's
  proposals, on a copy of their own made after the first is closed, so
  what the proposals read or take never reaches the dashboard's numbers.
  A failure of the proposals' evaluation (its copy, its deadline, a busy
  computer) is each proposal's sentence, and a proposal its deadline
  stops, or leaves no time for, fails in its own place, saying so; a
  proposal's rows are let go once its value is drawn. Two evaluations
  run at once on a computer; a third waits up to 2 s and is refused as
  busy. A count with proposals takes at most 12 s (two waits and two
  evaluations) against its method's 45 s.
- The app's functions (`days`, `working_days`, `day` and the wrapper's
  `bounded_rows`) are registered once per time zone a count names, on a
  driver of that zone the process keeps (`trackerdash/funcs.go`): one
  per zone name the computer's time zone database loads, as the request
  spells it, and at most 32 per process; a count that names a 33rd is
  refused until the app restarts. The
  wrapper finds its session's rows by a random key it writes into its
  own text; a key that is not an open session's fails the statement, and
  a session removes its key when it closes or fails to open.
- A widget's failure is a sentence in the widget's place; the others
  show their numbers.

Costs at 2,000 and 20,000 issues, and a hostile query's temporary files,
are in [Tracker API measurements](../references/tracker-api-measurements.md#the-built-in-dashboard).

## Agent tools

Status: built: `ticket_find`, `ticket_read`, `ticket_create`,
`ticket_update` and `dashboard_widget` on the `ao-tracker-tools` server,
with the read-only refusal, the approval and the timeline row ([Writes by
agents](#writes-by-agents)); `time_read` and `time_draft`, with logged
time and the draft, and the post of drafted time once the person
approves it ([Time](#time)).

**Who has them.** A session gets the server when its thread belongs to a
project, its provider is Claude or Codex, and the computer has a Jira or
Linear connection turned on when the session starts
(`trackerapp.TicketSources`, which reads the connection rows alone). agy
and claude-tui threads do not get the tracker tools, and a session with
none pays nothing for them: no server entry, tool list, guide or app-guide
sentence. A connection turned on later reaches a thread at its next
session start; one turned off later refuses each call with trackerapp's
sentence. Session start resolves the thread's workflow scope once
(`sessionScopeOf`) for the workflow and tracker servers, and the tracker
server reads the connection rows before it, so a computer with none on
never resolves it for these tools. The MCP menu lists the server without a
toggle: for a running session, when that session was given it; for a
stopped thread, when its next session would be. The wiring is
`internal/app/app_tracker_tools_mcp.go`; each tracker read runs bounded
as the bound method that makes the same read (`TrackerLookup`,
`TrackerSearch`, `TrackerComments`), and a detail read, which no bound
method makes, has the default method budget. The tool list carries no
always-load marker (`threadmcp.Server.OmitAlwaysLoad`): no agy session is
given it.

**Keys.** With one connection on, a key of its site is the tracker's
(`WEB-12`), and a key of another site, such as a Time setting's, names
its site. With several on, every key an answer gives names its site, `site/KEY`
(`example.atlassian.net/WEB-12`, `example/ENG-4`), so a key a search
gives reads that ticket whatever the other connections hold. A read with
no key reads the ticket the thread is linked to
(`trackerapp.LinkedTicket`); a key the link names, with or without its
site, reads the link's issue with no request. `site/KEY` is looked up on
each connection of that site in order, one `Lookup` request each, and read
from the first that has it, so a key one account of a site found reads
though another account on the site comes first; a site no connection on
reaches is refused naming the sites on. A key without a site is looked up
the same way on every site on: a site whose connection finds it is read,
and when several sites do the call is refused (`tracker_ambiguous`)
naming each `site/KEY`. A key none finds is refused naming every site
asked, each once.

**Search.** `ticket_find` takes `text`, `mine` (assigned to the
connection's user), `status` (a status's name, case ignored) or `group`
(the tracker's group of statuses: `todo`, `in_progress` and `done` are
Jira's status categories and Linear's state types, `backlog` and
`canceled` Linear's alone; `not_done` is every group but done and
canceled), and `sprint` (`active` only), at least one
of them, all of which must hold; `next` continues the search it came
from and is refused for other arguments (its cursor digests every
argument). The filters reach the tracker as its own query, so a search
is still one request per source:

- Jira: `assignee = currentUser()`, `status = "<name>"`,
  `statusCategory = "<category>"` or `statusCategory != "Done"`, and
  `sprint in openSprints()`, which holds started sprints only
  ([measured](../references/tracker-api-measurements.md#jira-open-sprints)).
  A backlog or canceled group is refused before anything is sent, since
  Jira has no such group; beside another source the refusal is named in
  `unsearched`. Jira answers 400 to a status name it does not have, and
  the call is refused naming it (`tracker_failed_tracker`, "Jira refused
  the search (HTTP 400); check that it has a status named ..."); the
  connection keeps no problem from it, since Jira answered. A 400 to a
  later page is its page token's, since the first page's query was
  accepted, and says to search again from the first page.
- Linear: the issue filter's assignee `isMe`, the state's name
  (`eqIgnoreCase`) or types, and the cycle's `isActive`, passed to
  `searchIssues` beside the text
  ([measured](../references/tracker-api-measurements.md#linear-search-filter))
  or to the listing with no text. A state name the workspace lacks finds
  nothing.

The tool's definition grew from 281 to 525 bytes, and the tool list and
guide from 1,802 to 2,046 bytes.

**Answers.** Every answer is one JSON text block
(`threadmcp.WriteToolJSON`) of at most 24 KiB (`answerBudget`), with no
empty member and no internal ID. Codex keeps at most 64 KiB of a tool's
serialized result, and serializing the answer into it at most doubles it.
A search fills one page of 20 from the sources in order, one search
request each at most, and names a source that refused in `unsearched`;
`next` is present only when more exists. Each row's title, status and
assignee is cut at 300 bytes and each refusal at 1 KiB; within those the
texts share what the keys, the sites and `next` leave of the budget, as a
read's parts do, so every row and `next` stay whole.

A read carries the card whole: key, title, status, type, assignee,
priority, the estimate an update of `estimate` is judged on (the first
of the ticket's boards with estimates, `estimateTarget`; `3 points`,
`1.25 points`, `45m`, `1h 15m`, in a form `set` takes back as the same
value; from the ticket's own read when it carried the board's field),
and when it was
created, last updated and resolved. An issue on no board the app reads
has no estimate in a read, because the app knows no board's field to read
it by. The
sprint is not in a read: the detail names sprints by ID only. The other
parts share what the card leaves of the budget by weight (description 3,
latest comments 4, fields 2, history 2 when asked, links, attachments and
labels 1 each); what a part does not need goes to the others, the
description first. So a long part never pushes another out of the first
answer: a ticket with a 200 KiB description and ten 20 KiB comments
answers the card, about 8 KiB of the description and a piece of each of
the ten latest comments. Within its share, the 10 latest comments share
equally; each field's value is cut at 2 KiB; links, attachments, labels
and changes are listed whole while they fit. A text cut short ends with
`…`, and its rest starts with one. A ticket title is cut at 1 KiB, and
every other title or name (a linked ticket's title, a status, a type, a
priority, a person, a link's relation, an attachment, a label, a changed
field) at 300 bytes, with no rest. Each value of a change is cut at 1 KiB with no rest: a
description change records the whole old and new text, and the
description itself is read whole.

Everything a read leaves out is in `more`: one member per part, named by
a sentence that says what and how much (`the rest of the description
(193 KB)`, `the rest of the comment by User 2 at 2025-11-24T15:59Z
(20 KB)`, `20 older comments`, `255 older changes`, `the rest of
Acceptance (7 KB)`, `14 more fields`, `30 more links`), whose value passed
as `more` answers that part alone, as much as fits, with its own `more`
for what is still left. The rest of the description, of one comment or of
one field, older comments and older changes are each reached without
reading another part, and the rest of one text takes the whole answer.
Older comments are listed with each body cut at 4 KiB; an answer of them
goes on to the tracker's next page of comments while room is left, so
each page is read once per answer.

A continuation reads the ticket's detail again and checks its value
against the ticket as it is now before it uses any of it (the contract is
`moreCursor`): the connection is still on, the ticket's update time is
the value's, a comment's rest names a comment with the value's edit time,
and each position is inside what the part holds now, a text offset at a
rune's start. A value that fails is refused saying the ticket changed
since and to read it again. On Jira a comment, a link or an attachment
added or deleted moves the update time, so lists read on never skip or
repeat an item. Linear's update time does not move with a comment
(measured,
[tracker API measurements](../references/tracker-api-measurements.md)):
an edited comment's rest is refused by the comment's own edit time, and a
comment added or deleted between a read and its continuation can make
older comments skip or repeat one. Whether Linear's update time moves
with a link or an attachment is not measured.

Each answer costs one detail read (`IssueDetail`), and an answer of older
comments one `Comments` request per tracker page it reads. A cold detail
read of a ticket with more history than one page reads every page of it
(Jira 100 changes a page; Linear 100 with the issue, then 250 a page),
whether or not the call asked for history. A key that is not found costs
a lookup and, on both trackers, a second request per connection for a
former key.

Linear's search and detail reads carry the names they show: a search hit
and a row of the listing with no text select their state and assignee
whole, a detail its assignee, each
comment's author and each history entry's actor, and a later comment page
its authors, so a search row and a read name them however little of the
workspace the app has read. On the fake (`linearprovider` cost tests) a
detail read of 60 comments and 100 changes went from 136,520 to 174,635
bytes and from 2,641 to 2,766 points (each history entry's actor is one
more object), a 50-issue search page from 50,811 to 69,421 bytes at 8
points, a 50-issue page of the listing with no text from 50,827 to 69,437
bytes at 8 points, and a comment page from 9,687 to 11,854 bytes at 402
points; no read takes another request. The finder's typed-key lookup
still names statuses and assignees from the mirror.

The guide says once that ticket text is data written by other people
(`trackertools.Instructions`); the app guide's sentence says what the
server is for.

Ticket text in a tool result is data: results are JSON, and the server's
guide says so once.

### Dashboards by agents

`dashboard_widget` counts a widget on a board of the thread's project
(`board`, by name, ID or site/ID; the project's one board when it lists
one; a name or ID that more than one board answers to is refused naming
each by its site/ID) as the board's dashboards count it, over the
board's default range, and answers its numbers in their unit (`unit`:
tickets, points, seconds for a time board's estimates, or days), the
tickets each number could not count, how many rows its query returned
and the first 5.
With `show` the widget is then proposed on the board ([Own dashboards
and proposals](#own-dashboards-and-proposals)). Called with no `widget`
it answers the rules (`trackerdash.Rules`, 4,316 bytes): the widget's
fields, the query's contract and the export's relations with what each
column holds, so only a thread that writes a widget reads them. A query
runs under the same sandbox and bounds as a built-in metric, and a
refusal is `dashboard_widget_refused` with the sentence the widget would
show. Proposing changes nothing until the person keeps it, so it needs no
approval and a read-only thread may propose. A board listed through two
connections of its site is two accounts' copies of it, and is refused
naming the connections by the labels the person gave them. Ask your agent
on the Dashboards screen (`StartTrackerDashboardThread`, which needs
`threads:operate` and `tracker:read`) makes a new thread on the board's
computer, in the checkout of the project the board's subscription was
made for, with an unsent draft naming the board by its name and
site/ID, on a provider whose agents get the tracker tools
(`SupportsTrackerMCP`) and that the computer offers: the one a new thread
starts on when it has them, else Claude, else Codex
(`app.trackerToolsProvider`). A draft that cannot be saved takes the
thread back. With neither offered, the list says why, and Ask is
disabled with that sentence; the screen lists again when the computer's
settings change.

The tool is listed in every session that has the tracker tools. Listing
it only in a thread Ask your agent started would need a mark on the
thread kept across restarts, and would leave a person asking in any other
thread without it. Its definition adds 304 bytes: with the time tools
([Time](#time)), the tool list and guide of a session with a Jira
connection are 3,307 bytes, and of one with Linear alone 2,350.

### Writes by agents

`ticket_update` changes the tickets `keys` names (at most 20, or the
linked ticket), then comments on each; `ticket_create` creates one ticket
and answers its key. Both go through `trackerapp`'s bound write methods
as the agent's writes (`ByAgent`), so the screens learn of them as of a
person's ([Writes](#writes)), with no saving state, note or toast, and a
person's draft of the same field stays a draft. The code is
`internal/trackertools/update.go` (the call), `keys.go`, `plan.go` (what
each ticket's change is), `apply.go` (sending it), `create.go`, `set.go`
and `marks.go`; the app's side is
`internal/app/app_tracker_tools_host.go`.

**What `set` takes.** A name matches without regard to case: the members
a read writes at its top (status, sprint, estimate, title, assignee,
priority, labels, parent, description), or a field by the name the read
shows under `fields` (a name two fields share with its ID, as the read
writes it). The estimate also answers to the field a board of the ticket
estimates by (Jira's Story points). An estimate has one board
(`estimateTarget`): for a field, the first of the ticket's boards the
mirror holds, by board ID, that estimates by it, and for `estimate` the
first that has estimates; the plan judges it on that board's row, the
approval words it in that board's unit, and the read after the approval
and the send name that board; two entries of one call that both name a
ticket's estimate are refused. A name that matches nothing is refused naming the
ticket's names, at most 40; a member the read shows that no write sets
(`created`, `key`, ...) and a field that is not editable are refused. A
value is text, a number, a list, or null to clear; an empty list clears.
A value in the form a read prints it is taken back: an estimate as `5`,
`5 points` or `1h 30m`, a date-time stamp as RFC 3339, a parent as `WEB-4
Title`, and `me` for a person is the connection's account. A status is a
move's target by name, refused naming the moves; a sprint is an open
sprint of the ticket's board by name, refused naming the open sprints, and
null or `backlog` is the backlog. A sprint or an estimate needs a board of
the ticket the mirror holds, else it is refused saying to add or open the
board. A call that sets `estimate` reads each ticket through its row on
the estimate's board alone (`trackerapp.Service.IssueRows`, one rows read
per board, the way a write's refresh reads a row), so the estimate is
judged on what that board holds now; a ticket that board no longer lists
is refused its estimate before the approval, with no other board tried.
A field name is judged on the ticket's detail read.

**Long texts.** The server remembers, per session (the capability's
token), for each ticket the session read (at most 256, oldest out), the
ticket's update time at that read and, per long text, how much of it the
session was given: the first read and each continuation add to it. An edit
of the description or of a rich-text field, or of any text longer than a
read gives at first, is refused when the session never read the ticket,
when the ticket changed since, or when the session was not given the text
whole ("You have read only part of WEB-12's description; read the rest of
it through more before you change it."). A plain text field with several
values is judged per value. An edit the tracker confirmed moves the mark
to the refreshed time with the texts that edit wrote; the ticket's other
texts are unread at that time, so an edit of one says the ticket changed
since it was read. An edit that was refused or never answered moves
nothing. A mark is recorded only while the app still holds the session,
and the marks end with the session's capability.

An edit that replaces a long plain text carries the text it replaces
(`tracker.Edit.Replacing`), and an edit of rich text, a clear included,
carries the Markdown it was written from. Jira compares either with what
it stores before it writes and refuses a text that changed; Linear cannot
compare a plain text and refuses one as unsupported.

**Order and outcomes.** Every ticket is resolved and checked before
anything is sent: names, values, the move and the sprint. A naming or
value error is a tool error and nothing is sent; a status that is not a
move, a sprint that is not open or a ticket on no board is that ticket's
`refused` and nothing is sent for any ticket. Then each ticket's steps
run in order (status, fields in one edit, estimate, sprint, comment); the
first refused or unknown step stops the ticket, and the steps after it
are in `not_sent`. A step the ticket already has (its status, its sprint)
sends no request and is in `already`, not `applied`. Each ticket's answer
is `applied`, `already`, `refused`, `unknown`, `not_sent` and a `note`,
each only when it has something. An unknown outcome says the same call
again makes only what was not. The boards a call judges are read once
each per call.

**Approval.** A call that names more than one ticket waits for the
person, unless the project's setup turned off asking (`agents`
`ask_several` false, Settings > Issues > Agents): then it is sent as one
ticket's call is. The call reads the project's rules
(`Host.WritePolicy`) three times, each for one job: when it starts, to
refuse before any tracker read; after planning, to decide whether it
asks (and to bound what the approval carries); and before the first
write, to send nothing the rules no longer allow. A call of several
tickets that went unasked and finds asking turned on by then is refused
with nothing sent (`tracker_rules_changed`), telling the agent to make
the call again. The app raises a prompt of kind `tracker-write` in the thread
(`provider.EventApprovalRequest` through triage, decisions accept and
decline only), headed by the action (`Change 3 tickets`; the
`Approval`'s `Title`, with `Tool` the prompt's tool name), showing each
ticket's changes and the comment. The prompt
carries each value of `set` whole, and a change line that shows a value
in part names it, so the panel's Show all opens the whole value with
nothing fetched. A call whose `set` and comment together pass 256 KiB is
refused before any prompt, telling the model to make the change in
smaller calls. The person's answer reaches the call by the kind of the
prompt triage holds open (`triage.PromptClaim.Kind`), not by its request
ID, and the waiter is keyed by thread and request ID. An approval counts
only with `ApprovalResponse.Shown`, which the screen's tracker write list
(`TrackerWriteChanges.svelte`) sets once it has drawn every ticket,
change and note of that prompt occurrence. An approval without it comes
from a screen that drew none of them (a paired v0.0.19 one, a build from
before the list, or input the list could not draw): it is refused saying
to answer on a screen with the current version or to update, and the
prompt stays open. A decline settles from any screen. Triage never
routes it to a spawner (`triage.promptRoutable`), so a worker's tracker
write is the person's, which the worker's composer shows on every screen
and its spawner's Agents row reaches like any prompt routed to nobody
([ruling 19](../specs/standing-agents.md)). The call's waiter settles
once, by the first of the answer (`RespondToApproval`), 15 minutes
(`IsolationConfig.TrackerApprovalWait` in a harness boot), and the
call's end; an answer that loses is refused as already handled, and a
wait that ends first withdraws the prompt from every screen. A prompt
triage does not keep, because the session no longer holds the thread,
ends the call at once. Nothing is persisted: an app that stops ends the
call, and nothing was sent. The writes after an approval run on the
server's context to their end.

**Changed while waiting.** What `set` changes was judged on the tickets
as they were before the prompt, so after the person approves, the call's
cards are read again (`planCall.recheck`): for a call that sets an
estimate, through each ticket's row on its estimate's board, one rows
read per board (`IssueRows`), which reads the card too; for any other,
one key lookup per connection.
On Jira that is one `issue/bulkfetch`, and one more `GET issue` for each
key the fetch did not answer under that key (a ticket moved to another
key, or gone), four at a time; the field catalog is read again too when
the client's copy is older than 30 minutes. On Linear it is one `issues`
list, one `IssueSummary` for each key the list did not find, and one
read of label names when an issue holds labels the list left unnamed. A
ticket whose update time, status or sprints differ from what
its plan read, or that the read no longer finds, is sent nothing, and so
is one whose estimate on its board moved, or that the board no longer
lists: its
`refused` says it changed on the tracker while the user was deciding,
every step is in `not_sent`, and the call's other tickets go on. The
update time covers every value the plan judged on Jira, where a field
change, a comment, a link, an attachment and a worklog each move it, so a
teammate's comment during the wait refuses the ticket too. On Linear a
title, assignee, priority and status change move it, an estimate change
does on a team with estimation off (one with it on is not measured), and
a comment does not; a cycle change is not measured, which the sprint
comparison covers ([measured](../references/tracker-api-measurements.md)). A read that
fails sends nothing and is the call's error. A comment alone is not read
again after the approval: its ticket is looked up just before its
comment, and one the lookup no longer finds is sent none. A call naming
one ticket does not wait. After the read, in the seconds the earlier tickets' sends take, a
change is caught only where a write compares at send: a text an edit
replaces whole. A move the ticket's workflow no longer offers, or a status
its Linear team no longer has, is sent as the plan's read found it, and
the tracker's refusal, which the provider maps to a stale refusal
(`jira.UnofferedError`, Linear's refusal naming `stateId`), stops it: the
agent is told the ticket changed and to read it again, as a person's
Board move is.

**A Jira comment's audience.** Who a Jira comment reaches is judged on the
project the ticket is in when the comment is sent: a ticket moved into a
service desk project would otherwise get a public comment, which emails
the customer. Nothing the call sends, other than its own comments, goes
out on the connection between the read a comment's audience is judged on
and that comment. A call that comments on Jira tickets looks them up with
one key lookup per connection at the start of its comment phase, after
its other writes, and each comment is judged on the project that lookup
found (`trackerapp`'s write session keeps it,
`tracker.CommentInput.Project`). The session gives a comment that
project only while it sent no write but comments on the connection since
the lookup; otherwise the comment reads the issue's project itself, just
before it is posted. A ticket the lookup no longer finds is sent no
comment. The Jira client keeps each project's
type by ID for 30 minutes, from the projects a bulk fetch's issues name
(`projectTypeKey`, which the bulk fetch carries as measured in
[tracker API measurements](../references/tracker-api-measurements.md#jira-cloud))
or a read of the project; only a comment's audience reads it there. A
bulk fetch whose project has no `projectTypeKey` keeps nothing, and the
comment reads the project (`GET project/{id}`) before it is sent. The
tools name no audience, so a comment on a service desk ticket is
refused. A lookup that fails sends none of its tickets' comments, and
the steps the call sent before them keep their answers; a project whose
type cannot be read, by either read, sends no comment to that ticket. A person's comment
reads the issue's project with the comment, as before.

**Requests.** What each call shape costs on the fake, cold (the mirror
holds the tickets' cards and rows) and warm (the agent read the tickets
first), pinned by `TestTrackerToolRequestCounts`
(`internal/app/app_tracker_tools_costs_test.go`), every request of the
call included, its refresh too. A pin holds the writes in the order they
are sent and counts the reads between two writes, which a call makes
for several tickets at once. A call of three tickets waits for the
person and reads its cards again after the approval when it sets
anything, through the estimate's board's rows when it sets an estimate;
a call of one does not. A Jira comment's lookup is one more read, just before the comments.
A person's estimate costs what it did: a rows read (Jira) or `IssueTeam`
(Linear) checks the board, then the estimate and the refresh, 3 requests
(`TestTrackerPersonsEstimateRequests`).

| Shape | Jira, 1 | Jira, 3 | Linear, 1 | Linear, 3 |
|---|---|---|---|---|
| Comment | 2 | 4 | 1 | 3 |
| Status, cold | 4 | 11 | 3 | 6 |
| Status, warm | 4 | 9 | 3 | 6 |
| Sprint | 5 | 6 | 4 | 5 |
| Backlog | 4 | 5 | 4 | 5 |
| Title | 4 | 9 | 3 | 6 |
| Estimate | 3 | 6 | 4 | 9 |
| Status, title, estimate, sprint and comment, cold | 13 | 30 | 9 | 20 |
| The same, warm | 13 | 28 | 9 | 20 |

A cold status takes its moves from the plan's detail read; a warm one is
planned on the cards, which keep no read, so the move reads its moves.
A Jira estimate takes the rows read its call made, after the approval
the second one, as its check that the board lists the ticket (`tracker
RowsRead`, kept by the write session until something is sent to the
ticket), so it sends with no read of its own: one request per ticket;
after a status move to the same ticket it checks again with a rows read.
A Linear estimate reads the issue's team and its scale (`IssueTeam`)
before each send, as a person's does: the rows read selects no team
scale, which only this check would use. A Jira ticket on two boards
costs 4 for a new estimate (its board's rows, the estimate, and the
refresh of both boards) and 1 for one it holds already
(`TestTrackerAgentEstimateOnTwoBoardsRequests`). A create costs 5 on Jira
(the types, the create screen, the create, the match and the refresh) and
2 on Linear; a second create within the minute the client keeps a create
screen costs 4 on Jira; a create that also sets the status and the sprint costs 12
and 6. A read costs 2 on Jira (the issue and its comments) and 1 on
Linear; the same read again costs 1 on each. A find costs one search on
each connection.

**Read-only threads.** A writing call is refused when the calling
thread's runtime mode is read-only, or its project's setup turned off
Change tickets (`agents` `change_tickets` false, code
`tracker_changes_off`, whose sentence tells the agent to ask the person
to turn it on in Settings > Issues > Agents), before anything is read or
sent, and both are read again before the first write is sent, after the
approval when there is one (`Host.WritePolicy`). A project whose setup
cannot be read, or whose `agents` section a newer version saved, has
every writing call refused (`tracker_setup_unreadable`,
`tracker_setup_newer`): this build cannot tell what the project allows.
A thread with no project writes by the built-in rules. The provider
cannot enforce either, because app tools are admitted in every mode.

**Nothing made twice.** A comment or a create is claimed before it is
sent, in one store transaction under one lock
(`store.ClaimAgentWrite`): within an hour, one caller of a thread
holds the right to send a given digest at a time. The digest is the
comment's Markdown on the issue, or what the create request sends (the
tracker, site, project, type, title, sprint and every field, by field ID in any
order), never the steps made after a create, so a call again after one of
those was refused finds the ticket. A later call whose digest is applied
sends nothing and puts the step in `already` with a note, the create
naming the key its row kept; one whose digest an earlier call is still
sending is refused saying to call again once that call has answered. One
whose earlier send was interrupted (its answer never came, or the app
stopped) reads the tracker first: the issue's comments by the writing
account from a minute before the send to two minutes after, compared on
letters and digits outside mentions, or a search for the title in the
project in that window. What it finds settles the earlier row and is the
answer; nothing found sends under the same claim; a read that fails sends
nothing and is refused saying it could not be checked and to call again
to check. A row sent before the connection's account was verified cannot
tell the agent's write from a person's, so nothing is read or sent and
the refusal says to check the connection. A claim's row ID is never
reused (`AUTOINCREMENT`), so a caller holding a refused claim's ID never
settles another write's row.

**Creates.** A create goes through one board, chosen over every
connection's listings of the project: a board read here that the
thread's project lists, else any board read here. None is refused saying
to add one, and only unread ones are refused saying to open one. Boards
of the project on two sites are refused naming both. The create screen
is read once for the call: the tool's read of the create fields asks
Jira, and the create takes the screen the Jira client kept from it for a
minute, which a create refused for a field on it drops. Jira takes the type by name (Task, else the first type that
is not a subtask, when none is given); Linear has none and refuses one.
Every field of `set` is resolved before the create request, so a value
refused there creates nothing; status, sprint and estimate are set after
the create, and one refused there says the ticket was created ("WEB-42
was created, but status was not set: ..."). A create that a later call
finds made is answered with its key, and the steps after the create are
set on it where it does not hold them already.

**Timeline.** A call that applied anything, and only then, adds one notification row of
kind `tracker_write` to the thread at its current turn (`Created WEB-42`,
`Changed WEB-12`, `Changed 3 tickets`), with each ticket's connection,
issue, key, title and applied steps; each key opens the ticket.

### Time

**Who has them.** A session is given `time_read` and `time_draft` when a
Jira connection is on as it starts (`trackerMCPAccess.Time`); a session
with only Linear gets the ticket tools and `dashboard_widget`, and a call
of a time tool from it is refused as no such tool. Their two definitions are 268 and
687 bytes, 957 of the tool list with the commas that join them
(`TestTheTimeToolsAreGivenWithAJiraConnection`); the whole list is 3,135
bytes and the guide 172 (`TestTrackerMCPToolListAndGuideStayWithinTheirBound`). The time
is the connection's verified account's: a connection with no verified
account, or whose key file holds another token than the one verified, is
refused saying to check it, since a post logs time as the token's owner.

**`time_read`** takes `from` and `to` (YYYY-MM-DD), this week Monday to
Sunday when neither is given and 7 days from the one given, at most 7.
It reads each Jira connection on (`trackerapp.Service.ReadTime`): the
days from the tracker first (`RefreshWorklogs`), then the mirror. A
week's read is one request on Jira's own worklogs, and two on Tempo: its
worklogs and the Jira issues they name
(`TestAWeeksDraftCostsBoundedRequests`). Each day answers its worklogs (`logged`), the draft's entries not yet logged
(`drafted`, by `agent` or `you`, in state drafted, sent or unknown) and
its open questions. A connection whose read failed is named in `unread`
with its sentence; the call fails only when every connection did. The
answer names the tickets the thread's project's Time settings put
meeting and time-off hours on (`meetings_ticket`, `time_off_ticket`,
each absent when not set, and written as [Keys](#agent-tools) are),
read from the setup with no request (`Host.TimeSettings`). Time
settings that cannot be read, or that a newer version saved, fail
`time_read` and a draft of entries with their sentence; a post reads no
Time setting.

**`time_draft`** takes `entries` to draft or `post` to log the draft of
days (below), and refuses a call with neither or both. `entries` are each a `day`, `hours` (whole minutes),
a `reason`, which becomes the worklog's description, and a `key`, or no
key for a question, whose reason says what the time was and whose
`offer` names up to 5 keys. A call holds 1 to 250 entries over at most
31 days, on one Jira connection; a call with more entries or offers is
refused before any key is looked up. Each key resolves as the ticket tools' do, a Linear key
is refused, and so are keys of two sites. A day's entries not yet logged
hold at most 24 hours, whichever threads drafted them and whoever changed
them (`store.TimeDayOverError`, checked in the write's transaction); a
ticket is drafted once a day, and no day is in the future. A read-only thread is
refused (`Host.WritePolicy`, read at admission and again just before the
store write, so a thread made read-only during the key lookup drafts
nothing). A key of the project's meetings or time-off ticket that no site
finds is refused naming the setting, so the agent asks and the person
picks another (`goneSetting`, from the not-found refusal's cause); a key
the mirror holds resolves with no request, so a ticket gone from Jira
drafts until this computer reads it again. With
Never log to an epic on, an entry whose own ticket is an epic (type level
1 or more) is refused; the type is the lookup's card's or the mirror's,
which the draft reads anyway, and an entry whose type neither has, such
as the thread's linked ticket the mirror does not hold, is drafted with
no request asked for it. An offer of an epic is the person's to pick. The draft replaces the drafted entries that
the calling thread drafted on each day the call names; another thread's
entries, the person's, and entries sent, posted or of unknown outcome
stay, and a new entry for a ticket that has one not yet logged that day
is skipped. The answer is by day: the ID and key of each entry made, the
IDs of the questions, the hours of the day's other entries not yet
logged, the keys skipped, the hours already logged as this computer last
read them, and that nothing is logged until the user approves it; with
more than one connection on, the site is named once and the keys are
bare;
`time_read` shows the entries whole.

**`time_draft` with `post`** names up to 7 days (YYYY-MM-DD) and drafts
nothing (`trackertools/time_post.go`). It reads `Host.WritePolicy` at
admission, then the entries not yet logged on exactly those days of each
verified Jira connection, with no tracker request
(`trackerapp.Service.OpenTimeEntries`, the day index, at most 251 rows):
every entry, whichever thread or person drafted it, as a day's approval
names them. A connection never verified holds no draft and is not read;
one whose read is refused is named in `unread` and on the prompt, and the
others go on. When no connection that was read holds an entry, the
refusal names each connection that could not be read, by account where
two share a site, and why, with their code when they share one
(`tracker_call_failed` when not). Days
with no entry not yet logged are refused, as are more than 250 entries,
256 KiB of approval, or more entries than one answer names. When no entry
is one the post would send, the answer gives each entry's reason and
nothing is asked. Otherwise it always asks the person, through the
tracker write's prompt (above), titled `Log time for Tue Nov 24`: each
entry under its ticket's key as its day, time and whole reason, an entry
that will not be sent saying why in the words the answer uses
(`trackerapp.NotSentWhy`: it may already be logged, it is being posted,
its ticket is not picked), and each question as a note
(`trackerapp.EntryLine`). A key carries its site when more than one
connection is on, as every answer writes it. When two Jira connections
share the site, each ticket, answer line, `unread` and `warnings` entry
also carries `account`: the name the person gave the connection in the
Issues settings, or its account email when they gave none, and both
(`Work · user@example.com`) for every connection of the site when two
would share one name (`trackertools.postAccount`). Once approved it reads the policy again, then
posts on each connection exactly the entries shown, at the revisions
shown, through `PostTrackerTimeEntries` on the server's context, so a
post is a day's approval with its rules: an entry the person edited or
removed while the prompt was open is not posted, and one drafted
meanwhile is not sent. The answer names every entry as the prompt showed
it (day, key, hours): `logged`, and `not_logged` and `may_be_logged`
grouped by why, from each entry's outcome (`PostTimeView.Outcomes`,
which only the agent's post reads: posted, not_posted or uncertain; an
entry not sent takes `trackerapp.NotSentOutcome`); the reasons and
warnings share what the entries leave of the answer's budget, which the
admission reserves for every entry under a reason of its own and a
warning per connection. A connection whose post was refused before it
sent lists its entries as not sent with the refusal; when no
connection's post sent anything, that answer is the call's, and nothing
goes on the timeline. `warnings` names, per connection, the days whose
read-back failed (`trackerapp.ReadBackSentence`) and a post that sent
and could not store its outcomes (`trackerapp.ErrPostUnrecorded`), with
the store's error, which the person may need to fix; those entries read
as may already be logged after the app restarts. An error after the
outcomes were stored (the read of the entries after them) changes
nothing the answer says and is only logged.

**The draft.** There is no draft object: the entries of a connection's
author by day (`tracker_time_entries`, [schema](schema.md)), each naming
the thread and call that drafted it. Every change is conditional on the
entry's revision as the caller read it (`trackerapp/time_draft.go`) and
to the connection's verified account: the person's edit makes the entry
theirs, and a remove deletes entries drafted, of unknown outcome or left
out, sending nothing. `sent` means this process is sending the entry:
nothing changes or removes it until its settle. The once-per-process
recovery (`trackerapp.recoverEarlier`) makes an earlier process's `sent`
entries `unknown` before this process reads or writes any entry, so every
reader sees an interrupted send as one that may have happened. A question answered with a
ticket that has a drafted entry that day is added to that entry in one
transaction (`store.EditTimeEntry`): its time is added, the entry's
reason ends with ", plus " and the question's, and the question is
deleted. An answer onto an entry being posted is refused, and onto one of
unknown outcome with the sentence a post gives it, as is moving an entry
onto a ticket that has one that day.

**Posting.** `PostTimeEntries` (`trackerapp/time_post.go`) is the one
path that sends a draft's time, and only the person's approval reaches it
(`PostTrackerTimeEntries`). A request names each entry once, at the
revision read; logging alone names exactly one. It posts the drafted entries for
the connection's verified account, in one batch through the time source
(`TimeTracker.AddWorklogs`, which names the worklog each made). Each entry
is marked `sent`, with the time, in one transaction after the connection
is held and before any request; an entry another window is posting, or
that changed since it was read, is not sent and is said so. An entry is
posted only when the time source answered that it applied it, with the
worklog's ID. Approving a day or the whole draft:
- skips an entry whose ticket and day have a worklog created since the
  entry was drafted (the comparison allows a minute of clock difference;
  a worklog changed since but created before is no reason), and says so;
  logging that entry alone posts it;
- posts a day's entries and leaves its open questions open.

The check reads the days in the send's own hold of the connection, on the
provider and time source that then send, and stores them in the mirror as
a refresh does; a day that cannot be read sends nothing of it. A worklog
logged by hand between that read and the POST is not seen, nor, on Jira's
own worklogs, one logged so shortly before that the issue search the
check reads does not list it yet.

An entry whose post may have happened (`unknown`, after a lost answer, a
dropped request, or a send an earlier process did not settle) is never
sent again, since no read the app can make proves the first send did not
land: logging it alone or approving its day names it, sends nothing of
it, and says that the time may already be logged, to look at the day's
logged time, and to remove the entry if the time is there or to log the
time by hand and then remove it; the day's other entries post. Every
view of such an entry carries that sentence (`TimeEntryView.refusal`),
so a surface shows it before anyone approves. The answer is built after the post from the entries
as they are then, names every entry, says in one sentence each one not
posted, with the tracker's refusal, gives each named entry's outcome
(`Outcomes`), and names the days whose worklogs
could not be read back after the post, which show the posted time after
their next read. A post that fails after it sent (its settle's store
write, the service's stop) answers the outcomes it knows with the error.
One day's approval of three entries is 3 POSTs and 2
reads on Jira's own worklogs (the day before the post, for time logged
since, and after it); on Tempo it is 3 POSTs, 2 reads of the worklogs and
2 of the Jira issues they name, the timesheet approvals and, once per
connection, the work attributes. Logging an entry alone skips the read
before (`TestAWeeksDraftCostsBoundedRequests`). A week of 25 entries is 27
requests on Tempo (2 reads that check the batch, then a POST each) and 25
on Jira's own worklogs, with the account read before (`jiraprovider` cost
tests).

**Timeline.** A `time_draft` call that changed the draft adds one
notification row of kind `time_draft` (`Drafted time for Tue Nov 24`),
naming the call, the connection and the days, and never rewrites it; a
thread reads the call's entries as they are now through
`ThreadTimeEntries`, one call's at a time, so the read is bounded by the
call (at most 250 entries) however long the thread has drafted. A post
that ran adds one row of kind `time_post`,
written once with the count it ended with (`Logged 2 of 3 entries for
Tue Nov 24`, of the entries the prompt offered to log, and `; 1 may have
been logged` for those whose answer was lost), naming the call, the
connections and the days; a denied or unanswered post, or one refused on
every connection before it sent, adds none.

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
form-read, worklog and planning poker bound methods, the ticket file routes,
`tracker:board_updated` and `tracker:issue_updated`, the app's calls at
start, the harness's fake trackers (`HarnessTrackerSeed`), the project
setup file with its status groups and ticket pins, its three bound methods
and `tracker:setup_updated` ([Project setup](#project-setup)), and the
Issues settings screen (Settings > Issues: Connections, Boards and Status
groups), with
the `bindings.ts` wrappers of the connection methods, the two picker lists,
`ProjectTrackerBoards` and `SetProjectTrackerBoards`, and their stores
(`stores/trackerConnections.svelte.ts`,
`stores/trackerProjectBoards.svelte.ts`), and the Issues pane's Board
screen, with the `bindings.ts` wrappers of `TrackerBoardChoices` and the
board subscription's methods and `tracker:board_updated`'s TypeScript
type, and its ticket screen, in the Issues pane or a pane of its own,
with the wrappers of the ticket
subscription's methods, `TrackerComments`, the issue and comment writes,
`TrackerAssignableUsers` and `TrackerChoices`, and
`tracker:issue_updated`'s TypeScript type, and the ticket's attachments,
links, planning poker and Log time, with the wrappers of the attachment
mints, `SaveTrackerAttachment`, `TrackerLinkTypes`, `LinkTrackerIssues`,
`UnlinkTrackerIssues` and the planning poker methods, the Board
screen's changes to a card (its fields, the carry, Move to… and Move to
sprint…), with the wrappers of `TrackerMoves`, `RankTrackerIssues` and
`MoveTrackerIssuesToSprint`, its Backlog screen (the sections, a
row's changes and carry, New ticket in a section, and the sprint
writes), with the wrappers of `CreateTrackerIssue`, `TrackerIssueTypes`,
`TrackerCreateFields`, `CreateTrackerSprint`,
`EditTrackerSprint`, `StartTrackerSprint` and `CompleteTrackerSprint`,
its Time screen,
with the wrappers of `TrackerWorklogs`, `RefreshTrackerWorklogs`,
`AddTrackerWorklog`, `EditTrackerWorklog`, `DeleteTrackerWorklog`,
`TrackerLookup` and `TrackerSearch` (`components/issues/AGENTS.md`),
and the thread link's screens: the header's ticket, its menus, the
sidebar row's line and search, and Link and Change ticket through the
ticket finder, with the `SetThreadTicket` and `ClearThreadTicket`
wrappers (`stores/threadTicketLinks.svelte.ts`), and the ticket's Work
with Ask and Start work and the pull request rows' ticket chips
([Ticket keys, Work, Ask and Start work](#ticket-keys-work-ask-and-start-work)),
with the wrappers of `GetTicketWork`, `SearchTicketWork` and `StartTicketThread`
(`stores/ticketWork.svelte.ts`), the project's setup: Settings >
Issues > Status groups, the ticket's pins and the Start work defaults,
with the wrappers of `ProjectTrackerSetup`, `SetProjectTrackerSetup`,
`ProjectTrackerStatuses` and `tracker:setup_updated`
(`stores/trackerSetup.svelte.ts`, the one holder of a project's setup
and statuses in a window), and a board's dashboards on the Dashboards
screen ([Dashboards](#dashboards)): the built-in, the ones its project
keeps and the widgets agents propose, with the wrappers of
`TrackerDashboards`, `EvaluateTrackerDashboard`, `TrackerDashboardDrill`,
`KeepTrackerWidget`, `DismissTrackerWidget`, `RemoveTrackerWidget`,
`StartTrackerDashboardThread` and `tracker:dashboards_updated`
(`stores/trackerDashboard.svelte.ts`).
A failure of the linked-issues
loop after boot reaches only the log (`Deps.LoopFailed`): its failures
are outside the tracker (a store error, the cleanup), and a tracker
failure of the loop's sync shows on its connection's row as its problem.
Not built, owed by the app layer:

- The rest of the project's tracker setup, each a section of its file:
  card marks, draft settings and what agents may do, and the Setup pages
  that edit them. A query that
  needs part of the setup takes it as a bound argument, as the status
  groups are; nothing of the file is copied into the mirror.
- Searching the forge by key for a pull request outside the stored list.
