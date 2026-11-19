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
  priority, labels, parent, the sprints it is or was in, created, updated
  and resolved times and when it entered its status. A board row is a card
  with that board's order key and estimate. **An issue's detail** adds the
  description as Markdown, every other field with its metadata, comments,
  links, attachments, history, the moves its workflow allows and worklogs.
  Detail is read when a ticket is opened.
- **A field value is typed**: text, rich text as Markdown, number, date,
  date-time, user, option, a list of one of these, or a value the app can
  show and not edit. An edit names an option or user by ID when it was
  picked from a list and by text when a person or an agent typed it; the
  provider resolves text and reports a value it cannot match.
- **Capabilities** say what a connection supports: issue types, custom
  fields, moves that require fields, sprint create and complete, relative
  ranking, time tracking, attachment upload, estimate voting.

### Provider

A `Provider` is one connection's tracker. Its calls take neutral inputs and
return neutral values. A read fails with the transport's typed errors
unchanged.

- Identity: who the credential belongs to.
- Structure: projects, boards, one board's columns, statuses, estimate and
  sprints.
- Sync: a page of a scope's changed issues from a cursor; the complete set
  of IDs in a scope, or a statement that the set is incomplete; cards for a
  set of IDs; status, sprint, estimate and assignee changes for a set of
  issues.
- Detail: one issue's detail; pages of its comments.
- Writes: create, edit fields, move to a status (with the fields the move
  requires), rank before or after a neighbor, move to a sprint or the
  backlog, sprint create, start, complete and edit, comment add, edit and
  delete, link, attach, and worklog add, edit and delete.
- People and values: users who can be assigned, and the allowed values of a
  field.

**A sync cursor is the provider's own bytes.** `trackerapp` stores what a
page returns and hands it back; it never builds or adjusts one. A page's
cursor is the place to resume after that page, including inside a run of
issues that share an update time, and overlap for a tracker's time
resolution is the provider's business at the end of a walk.

**A write returns an outcome for each thing it touched, not one error.** One
operation can be several requests: a create followed by an image upload, a
rank of 120 issues in three groups. Each issue or step is applied, refused,
unknown or not sent, and the result names every issue the operation may
have changed and any it created. `trackerapi.WriteNotApplied` judges one
request inside a provider; it never judges an operation. A write does not
return a card: an answer that carries an issue carries only the fields the
tracker chose to send.

## Connections and key files

A connection is one credential for one site: a Jira site with the account's
email, a Tempo region tied to a Jira connection, or a Linear workspace. Two
sites of one tracker coexist. Connections belong to the computer and are
rows in `tracker_connections`.

- The token is the only secret. It lives alone in
  `<configDir>/credentials/tracker-<connection id>`, an owner-only file
  read through `internal/keyfile`. The site, the email and the region are
  not secret and are columns.
- `trackerapp` gives each client a `trackerapi.CredentialSource` that reads
  the key file. The client reads it at first use and after one 401. When
  the key file's status is asked for or the user saves it,
  `trackerapp` calls `ForgetCredential`, so a replaced token is picked up
  without a restart.
- Adding a connection creates the row and the empty key file, opens the
  tracker's token page and the key file on that computer, then verifies:
  one identity call to the tracker. The row records who the token belongs
  to, when it was verified and when the token expires if the tracker says.
  A status call returns the file's state and the last verification, never
  the token.
- Editing the key file and opening the token page need the `host` scope and
  have no remote form. Every other call works from a connected browser or
  another computer and acts on the computer the call is routed to.
- Removing a connection deletes its row, its key file and, through the
  schema, its mirror. Turning a connection off keeps the row and the key
  file, stops its pumps, closes its clients and clears its mirror.
- Work in flight cannot write into a mirror that was cleared. A mirror is
  one `tracker_cache_roots` row with its own ID, and every mirror row
  references that ID. Each read or write captures the root ID when it
  starts and every row it stores names it. Clearing deletes the root, and
  turning the connection on again makes a new one, so a result that
  arrives late names a root that no longer exists and the schema refuses
  it. No caller checks a flag.

## Tables

All tracker tables are created by one migration while the feature is
unshipped, so a renumbering after a merge changes one version constant and
one frozen hash.

Durable records, restored and kept like other application records:

