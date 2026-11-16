# Issues

Status: product rulings and screen designs recorded 2026-11-15. Nothing is
implemented, and the tool row has no Issues entry until it is.

## Goal

Do what the user opens Jira or Linear for, in the app, without loading the
tracker's own UI. Issues is a tool pane in the shell that
[Pull requests](pull-requests.md) defined.

The user must be able to:

- Tie a thread to a ticket: start a thread, worktree and branch from a
  ticket, see a ticket's threads and pull requests, and see a thread's
  ticket.
- Read boards and backlogs, and reorder them by drag.
- Plan sprints: create, fill, start and complete a sprint with the points
  of each drop target visible.
- Edit every field a ticket supports for its project and issue type,
  including custom fields, transitions with screens, comments, links and
  attachments.
- Set up each tracker project: which boards, which fields show, which field
  is the estimate, how statuses group.
- Read velocity and sprint progress split by "mine" and "team", with credit
  rules the user defines. A project without estimates counts tickets.
- Ask an agent to build and adjust dashboards.
- Log time against tickets through Tempo, and approve the time an agent
  drafts.
- Vote in planning poker from the ticket, where the site has the supported
  poker app.

Out of scope:

- Jira administration (workflows, schemes, permissions, field
  configuration) and service desk queues.
- Jira Data Center and Server.
- A Confluence UI. Agents reach Confluence through the tools the user
  already configures (a CLI or an MCP server); the app renders links only.
- GitHub and GitLab issues. The pull-requests spec named them for this
  pane; they are not covered by these rulings.

## Rulings

All dated 2026-11-15.

- **The app has its own tracker client.** Boards, ranking and field editing
  need a typed client with paging, retries and a local mirror; a CLI or an
  agent MCP server cannot drive them. This covers the Issues pane. The
  2026-07-11 ruling in
  [workflows-system-decisions.md](workflows-system-decisions.md) still
  governs workflow starters, which bind to profile-declared commands.
- **Jira Cloud and Linear are built together on a tracker-neutral core.**
  Each tracker keeps its own provider code; the shared core is shaped by
  both so neither is retrofitted.
- **Cloud only.** Data Center is closed to new customers and reaches end of
  life in 2029.
- **The credential is the user's own API token or key in an owner-only key
  file on the owning computer.** It never crosses the app connection: the
  app opens the tracker's token page and the key file for the user on that
  computer, verifies the token against the tracker, and reports status
  only. Token expiry is shown before it lapses. `internal/openaikey` is the
  precedent. This is a scoped exception to the forge ruling "no stored
  token", because neither tracker has a CLI login to borrow and Atlassian
  OAuth has no flow for a desktop app without a shipped secret.
- **Reusable code from the `atl` project is copied in as files, without
  history. Nothing is imported from it, `atl` stays a separate tool, and it
  is not changed for this.** The ADF and Markdown converter, date parsing
  and error types copy as they are, with the converter's golden files. The
  HTTP layer, which never replays a write and marks a write whose outcome
  is unknown, and the Atlassian client copy with their terminal output and
  disk cache removed. The Jira and Tempo logic (the field-by-name value
  builder, create and edit metadata, transitions, comments, worklog
  validation) is bound to atl's command context, so it is separated from
  the commands on this side: its reads already return typed values, while
  most writes and the worklog validation sit inside command functions and
  are reshaped to take a client and return values. The refusal of an edit
  that would lose formatting lives in atl's command layer, not the
  converter, and is carried over with it. atl's command tests assert
  terminal output and do not move; their fixtures can. Copied files carry
  no site-specific field names, field or status aliases, or report
  conventions.
- **Agents reach a tracker through tools the app provides to its threads.**
  The tools read, search, create, edit, transition, comment, move between
  sprints and list and add worklogs. A write goes through the app's client,
  so the mirror and every open view update, and the write is a timeline
  event in the thread.
- **The local mirror in SQLite is a cache; the tracker is the authority.**
  Every edit is written to the tracker first. A lost mirror costs a resync.
- **Sync is polling.** Neither tracker can deliver webhooks to a computer
  with no public URL.
- **A metric is a named read-only query over versioned views of the mirror.
  A dashboard is a declarative layout of app-drawn widgets that reference
  metrics by name.** Agents author both. No agent-authored code executes,
  so a dashboard cannot reach the network or the app's API. Every number
  opens the tickets behind it, because a query can be valid and wrong. A
  widget has one of five forms: number, bars, line, table or ticket list.
