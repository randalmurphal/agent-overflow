# Issues

Status: product rulings and screen designs recorded 2026-11-15. Built:
the connections and a project's board list, in Settings > Issues
(Connections, Boards, Status groups, Fields, Starting work, Agents),
and the Issues pane with its Board, Backlog and Time
screens, opened from the tool row's Issues entry, and the Ticket screen,
opened from a card or a row, from a thread's linked ticket or from a pull
request's ticket chip; a thread's ticket link, in the sidebar and the
thread header, and the Dashboards screen. The other screens are not
built; the [Board](#board),
[Backlog](#backlog-and-sprint-planning), [Ticket](#ticket),
[Dashboards](#dashboards),
[Tickets, pull requests and branches](#tickets-pull-requests-and-branches),
[Thread link](#thread-link) and [Time](#time) sections say what of them is. A
project's status groups are set in Settings > Issues > Status groups, its
ticket pins in Settings > Issues > Fields or on the ticket, its Start work
defaults in Settings > Issues > Starting work or from the Start work form,
and what its threads may do to tickets in Settings > Issues > Agents.
Agents can find, read, create and change tickets, a change of several
tickets waiting for the person's approval unless the project turned that
off. They read and draft the person's time, and ask to log the draft of
named days, which is logged only once the person approves what the
prompt shows ([Agent tools](../architecture/tracker-backend.md#agent-tools)).
What they read beside the logged time is the [activity](#activity) each
computer measured and the day's commits; reviews and calendar meetings
are not read yet.

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
  is not changed for this.** The ADF and Markdown converter is the copy,
  with its hand-written golden files; the refusal of an edit that would
  lose formatting, which lived in atl's command layer, moved into it. The
  rest of atl is bound to its command context and its one site, so the
  tracker clients are written here with atl as the reference for logic and
  edge cases: the HTTP layer to the forge transport's rules, typed inputs
  in place of atl's text conventions for field values, and typed errors.
  atl's recorded responses hold real site data and are not copied; nor are
  its aliases, reports, prompts, dry runs, disk cache or relative date
  parsing. [Tracker clients](../architecture/tracker-clients.md) is the
  contract for what was built.
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
  the app.** A project's Agents setting can turn off its threads' ticket
  changes, or turn off asking before a change to several tickets
  ([Setup](#setup)). Each agent write is marked as the agent's in the
  ticket's history view.
- **Time is drafted by an agent and posted on approval.** The app supplies
  the measured inputs: what is already logged, when the user was active in
  the app, which tickets each day's threads, commits and reviews touched,
  and calendar meetings when that source is on. Hours come from the user's
  activity and meetings, never from how long a thread ran. The agent
  decides how a day splits between tickets and records its reason on each
  entry. What it cannot decide becomes a question. Nothing is posted until
  the user approves a day or the whole draft.
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
  handle is hidden (`internal/pump`). Unattended reads wait
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

- The pane opens on the board the person picked last. When its computer
  refuses that board (no project lists it, or its connection is off or
  removed), when that computer is no longer paired or does not let this
  window read boards, or when none was picked, it opens the board that
  opens first of the project it was opened from, else of the first project
  by name that lists one, on this computer first and then on the others in
  the order the app lists them, once each computer has answered or could
  not be read. An archived project's board opens this way only when no
  other project lists one; the picker lists it, and a pick of it opens. With no board listed, its way to Settings is the project it
  was opened from, else the first project by name. A board whose connection is turned
  off is listed in the picker but not offered, and the pane never opens it
  on its own. While the board's computer is not connected, the pane says so
  and shows the board when it connects.
  Each open pane keeps its board and Only mine; a pick and Only mine are
  what the next pane opens with. A board that ended because its connection
  was turned off opens again once the connection is turned back on. A board
  its project stops listing while another project of the same computer
  lists it stays open under that project, and a pick of it moves with it.
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
- A card, a linked ticket or the parent pressed with Ctrl or Cmd, or
  Open in its own pane in their menus, opens the ticket in a pane of its
  own right of the focused pane, at the average pane width, and focuses
  it; a ticket that already has one is focused there instead. In its own
  pane in a ticket's menu moves the ticket into one, with what is being
  written on it, and the Issues pane shows the list again. The pane
  wears a tool pane's header in every state, with maximize and close: its
  title is the ticket's key (Ticket until the key is known), moves the
  pane, and maximizes it on a double-click; the ticket's own controls sit
  beside it. While the ticket's computer is not connected, the pane says
  so and shows the ticket when it connects. A linked ticket opened in it
  takes its place, with Back, as in the Issues pane. Escape closes the
  Details rail, then steps back, and never closes the pane. A thread
  opened while the pane is focused takes its slot, as it does a tool
  pane's. A pane move by keyboard puts the keyboard in the ticket, so the
  arrows and Page Down scroll it. The layout saves which ticket the pane
  shows (its computer, connection and issue) and nothing the tracker
  wrote: after a reload the pane shows that ticket without its Back trail,
  titled Ticket with an empty title line until the ticket is read, and its
  points are those of the first board the ticket is on that has estimates,
  the board an agent's `ticket_read` prints. One whose ticket
  cannot be read back says so until it is closed.

### Board

Status: built: the columns of a sprint or kanban board, cards, Display,
Only mine, Search, Refresh, the board picker and the compact strip;
changing a card's type, priority, points and assignee from its marks
(from its menu under compact), through the ticket's own editors and
writes, so the card and an open ticket show one write; carrying a card
with a mouse or pen to another column, which moves it by the move that
reaches that column (a column no move reaches says so and takes no drop,
several moves ask which, one card at a time, a move that takes fields
opens the ticket with its form; leaving the board before its moves are
known moves nothing and leaves the card free), or within its column, which ranks it (a card let go anywhere
but over a column's cards goes home and changes nothing; several cards
ranked one after another in a column are sent in turn); under compact, the
status mark's Move to…; and the card's menu: Open, Open in its own pane
(where ticket panes are offered), Open in the tracker,
Move to…, Move to sprint…, the four fields, Ask, Start work…, Copy key
and Copy link, some at once and the rest behind Show more (Ask and Start
work among the rest until the person chooses), the set shown at once
chosen in the menu and kept on the device. Ask and Start work do what
the ticket's Work does, in the project its Work would use there: Ask
starts the thread at once, and Start work opens the ticket with its
Start work form, so the run's steps show in the ticket's Work (on a
narrow ticket, in the Details it opens). While a Start work of the ticket
is running, it opens no form: Work shows the run's rows and its Start
work button says the run is going. A dropped card waits where it was
dropped, dimmed, until its write settles, and is not moved again until
then. A refusal shows under the card or the ticket when one is in view,
else in a toast. Not built: the summary row. A sprint on the board is read whole, and while Only mine
or Search is on every column is read whole, so a board of thousands of
cards holds them all. A card's state line shows days in the status but not a
running thread or what blocks it; its parent shows as a chip but is not
known to be an epic; its type and priority marks are picked by name, not
the tracker's icons; its assignee shows as initials, not the tracker's
avatar.

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

Status: built: the board's open sprints, the active ones first, then the
backlog, each a header and its rows, the folds remembered per board on
the device with the active sprint folded at first; a row's type,
priority, points and assignee from its marks (from its menu under
compact), through the ticket's own editors and writes; the row's menu,
the card's; carrying a row with a mouse or pen, level, the rows under it
sliding to open a gap, which ranks it within its section or moves it into
another sprint or the backlog at that place, a dropped row waiting there
until its write settles and not moved again until then; under compact,
Move to sprint… on the menu. A future sprint says its points against the
average of the board's last finished sprints (at most five, worded as
"the last sprint" or "the last N sprints' average"), each person's load,
and how far it is over; a finished sprint with no ticket on the board
counts as zero, and a ticket whose history the app has not read yet
counts as done only in the last sprint it is in. The backlog says its
tickets, its points and how many have no estimate. New ticket at a
section's foot asks the fields the tracker requires the person to give
and offers those it fills itself, sending nothing for one put back to
none; on Jira it shows the project, one of the projects the board shows
(the one picked last on that board this session, else the board's
location, else the only one, else it asks; while that list is being
read, or could not be, the board's location), and the type, the one
picked last on the board this session, else the project's first at the
standard level. The values given in the form are kept for each project
and type: the form shows, and Add sends, only those given for the
project and type it shows, and coming back to one, by a pick or by the
board's projects, shows what was given for it. The form offers what the
tracker offers now: opened, or back in view, more than a minute after it
last read the projects, types and fields, it reads them again while it
shows what it holds, and the screen's Refresh reads them again too; a
value given that the tracker no longer offers is said before Add and not
sent, and a project or type the person picked that it no longer offers
is said and asked for again before Add. What was typed in the form, a
field being typed in included, outlives the form scrolling out of view. A ticket the tracker
made stays made: when its move into the sprint failed, or may have, the
form says so with the ticket's key and offers that move until that move
applies, the board shows the ticket in that sprint however it got there,
or the person dismisses it. On Linear a new ticket lands after the section's last row. New
sprint, a sprint's Change (which cannot clear a sprint's dates, and says
so), Start sprint (today for as long as it was planned, else two weeks)
and Complete sprint, which says how many of the sprint's tickets are
done and how many are not, as Jira's completion counts them (a ticket is
finished when its status is in the board's last column that has
statuses), and always asks where the tickets the tracker counts
unfinished go: an open sprint of the board, the first future one at
first, or the backlog; a sprint that left the list while the form was
open is not sent. The form counts by the board's columns as this computer
last read them for the board, and the completion by the board's
configuration as this computer last read it for its writes, which can be
another read. A ticket opens beside the list, and in its own
pane with Ctrl or Cmd or the menu; the row's menu has Ask and Start
work…. Not built: the pace counted with credit (it counts the tickets
whose status counted as done when their sprint was completed, by their
estimates now); support tickets are counted among those without an
estimate; a Linear team's cycles are started and completed in Linear,
which the screen says; a completion the tracker refuses part of says the
first refusal, not how many tickets moved.

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

Status: built: a ticket opened from a card, from a thread's linked
ticket or from a pull request's ticket chip, over or beside the board, the choice remembered, with the split between them, or in a pane of its own; the wide and narrow
layouts with the rail and Details; status, assignee, priority, points and
type changed in place, a move whose screen takes fields through their form (which asks for those the tracker requires the person to give, and offers no None for a field the tracker requires, a field of several keeping its last value), and the other fields with allowed values or one
typed value behind the control with its find box (a field of several
typed values is shown, not edited); each long text field as a
section that folds and is edited as Markdown, an image or attachment in
its text shown as that attachment (an image up to 16 MB in place while
it is near the view, any other file with its size and Save), else as a
chip naming the file;
comments in either order
with the control and the comment box, edited, deleted and copied from a
comment's menu; history; linked tickets grouped by relation, each
opening in the ticket's place, added through a form that finds the other
ticket by key or words, and keeps the relation picked when its list is
read again or says it is no longer offered, and removed from their row; attachments, attached
one at a time by picking, dropping or pasting, each saved or copied from
its row and an image up to 16 MB opened in the app; on a Jira site that
has it, planning poker, folded until opened, with the estimate set from
its result in the section or the ticket's menu; on Jira, the
estimate in the header and Log time; the control that opens it in the
tracker; Back through the tickets opened from one another; and Work, with
Ask and Start work; and the thread an agent's comment or change came
from, on the comment and in the history; a field pinned for the project
from its row, a section from its header. Not built: the mark on a section
holding the template text.

- At 720 or wider the content is on the left and a rail on the right holds
  Fields, then Work. Narrower, the ticket shows the content only, with one
  row of status, assignee, priority and points, each changed in place, and
  a Details control that slides the rail over the content.
- The ticket's points, its Estimate and planning poker's estimate are
  those of the board it was opened from when that board has estimates,
  else of the first board it is on that has them, the board an agent's
  `ticket_read` prints and its `estimate` sets.
- A ticket opened from another ticket takes its place. Back returns
  through the last 20 tickets opened that way, then closes the ticket;
  Close leaves at once. Picking another board drops them, and a reload
  does not remember them.
- Fields pinned for the project show first on every ticket of that
  project, after the card's own rows (type, status, assignee, priority,
  points and sprint), which take no pin. The rest sit behind a control
  with a find box, filled fields first. A field is pinned from its row,
  by a pin that shows while it is pinned and on hover or focus of the row
  otherwise (always under compact). A pinned field this ticket lacks is
  listed behind the control, "Pinned for" the project, with its unpin, so
  a pin no open ticket shows can be removed from any ticket of the
  project on that site. A Linear ticket's fields take no pin. A field with allowed values is
  changed in place. Type is changed wherever its mark shows; the tracker
  may refuse a
  [type change](../references/tracker-api-measurements.md#jira-cloud), and
  the refusal is shown.
- Each long text field is its own section. Sections fold, and fold state is
  remembered. A section is pinned per project from its header, the
  description, linked tickets and attachments included; until the
  project pins any, those three are pinned. Pinned sections show in the
  order pinned, and unpinned ones collapse into one line that names them,
  where a pinned long text field this ticket lacks is listed with its
  unpin. A section still holding the project's
  template text is marked as not filled in.
- A pin saves at once to the project's setup on its computer, and every
  open ticket of the project lays out by it. A pin that lost to another
  save, or whose save failed, is said in a toast naming the field or
  section and the project, and the tickets show what is stored. A setup
  that cannot be read is said on the rail, and the ticket shows its
  default layout with no pins; one a newer version saved offers no pins.
- Work lists the threads, pull requests and branches tied to the ticket,
  each with its state and owner. It offers Ask and Start work, and shows
  the project their thread goes in, which the person can change: the
  project the board was opened under, or the one the opener named for a
  ticket opened from a thread or a pull request. Opening the ticket
  already shown from another thread or pull request takes that one's
  project, unless the person picked one. A ticket moved to a pane of its
  own keeps the project it had; one opened there from a linked ticket, or
  restored after a reload, has none until the person picks it. There are
  no suggested prompts.
- Comments can be read oldest or newest first, and the order is remembered.
  Past five, earlier comments sit behind a control. The comment box stays
  in view while comments are in view. A comment written by an agent names
  the thread it came from.
- The ticket also shows history, attachments (attach by picking, dropping
  or pasting), linked tickets grouped by relation, and a control that opens
  it in the tracker. Jira tickets offer Estimate and Log time.
- A Linear ticket has no type mark, a fixed field list, and no time or
  poker controls. Until the ticket's computer answers its opening, it offers
  neither tracker's own controls. They stay while the computer reconnects.
  An open Log time keeps what was typed and logs once the computer confirms
  the person's account again; it closes if the computer answers that it
  knows no account there.
- The parent's chip opens the parent; its right-click or long-press menu
  opens it here or in a pane of its own and copies its key.

### Tickets, pull requests and branches

Status: built: every bullet below. A key
matches when this computer's copy of an enabled connection holds an issue
of its project. A ticket's Work reads the stored list of open pull
requests, the pull requests its linked threads were last seen with
(merged and closed among them) and the branches of the projects that list
its connection's boards, and searches the forge of each of those
projects' repositories once. It is read when Work shows and on the
ticket's Refresh, never on a timer. The rows Work stores show at once,
with "Searching the forge…" until the search answers; a search that fails
is said in Work in the Pull requests page's words, beside the rest. A pull
request found by the search shows its state and author and no CI.

On a Jira ticket the same read takes the pull requests Jira's development
data links to the ticket from the site's forge integrations, the case
below among them (a merged merge request whose key was only in a branch
since deleted). It is read beside the forge's search, and Work waits for
it at most 10 seconds. Such a row shows its state, title and author as
Jira reports them, opens on the forge like the others and is listed once:
a pull request the stored list or the search lists stays as they list
it. Jira's link is the evidence, so its rows need not name the key. A
status Jira reports that is not open, merged or declined shows as Jira's
own word. A link to an address the app cannot open as a pull request, or
with no address, is not listed; Work says how many there are. Each
integration is read on its own: one whose read fails leaves the others'
rows, and each failed read is said under the pull requests with its
endpoint, beside the rest; errors Jira reports from an integration are
counted, not quoted. A read that does not answer within its wait is
said beside the other failures, and Refresh reads it again. A ticket the
data links nothing to
says nothing, and so does a site whose summary answers 404 or has no
pull request part (unmeasured: what a site with no forge integration
answers). On a success it costs one request to Jira for a ticket with no
pull request and one more per forge integration that links one; a
refused or limited answer can add the transport's retries within the
wait, a credential replaced during the read runs it again once, and a
404 that names no account adds a sign-in check. A Linear ticket costs
none. These endpoints are undocumented and can change without notice
([measured](../references/tracker-api-measurements.md#development-data)).

The search finds a pull request in any state by its title, and by a
branch name that holds the key: a branch the searched repository still has, a branch
this computer has (local or fetched), and on GitHub any branch whose name
begins with the key. The search misses a merge request whose key is only
in its branch name once that branch is deleted on the forge, unless this
computer still has the branch; on GitHub that holds only when the key is
not at the branch name's start. On Jira the development data lists such
a merge request when the site's integration links it. A fork's parent is searched only for the
branches of the ticket, not by title. Each repository's search reads at
most 500 matches of each kind and says so when there were more.

- Ticket keys are found in branch names and pull request titles. Only the
  keys of connected tracker projects match.
- The match is used in both directions: the Pull requests page shows a chip
  per ticket that opens it, and a ticket's Work lists its pull requests and
  branches.
- A pull request row opens its review in the ticket's place, the one the
  Pull requests page shows, in any state: over the board or beside the
  list, in the ticket's own pane, full screen on the phone. A bar over it
  holds Back, the number and Open on GitHub or GitLab, and stays when the
  review cannot load. Nothing of the ticket shows or takes the keyboard
  under it. Find
  searches the review while the person is in it. Back closes what is open
  in the review (a CI log, the conflicts, the phone's file list), then
  shows the ticket as it was. A row opens on the forge instead when no
  project of the ticket's computer has the pull request's repository as
  its origin: a fork's checkout cannot show its parent's pull request,
  since the review fetches it from origin. The row's menu opens it on the
  forge either way.
- The forge is searched by key for a pull request outside the stored list,
  and a match counts only when its title or branch names the key by the
  same rule. Jira's own
  [development data](../references/tracker-api-measurements.md#development-data)
  is a fallback only, because its endpoints are undocumented: it is read
  beside the forge's search, and never replaces a row the forge's search
  or the stored list gives.

### Thread link

Status: built: every bullet below.

A [second mockup](issues-thread-link-mockup.html) shows the link in the
sidebar and the thread header, on desktop and on a phone.

- A thread linked to a ticket shows the ticket's status mark and key as a
  button in the thread header, before the pull request button. It opens the
  ticket in the Issues pane beside the thread, over the board shown when
  that board is of the ticket's connection, else over the thread's
  project's board of that connection, else another project's. A pane
  showing Time switches to the Board for it; the next pane still opens on
  the screen the person picked. Its
  right-click menu opens the ticket, copies its key and removes the link.
  On a phone the ticket is the Thread actions menu's row above the pull
  request.
- The sidebar row's menu opens the ticket and removes the link, and the
  Thread actions menu removes it.
- Link ticket… (Change ticket… on a linked thread), in the sidebar row's
  menu, the Thread actions menu and the header ticket's menu, finds a
  ticket by key or by words of its title on the thread's computer's Jira
  and Linear connections that are on, and links the thread to the one
  picked. A computer with no such connection says so. A New thread
  linked before anything is typed becomes a thread with the link.
- A link keeps a thread that has sent nothing, as a bound worktree does:
  it is listed, and erasing its text keeps it. Once its link is removed,
  a thread with nothing sent and nothing typed is an empty draft again.
  A linked draft moved to another project takes its link with it.
- A link its computer has not read, cannot see now or reads through a
  connection that is off dims its mark and says which in its title. A
  ticket whose connection is off or gone does not open: a message says
  what to turn on or add.
- The link outlives the pull request. It stays after the merge and after
  the worktree is removed.
- In the sidebar the key and its status mark join the row's second line,
  the one that names the computer and the worktree. The key is left out
  when the worktree name already contains it. A linked thread with no
  second line gets one for the key. No other row grows.
- Sidebar search also matches a thread by its ticket's key and title.

### Dashboards

Status: built: Overview, the dashboards a project keeps and the picker,
Team or Mine, the range, Ask your agent, edit mode, which removes
widgets and removes the dashboard with its last one (there is no
Delete), and the widget an agent proposes, which the person keeps or
dismisses; a number, bar or cell's tickets, a number's change and trend, narrowing to a person or
an epic, and selecting rows in a ticket list or a drill to move, assign
or start a thread on them. A query reads the board's tickets only: pull
requests, threads and logged time are not built, nor the credit rules in
How this is counted, or a number tile's limit.

- Every board has a built-in dashboard, Overview: its usual pace, days
  from start to done, the tickets stuck now, what was finished per sprint
  or week, who is carrying what, what was created and finished, and on
  Jira open work by epic, counted from the computer's copy of the board,
  which keeps syncing while the dashboard shows.
- A dashboard is a twelve-column grid of widgets with a toolbar: dashboard
  picker, Team or Mine, the range, Ask your agent, and an edit mode that
  removes widgets.
- A query may read tickets, pull requests, threads and logged time.
- A number, bar or table cell opens the tickets behind it. Each widget
  shows how it is counted: the rule in plain words, the credit rules it
  reads, the query, and its sources.
- A number tile shows its change against the previous range and a trend
  line. The arrow gives the direction and its color says whether that
  direction is good. When the range before left tickets out, the change
  says how many ("Up 0.5 tickets from 0.5, the 2 weeks before, 2 not
  counted then").
- A number says how many tickets it could not count beside it ("2 not
  counted"), and reads "Not known" with no change mark when it counted
  none. A drill's header gives its number and its rows and tickets ("6
  rows, 5 tickets" when a ticket counts in two sprints); each row names
  the period it counts in, and a row not counted says "Not counted" and
  why.
- A chart lists the tickets it draws in no bar or point (a finish whose
  time is not known) in a "not counted" that opens the whole chart. A
  chart or table that shows only its first 12 bars or 50 rows says how
  many more ("2 more epics"), which opens the tickets of the ones left
  out.
- Until the board's first read has finished, the dashboard and each
  drill say "This board's first read has not finished: these numbers
  count the tickets read so far.", and nothing is compared.
- While a new range or narrowing is counted, the last answer's numbers
  stay dimmed and take no click. A count that fails says "Not counted
  again" and why beside the last answer. Another dashboard picked shows
  none of the last one's numbers. The widgets, the proposals and Keep,
  Dismiss, Edit and Remove are there whatever a count does.
- Ask your agent opens a new thread in the board's project, its draft
  unsent and naming the board by name and site/ID, on a provider whose
  agents get the tracker tools. With none turned on and installed on the
  board's computer, Ask is disabled and says why.
- Clicking a person or an epic narrows every widget on the page to it. A
  chip in the toolbar clears it.
- Rows in a ticket-list widget or a drill can be selected: a row's box
  shows on hover, and on every row once one is selected. The bar says how
  many are selected and offers Move to, Assign and Start a thread, and
  says under them why one cannot run. A count again keeps the selected
  rows it still lists, and closes an open Move to, Assign or Start a
  thread whose tickets it changed, and an open Move to whose tickets'
  statuses it changed, so none acts on a ticket the list no longer shows
  or plans from a status it has left.
- Move to lists the statuses the selected tickets can move to. A ticket
  already in the status, as its row shows it or where this list last
  moved it, is left as it is ("3 tickets moved to Done, 1 already in
  Done"). A status that a ticket cannot reach, or reaches only
  by a move that asks for fields, is off and names the ticket. Assign
  lists the people every selected ticket's project may assign, and
  Unassigned; when a project's list cannot be read it offers nobody and
  says which project and why. Each ticket is its own write, at most four sent at a time,
  and what the tracker refused shows under that ticket's row ("2 of 3
  tickets moved to Done"). The bar's note says what came of it for 5 s.
- Start a thread makes one new thread in the board's project on up to 8
  tickets, one file each on its unsent draft with the text typed, and
  opens it. It is linked to a ticket only when there is one; several are
  named in its title ("WEB-1, WEB-2, WEB-3 and 2 more"). Nothing on the
  tickets changes. A drill closes as its thread opens. A list starts one
  thread at a time; a start that is refused keeps the text and the
  selection. Typed text stays with the tickets it was typed for.
- An answer to a one-off question appears as a provisional widget. It joins
  the dashboard only when kept.
- A number tile can carry a limit. Over the limit, the tile is marked on
  the dashboard and on the board's summary row.
- Charts are drawn to the widget's width and have hover values. The three
  series colors were checked for contrast and color-blind separation on the
  dark and light surfaces.

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

Status: built: the person's own logged time on the connection of the
board the pane shows, a week at a time, By day and By ticket, logged,
changed and deleted by hand from a cell, a day or an entry; and an
agent's draft (`time_draft`), shown and approved as the bullets below
say. Not built: points on a support ticket's entry.

The week's draft shows dashed under each day's logged time in By day,
with each day's approval, and in its tickets' cells in By ticket, where
the questions are a last Not placed row. A banner above the week says
what the agents drafted in it, with Discard and the approval of the
whole week shown. The drafting thread's card shows the call's entries by
day, with each day's approval, the call's, Discard and Review in Time,
which opens Time at the draft's week, over the board the pane shows when
it is of the entries' connection and otherwise the first board of that
connection. The week and the card follow an agent's draft, a
person's change and a post without a reload, on the computer the thread
runs on, a paired one included. Discard asks once.

An approval says what posted, what did not, and why, in the tracker's
words. A day's approval skips an entry whose ticket has had time logged
on that day since the draft, and says so; Log alone posts it. A day with
an open question posts its entries and leaves the question open. An
entry whose post may have landed (its outcome unknown) shows that state
and the sentence of what to do, with Remove and, on Time, Log by hand: a
log form filled from the entry, after whose save the entry is removed.
It never offers a way to post it again. A question answered with a
ticket whose entry that day is still drafted adds its time to that
entry; the question's text becomes part of the entry's reason, which is
the worklog's description once posted.

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
- An agent's draft puts meeting hours on the project's meetings ticket and
  time off on its time-off ticket ([Setup](#setup)) without a question.
  When one of them is not set, the agent asks instead. One that is gone
  is refused once Jira is asked, so the agent asks; until this computer
  reads that ticket again, its copy may still answer. With Never log to an epic on, a draft entry on an epic is
  refused, and the agent puts the hours on a ticket under the epic or
  asks. Time logged by hand is not held to it.

### Activity

- While a Jira connection is on, each computer records when the person
  gave input to the app on its screens, and on which thread, and which
  threads started a turn each day. A computer with no Jira connection on
  records nothing: turning the last one off keeps what came before and
  records nothing after, and turning one on records from then.
- Input is a key, a press, a scroll or a touch. A minute counts when
  there was input in the 3 minutes before it, the last input of a burst
  included. Moving the pointer is not input. A screen asleep or in the
  background adds nothing when it wakes.
- The thread is the one the screen shows: on the phone, an agent thread
  opened from the Agents rail counts on that thread. When the thread or
  the layout the screen shows changes, the time up to the change stays
  with what was shown before it, and the time after goes to what is
  shown now. A change adds no time of its own.
- `phone` time is time in the compact layout: the phone shell, or a
  browser on a narrow touch screen.
- A computer counts the screens attached to it, and only those of its
  own person: its own window, a browser on it, and its own devices. A
  screen attached only to another computer is not in its answer.
- Reading a page in the desktop's browser pane counts only within 3
  minutes of the last input to the app, since the page takes its own
  input. The operating system's menus and dialogs do not count either.
- A thread's minutes go to the ticket it is linked to now, so relinking
  it moves its past minutes. Days are the computer's local dates. Linear
  tickets get no minutes.
- When the computer cannot write what it measured, an agent's read says
  since when, and so does `GetActivityRecording` for the Time screen. A
  crash loses at most the last 5 minutes measured.
- The measure is kept 92 days. Its mechanism, costs and limits are in
  [Activity](../architecture/tracker-backend.md#activity).

### Setup

- Setup is the Issues group of Settings, where the mockup draws it inside
  the pane: one page per section, Connections and Boards first.
- Connections belong to the computer. Each shows who it is signed in as
  and how long its token has left, and warns before the token lapses
  (two weeks before the date entered). Adding one walks the credential
  steps in the rulings. Tempo is added from its Jira connection.
- Status groups lists, per tracker site, each status of the project's
  boards as this computer last read them, with the tracker's own group,
  and a pick of another group or left out of counts, saved at once. The
  tracker's group picked again puts the status back. A status regrouped
  that no board shows now is listed apart with Put back. A board this
  computer has not read is named, with Check again; once it has been
  read, Check again or the page shown again lists its statuses. The
  Board, the Backlog, their counts and every status mark (a card, a
  Backlog row, a column, a ticket and its linked tickets, a linked
  thread's row and header, which follow the thread's project) follow a
  save without a reload; a status left out of counts keeps the tracker's
  mark, and a ticket shown with no project draws the tracker's groups. A
  save that lost to another, from another window or a hand edit of the
  file, is said, and the page shows what is stored; a save whose outcome
  is unknown says so in the computer's words and reads the setup again.
  A setup that cannot be read is said with its file and Try again; one a
  newer version saved is said, with no change offered.
- Fields, Starting work, Time and Agents are per project and behave as
  Status groups does: each change saves at once, and a save that lost, a save
  whose outcome is unknown, a setup that cannot be read and a section a
  newer version saved are said the same way.
- Fields lists, per Jira site of the project's boards and per site its
  pins name, the fields on top of every ticket and the sections open on
  every ticket, the same pins as the pin on a ticket. What is pinned shows
  at once; a field or section that is not pinned is found by typing its
  name, the exact name first, and at most 20 matches show; two fields of
  one name show their IDs. A site's fields are its whole field catalog,
  several hundred on a real site. Each site's fields are read on their
  own: a site whose fields cannot be read says why beside its pins, with
  Try again, while the other sites show as usual. A site read before keeps
  the fields it last read, to pin and to find; one never read shows its
  pins by ID, which can be taken off. A long text field named as a
  built-in section (Description) shows its ID beside the name, as two
  fields of one name do. A project with only Linear boards says it
  has nothing to pin: a Linear ticket's fields are fixed, and its
  sections are pinned on the ticket.
- Starting work lists each tracker project of the project's boards, and
  any that already has defaults, with what Start work does: move the
  ticket (off, or on to a status the project's boards offer it, the
  first one counted in progress when turned on: a Linear team's own
  boards' statuses, and for a Jira tracker project every status the
  project's boards on its site show, its own boards' first, since a Jira
  board can list several tracker projects' tickets; a tracker project offered no status says
  so, and one whose boards this computer has not read names them),
  assign it to the person, name the branch after the ticket, and the
  project the thread runs in. With naming off, the Start work form
  opens with an empty branch box for the person to name. The form's Save
  as defaults writes the same defaults, so each shows what the other
  saved.
- Agents has three rows: Read tickets, always on; Change tickets, on
  unless turned off; and Ask before changing several at once, on unless
  turned off. Change tickets covers logging time, which always waits for
  the person's approval. With Change tickets off, a thread of the project that
  creates or changes a ticket, or drafts or logs time, is refused at the call with a sentence that
  tells the agent to ask the person to turn it on here. With Ask before
  changing several at once off, a change to several tickets is sent
  without waiting for approval, an exception to the approval ruling that
  the project chooses. A setup that cannot be read, or an agents section a
  newer version saved, refuses every ticket change of the project's
  threads until it is fixed.
- Time names the ticket meetings go on, "Meetings that are not about one
  ticket", and the ticket time off goes on, each found by key or title on
  the connections of the project's Jira boards, shown with its title and
  cleared with Clear, and Never log to an epic, on unless turned off. A
  ticket set that is gone, or that the connection cannot see, is said
  beside it, as is one whose site no connection that is on reaches. A
  project with only Linear boards says it has no time settings, and one
  with no boards says to add one; tickets it chose while it listed a Jira
  board are kept and apply again once it lists one. The draft's sources are not offered
  here until the calendar and call-duration readers are designed.
- The rest is per project: which boards the picker lists and which opens
  first; which group each status belongs to, including left out of counts;
  pinned fields and pinned sections; Start work defaults; the
  estimate field, poker and credit rules; the meetings and time-off
  tickets and whether a draft may log to an epic; and what agents may do.
  What cards and Backlog rows show is chosen with Display, per board, on
  each device.
- A Linear project has no fields, credit or time settings.

### Compact layout

The [compact rules](../../frontend/AGENTS.md#compact-layout) apply: two
screens, the compact hit size, nothing reached by hover alone, no drag, and
menus anchored at their control. Both mockups show the compact layout in a
window 640 wide or narrower. On top of those rules:

- The top bar holds Back, the board picker, one picker for the tabs, a menu
  with Display and Only mine, and Search. Back closes what is open on the
  page before it leaves for the list.
- The page never scrolls sideways. Board shows one status at a time, picked
  from a strip of the statuses with their points. Time by day shows one day
  at a time, picked from a strip of the five days with each day's total and
  whether it is logged, drafted or waiting on a question.
- A card moves by its status mark. A backlog row moves between the sprints
  and the backlog from its menu. Ranking within a list is not offered.
- A backlog row is two lines: the title, then type, key, epic, priority,
  points and assignee. Labels show on the ticket only.
- A ticket takes the whole pane in its narrow form. Back closes the fields
  rail, then the ticket. A ticket does not open in a pane of its own.
- Dashboard widgets stack, number tiles two to a row. A tile shows How this
  is counted and any control that is switched on; the limit and the board
  pin show while the dashboard is being edited. A ticket list shows its
  selection boxes at all times, with each row's note under its title.
- Time by ticket keeps the grid and shows keys without titles.
- Settings lists its sections as the page. A section opens over the list
  and Back returns to it. A switch stays beside its text; a picker or a
  button sits under it.
- A linked thread's sidebar row carries the same second line. The thread
  header keeps its diff badge and menu, and the ticket is a row in that
  menu, above the pull request.

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
  work belongs to a standing agent's loop or watch
  ([wake supervisor](standing-agents.md#wake-supervisor)).
- Telling template text from written text in a section. Comparing a
  section's text across tickets is the proposed method and is untried.
- Light-theme chart colors.