| Table | Holds |
|---|---|
| `tracker_connections` | Kind, site, account, the Jira connection a Tempo connection belongs to, label, on or off, verified identity, verification and expiry times. |
| `project_tracker_boards` | Which boards an app project lists, in order, and which opens first. Keyed by project and connection; both cascade. |
| `thread_tickets` | One ticket per thread: tracker kind, site and issue ID with its key and title as last seen. Keyed by thread, cascading with it, cleared when a thread retires to a holder. It names the site, not the connection, so a link survives a connection that is removed and added again. |

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
  connection has at most one root. Clearing a connection's mirror deletes
  its rows in bounded batches and then its root. A test walks the schema
  and fails for a `tracker_cache_*` table that does not reach the root this
  way, and for any other table that references one.
- A restore recreates these tables empty, as it does the search tables.
- Their shape changes by dropping and recreating them in a migration; the
  next sync fills them. They are never altered in place.
- Rows hold columns for whatever a board, a backlog or a metric query
  filters or sorts by, and one JSON column for the rest of a card.

| Table | Holds |
|---|---|
| `tracker_cache_roots` | One row per mirror, with its own ID and its connection. |
| `tracker_cache_projects`, `_users`, `_statuses` | Structure shared by a connection's boards. |
| `tracker_cache_boards` | A board's name, kind, estimate and columns. |
| `tracker_cache_sprints` | Sprints of a board: state and dates. |
| `tracker_cache_issues` | Cards, one per issue per mirror. |
| `tracker_cache_board_issues` | Which issues are on which board, each with that board's order key and estimate. |
| `tracker_cache_issue_sprints` | Which sprints an issue is or was in. |
| `tracker_cache_changes` | Status, sprint, estimate and assignee changes with their times and authors. |
| `tracker_cache_issue_details` | The detail of recently opened issues, one JSON document each. |
| `tracker_cache_worklogs` | Logged time by issue, author and day. |
| `tracker_cache_sync` | Per scope: the provider's cursor, last read, last reconcile, last failure. |

Metric queries read versioned views over these tables, never the tables.

### What the mirror holds

For each board some app project lists:

- its structure;
- its sprints that are active or future, and those closed inside the
  history window (180 days);
- its issues that are not done, those done inside the window, and those in
  a kept sprint;
- every status, sprint, estimate and assignee change of those issues, back
  to their creation. What a sprint started with is rebuilt from changes
  made before it started, so changes are not cut at the window; they go
  when their issue does.

Plus, per connection: the issues linked to threads, and the detail of the
200 most recently opened issues. An issue outside every listed board is
read from the tracker when asked for and is not kept unless a thread links
it.

## Sync

Sync is polling, per scope. A scope is one board, or a connection's linked
issues.

- **Delta.** Each tick reads pages of the scope's changed issues from its
  cursor. One transaction stores a page's rows and the cursor that page
  returned, so a restart resumes exactly after the last stored page. A
  card is never replaced by a version with an earlier update time.
- **Reconcile.** A delta sees only issues whose update time moved. An
  issue enters or leaves a scope without that: a board's filter is edited,
  an issue is deleted, a sprint is reassigned. A reconcile lists the IDs in
  scope, reads the cards of those the mirror lacks, and removes the
  memberships no longer listed. A listing the provider reports as
  incomplete adds and never removes. It runs when a board is subscribed, on
  a slower cadence, and on request.
- **Structure** is read when a board is subscribed, on a slower cadence,
  and whenever a delta names a status, sprint or user the mirror lacks.
- **History** is read whole for an issue new to the mirror and again for
  an issue whose update time moved, limited to the four change kinds.
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
`pumpCore` in `internal/app` today, bound to the pull request types and
mutex. It moves to its own package with the request type as a parameter and
its lock supplied by the owner, and the pull request pumps use it from
there, so there is one implementation.

## Writes

Every edit goes to the tracker first.

1. `trackerapp` calls the provider and gets the operation's outcomes.
2. Every issue whose outcome is applied or unknown, and every issue the
   operation created, is read again through the same card read sync uses,
   and stored. There is one way a card is built.
3. The caller gets the outcomes as they are: all applied, all refused with
   the tracker's reason, or a mix that names which issues and steps
   landed. An operation that created an issue and then failed a later step
   reports the issue as created.
4. Subscribers get the stored changes.

The mirror is never written ahead of the tracker. A client that shows a
drop before the answer owns that preview and replaces it with the event.

A write an agent made is recorded in `tracker_agent_writes` (thread, issue,
what changed, when), which the ticket's history view joins.

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
to a tracker, and it sends a draft's day when the user approved it or when
the project's setting posts days with no open question. No tool reaches
the provider's worklog write.

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
  issues that share an update time, reconciles that add and remove, an
  incomplete listing, cascades, each mix of write outcomes, pump lifetimes,
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