- **Agent-written pages are not part of dashboards.** The app's
  Content-Security-Policy refuses scripts the app did not ship and framed
  content on every surface. Ticket text is written by other people and can
  steer the agent that builds a page, and a sandboxed frame can still
  navigate itself to an outside address carrying data. A custom page would
  be a separate page in the browser pane reading the same metrics; it is
  not built.
- **A credit rule is a named value with a first sprint, read by metric
  queries by name.** Changing how credit is counted edits the value, never
  a query. A change counts from the current sprint, and finished sprints
  keep the value they were counted with, unless the user applies it to
  every sprint. Rule values are per-project settings.
- **The pane shows one tracker board at a time.** There is no view that
  lists the user's work across boards. The board picker lists the boards of
  every connected project and shows a count beside another board that has
  something waiting on the user.
- **Start work applies per-project defaults.** It can move the ticket to a
  chosen status, assign it to the user, name the branch after the ticket
  and pick the repository the thread runs in. Asking about a ticket starts
  a thread with the ticket attached and changes nothing on the ticket.
- **Any thread may read tickets. A thread that is not read-only may write
  them. A write to more than one ticket waits for the user's approval in
  the app.** Each agent write is marked as the agent's in the ticket's
  history view.
- **Time is drafted by an agent and posted on approval.** The app supplies
  the measured inputs: what is already logged, when the user was active in
  the app, which tickets each day's threads, commits and reviews touched,
  and calendar meetings when that source is on. Hours come from the user's
  activity and meetings, never from how long a thread ran. The agent
  decides how a day splits between tickets and records its reason on each
  entry. What it cannot decide becomes a question. Nothing is posted until
  the user approves a day or the whole draft; a per-project setting can
  post the days that have no open question.
- **Calendar and call-duration reading are per-project settings, off by
  default.** A source that is off is never started.
- **Time logging is Tempo plus Jira native worklogs, on Jira only.** Linear
  has no time tracking.
- **Planning poker is an optional per-site add-on for Easy Agile
  TeamRhythm.** It uses the calls recorded in
  [tracker API measurements](../references/tracker-api-measurements.md#planning-poker-add-on),
  which are undocumented.
  When they fail, the add-on says so and the rest of the ticket view is
  unaffected.

Accepted risks:

- Atlassian stated on 2025-11-21 that integrations must not instruct
  customers to generate API tokens or store them, with enforcement from
  2026-01-01 aimed at Marketplace apps and no answer for tools a user runs
  themselves
  ([staff post](https://community.developer.atlassian.com/t/reminder-migrate-from-using-api-tokens-to-officially-supported-authentication-for-atlassian-apps-integrations/97221)).
  An organization admin can also block API tokens
  ([authentication policy](https://support.atlassian.com/security-and-access-policies/docs/authentication-policy-settings-for-your-organizations/)).
- Linear's terms bar using the service to build a competing product
  ([terms](https://linear.app/terms), 2.2(g)). The ruling treats this app
  as a personal client of the user's own workspace.

## Facts that bind the design

Sources, endpoints and what was measured are in
[tracker API measurements](../references/tracker-api-measurements.md).

- Jira ranks and moves at most 50 issues per call.
- Jira has no sprint report or velocity API. Committed, added and completed
  work is rebuilt from sprint dates and each issue's Sprint and estimate
  field changelog.
- Completing a Jira sprint has no parameter for where unfinished issues go.
  The client moves them, then closes the sprint.
- The estimate field is read per board, not per site.
- Jira token traffic is exempt from the hourly points quota and under burst
  limits. A token lives at most 365 days.
- Board, backlog and sprint issue lists use the `/rest/software/1.0` paths.
- Linear cycles cannot be created through the API. Linear has no custom
  fields, no time tracking and no planning poker.
- Tempo allows 5 requests per second per IP and takes numeric issue IDs.
- Read-only agent SQL needs `modernc.org/sqlite` v1.61 or later for the
  authorizer.
- The app's Content-Security-Policy forbids evaluated and framed content, so
  charts are app components.
- The poker add-on's
  [mechanism](../references/tracker-api-measurements.md#planning-poker-add-on)
  rides on two undocumented Atlassian endpoints.

## Constraints from this app

- The owning computer holds the credential and makes every tracker request.
  The frontend's `connect-src 'self'` means the tracker is reached only
  through scoped RPC, on every surface. See
  [connected computers](connected-computers.md) and
  [remote access boundaries](remote-access-boundaries.md).
- The key file follows `internal/openaikey`: owner-only, read per use,
  status-only RPC, edited on the host. Secrets in memory use
  `internal/secret`. The token appears in no log, error, argv, environment
  or wire message, as in
  [forge transport](../architecture/forge-transport.md).
- `internal/forgeapi` serves GitHub and GitLab only. The tracker client is
  a sibling that reuses its patterns (typed failure kinds, rate gates,
  backoff, an isolated HTTP fake for tests), not a third forge.
- Sync pumps exist while a client is subscribed and pause while every
  handle is hidden (`internal/app/app_pump_core.go`). Unattended reads wait
  for the first client's reads to settle. Turning the integration off
  clears its mirror and stops its pumps.
- Mirror tables are classified as cache at the
  [restore boundary](../architecture/sqlite-store.md#restore-boundary) and
  follow the migration and free-space rules in that document. Metric
  queries run on their own read-only connection with a deadline; the main
  store has one writer and five readers, four reserved for history.
- A tracker project is not a repository. The link between an app project
  and tracker boards is its own record, and per-project tracker setup lives
  in the app-managed project directory, never in the repository. A ticket
  link to a thread is its own table shaped like `thread_pull_requests`,
  which today is constrained to the two forges.
- Ticket text, comments and field values are untrusted input to any prompt
  and are delimited as data.
- Completed chat history is immutable. An agent's ticket write is its own
  timeline event.
- Every surface is supported, including compact layout and connected
  browsers ([frontend guide](../../frontend/AGENTS.md)). Retained frontend
  state is bounded by the open board, ticket and dashboard.
- [Product decisions](../decisions.md) rule out an attention feed in the
  sidebar and record that the Pull requests page keeps no record of what
  the user opened. Issues adds no inbox or cross-board work view.

## Screens

A clickable [mockup](issues-mockup.html) shows each screen with sample
data. It is a static page, not app code. Where it differs from this
document, this document is right.

Widths are CSS pixels of the pane, not the window.

### Pane

- The top bar holds the board picker, the tabs Board, Backlog, Dashboards
  and Time, Display, Only mine and Search. As the pane narrows, the
  right-hand controls become icons, then the tabs fold into one picker.
- A ticket opens over the board, and beside the list on Backlog,
  Dashboards, Time and the Pull requests page. The choice is remembered per
  screen. A ticket is shown beside the list only when the pane is at least
  900 wide.
- The split between list and ticket is dragged, starts at half, is
  remembered per screen and resets on double-click. The ticket keeps at
  least 380 and the list at least 520.

### Board

- A summary row sits above the columns. It is one line of chips until
  expanded into tiles. Its tiles are dashboard widgets pinned to the board,
  at most five. A tile that stands for a set of tickets filters the board
  when clicked. The row is edited in place: remove a tile, add one from a
  dashboard, or ask the agent for a new one.
- A card shows type, key, title, epic, labels, priority, points, assignee
  and one state line (a running thread, days in the same status, what
  blocks it). Display chooses which of these show, per board. Type,
  priority, points and assignee are changed from the card.
- A carried card hangs from the grab point and swings under gravity, with
  damping that rises when the pointer slows. A grab bar at the top center
  carries it level. Each column shows what its points would become while a
  card is over it.

### Backlog and sprint planning

- Sections are the active sprint, the next sprint and the backlog. Fold
  state is remembered, and the active sprint starts folded.
- A carried row stays level and the rows under it slide to open a gap. It
  does not swing. Type, priority, points and assignee are changed from the
  row.
- The next sprint shows its points against the usual pace (the average of
  the last five finished sprints, counted with credit), each person's
  load, and a warning when it is over. While a row is carried, the section
  it left and the one it is over show what their totals would become.
- The backlog counts tickets with no estimate. Support tickets are left out
  of that count.
- A ticket is created in place at the foot of a section. Completing a
  sprint asks where unfinished tickets go.

### Ticket

- At 720 or wider the content is on the left and a rail on the right holds
  Fields, then Work. Narrower, the ticket shows the content only, with one
  row of status, assignee, priority and points, each changed in place, and
  a Details control that slides the rail over the content.
- Fields pinned for the project show first on every ticket of that
  project. The rest sit behind a control with a find box, filled fields
  first. A field is pinned from its row. A field with allowed values is
  changed in place. Type is changed wherever its mark shows; the tracker
  may refuse a
  [type change](../references/tracker-api-measurements.md#jira-cloud), and
  the refusal is shown.
- Each long text field is its own section. Sections fold, and fold state is
  remembered. A section is pinned per project; unpinned sections collapse
  into one line that names them. A section still holding the project's
  template text is marked as not filled in.
- Work lists the threads, pull requests and branches tied to the ticket,
  each with its state and owner. It offers Ask and Start work. There are no
  suggested prompts.
- Comments can be read oldest or newest first, and the order is remembered.
  Past five, earlier comments sit behind a control. The comment box stays
  in view while comments are in view. A comment written by an agent names
  the thread it came from.
- The ticket also shows history, attachments (attach by picking, dropping
  or pasting), linked tickets grouped by relation, and a control that opens
  it in the tracker. Jira tickets offer Estimate and Log time.
- A Linear ticket has no type mark, a fixed field list, and no time or
  poker controls.

### Tickets, pull requests and branches

- Ticket keys are found in branch names and pull request titles. Only the
  keys of connected tracker projects match.
- The match is used in both directions: the Pull requests page shows a chip
  per ticket that opens it, and a ticket's Work lists its pull requests and
  branches.
- The forge is searched by key for a pull request outside the stored list.
  Jira's own
  [development data](../references/tracker-api-measurements.md#jira-cloud)
  is a fallback only, because its endpoints are undocumented.

### Dashboards

- A dashboard is a twelve-column grid of widgets with a toolbar: dashboard
  picker, Team or Mine, the range, Ask your agent, and an edit mode that
  removes widgets.
- A query may read tickets, pull requests, threads and logged time.
- A number, bar or table cell opens the tickets behind it. Each widget
  shows how it is counted: the rule in plain words, the credit rules it
  reads, the query, and its sources.
- A number tile shows its change against the previous range and a trend
  line. The arrow gives the direction and its color says whether that
  direction is good.
- Clicking a person or an epic narrows every widget on the page to it. A
  chip in the toolbar clears it.
- Rows in a ticket-list widget can be selected, then moved, assigned or
  given to a new thread.
- An answer to a one-off question appears as a provisional widget. It joins
  the dashboard only when kept.
- A number tile can carry a limit. Over the limit, the tile is marked on
  the dashboard and on the board's summary row.
- Charts are drawn to the widget's width and have hover values. The three
  series colors were checked for contrast and color-blind separation on the
  dark surface only.

### Credit

- A support ticket carries no points. It counts as its logged hours divided
  by the project's hours-per-point value, and shows hours where points
  would show. A project names which tickets are support by type, label or
  board.
- Review credit is a share of a finished ticket's points for each reviewer.
  A reviewer is anyone but the assignee who did at least a set number of
  the chosen actions on the ticket or its pull request: commented,
  approved, logged time. The share, the number and the actions are
  settings.
- Opening a credited number lists each finished ticket, each review with
  its arithmetic and what the reviewer did, each support ticket with its
  hours, and anyone who acted but did not qualify.

### Time

- By day shows the week as five columns. Each entry shows its ticket, its
  hours and the reason for them. By ticket shows a grid of tickets against
  days.
- An agent's draft appears dashed in both layouts and as a card in its
  thread. A day or the whole draft is approved from either place. An entry
  can be changed, logged alone or removed before approval.
- A question is answered with one click: one of the offered tickets, any
  other ticket found by key or title, or left out.
- Time is logged by hand from a cell, a day or a ticket.
- An entry on a support ticket shows what its hours are worth in points.

### Setup

- Connections belong to the computer. Each shows who it is signed in as
  and how long its token has left, and warns before the token lapses.
  Adding one walks the credential steps in the rulings.
- The rest is per project: which boards the picker lists and which opens
  first; which group each status belongs to, including left out of counts;
  pinned fields, pinned sections and card marks; Start work defaults; the
  estimate field, poker and credit rules; the meetings and time-off
  tickets, draft approval and draft sources; and what agents may do.
- A Linear project has no fields, credit or time settings.

## Open questions

Each answer is proposed, not ruled.

- **Q4. What the mirror holds.** Proposed, not ruled: the boards the user
  selects plus anything assigned to or mentioning the user, with status
  history kept for a bounded window.
- **Q5. Two sites of one tracker on a computer.** Jira, Tempo and Linear
  connections coexist. Proposed, not ruled: two Jira sites or two Linear
  workspaces also coexist.

## Not designed here

- Reordering and resizing dashboard widgets and summary-row tiles.
- Selecting several backlog rows to move them together.
- The calendar and call-duration readers behind the Time sources.
- Running the time draft on a schedule. A reminder fires once; recurring
  work belongs to workflow automations.
- Telling template text from written text in a section. Comparing a
  section's text across tickets is the proposed method and is untried.
- Light-theme chart colors.
