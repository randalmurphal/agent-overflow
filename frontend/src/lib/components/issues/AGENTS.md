# components/issues/

The Issues pane: the body of the `issues` tool pane
(`stores/toolPanes.svelte.ts`, mounted by `panes/ToolPane.svelte`). Product
behavior lives in [`issues.md`](../../../../../docs/specs/issues.md); the
board methods, events and the patch read in
[`tracker-backend.md`](../../../../../docs/architecture/tracker-backend.md#methods-and-events).

## Owners

- `stores/trackerBoard.svelte.ts` holds the one board subscription the pane
  shows: subscribe, release, the event fold, the one read in flight (full
  read, patch, page), visibility votes and Refresh. Components call its
  actions and read `attachTrackerBoard(...).current`; none calls a board
  binding. Which sections the Board screen reads is the board's computer's
  (`BoardView.screen`), and the Backlog screen's (`BoardView.plan`), held
  while a pane is on Backlog (`holdTrackerBoardPlan`); the store asks for
  no sections and holds what it answers. A write waits on `trackerBoardsSettled` (its end cases are
  written beside it) before its "Saving" ends.
- `stores/trackerBoardModel.ts` is the pure part: the patch rule (replayed
  in Vitest from `internal/trackerapp/testdata/board_patch_cases.json`, which
  the Go model writes), the grouping of sprint rows into columns
  (`screenColumns`), the group a count puts a row in, by the project's
  status groups (`groupOf`, the one rule; what a status mark draws is
  `stores/statusMarkGroup.ts#markGroup`, which every surface with a mark
  calls), the Only mine
  and Search match (`cardMatches`), a
  card's web address (`cardURL`, the one rule), the columns a ticket's
  moves reach (`columnsReached`), the lists a carry and a write name
  (`column:<i>`, `sprint:<id>`, `backlog`: `columnList`, `sectionList`),
  and the waiting cards drawn and counted where they wait (`withWaiting`,
  `columnWith`). `stores/trackerBacklogModel.ts` is the Backlog screen's
  pure part: its sections in the plan's order, each header's numbers with
  the moved rows counted where they go (`sectionNumbers`), what Complete
  sprint counts by Jira's rule (`completionNumbers`, over
  `finishedStatuses`), the one list its virtualizer shows and where a
  carried row lands in it.
- `stores/trackerBoardCarry.svelte.ts` holds the cards waiting where they
  were dropped while their move or rank is out, a picture that never
  changes what the board holds, and makes those writes (`moveCard`,
  `rankCard`, and `placeCard` into another Backlog section) through the
  ticket's write store: one drop per card from its release, held by the
  screen that accepted it until the screen decides it or goes
  (`dropOwner`, `cardBusy`, which a pending move, rank or sprint move
  counts), and the writes of a list one at a time, each naming the
  neighbors the board holds when it is sent. A card writes under its ticket's key (`cardWriteContext`),
  so the card and the ticket screen show one write.
- `components/shared/ActionMenu.svelte` draws the card's menu
  (`BoardCardMenu.svelte`, a Backlog row's too): the actions shown at once, the rest behind
  Show more, and pages such as Move to… (`CardMovePage.svelte`), with the
  shown set kept per menu name in `stores/menuShownActions.svelte.ts`. A
  new card action is one entry in `BoardCardMenu`'s groups. Its Ask and
  Start work take the project and the grants' sentences from
  `ticketWorkGate.ts`, the rule Work uses, and load none of Work's code
  until chosen: Ask imports Work's store, and Start work opens the ticket
  with its form through the trail (`TicketTrail.askStartWork`).
- `stores/trackerBoardChoices.svelte.ts` lists the boards to pick from, one
  `TrackerBoardChoices` answer per computer with its state (reading,
  answered, offline, failed), held while a pane shows it. It starts its own
  reads: on the hold, when a computer gains tracker:read, on reconnect, on
  `trackerBoardEvents.ts#trackerBoardListsChanged` (a computer's
  connections changed, or this window saved a project's boards) and when
  the picker opens. `isBoardChoiceOpenable` is the one rule for which board
  may be opened, by the picker and by the pane on its own.
- `stores/issuesPane.svelte.ts` owns the board a pane shows and its
  subscription (`createBoardSelection`, one per pane, created by
  `IssuesPane.svelte`), the opening rule (`openingBoard`), the board
  picked last and Only mine as saved for the next pane, Display per board,
  and the compact column for the session, a Backlog section's fold per
  board, and the project and type a ticket was made with last per board
  for the session. Only a pick is saved; another window's change never
  moves an open pane. Its selection also holds the screen (Board, Backlog,
  Dashboards or Time, saved for the next pane), the open ticket, its
  subscription and the trail Back steps through, and decides whether the
  board, the dashboard and the week are shown (`boardShown`,
  `dashboardsShown`, `timeShown`: the pane is on screen on that screen
  and no ticket covers it), which are their votes; the board's
  subscription is voted shown on Dashboards too, so the copy a dashboard
  counts keeps syncing.
  The ticket's placement per screen, its folds, the comment order and
  the Time layout are remembered here too. `createTicketTrail` is the one
  owner of a pane's open ticket, its subscription, its project
  (`TicketTrail.project`: the person's pick, else the last opener's that
  named one, an opener of the ticket already shown included, else the
  Issues pane's board's; Ask and Start work use it) and Back trail, for
  the Issues pane and a ticket pane alike; `TicketHost.svelte` mounts
  what a trail shows.
- `stores/ticketPanes.svelte.ts` (in the startup graph) owns ticket panes:
  which ticket each shows, the Ctrl or Cmd rule for opening one
  (`opensTicketPane`), the move's hold on the subscription until the
  pane's body holds it, and what the layout saves. Its header says who
  owns the ticket while running, saving and restoring.
- `stores/trackerTicket.svelte.ts` holds each open ticket's subscription,
  one per ticket for every surface showing it, with its connection's
  kind and site and the person's account there as the subscription tells them
  (the ticket reads no connection list), held through a re-subscribe
  until its answer (the rules are written beside them): subscribe with the vote,
  the `tracker:issue_updated` fold, the one view read in flight, the gap
  read, Refresh and the earlier comments held by page with the token that
  read each. The rules those pages keep ("Held comment pages") are written
  at the top of the store. It follows a ticket's planning poker while one
  surface shows the ticket with its poker part on screen
  (`setPokerShown`), and holds whether each connection's site has poker.
- `stores/trackerTicketWrites.svelte.ts` makes every write and picker read
  of the ticket screen and holds each write's outcome by ticket and target:
  one pending write per target, Save anyway as the person's second press,
  and each outcome kept under a control the person sees or said in a
  toast, never both (its rules at the top; a control registers through
  `ticketWriteSurface.ts`). Each ticket's writes are one box, so a write
  wakes that ticket's readers only. A board's writes (its sprints, and the
  tickets made on it from the Backlog) are held the same way under the
  board's key (`boardWriteContext`), and none of a ticket's rules (its
  view's settling, its read after a stale refusal, loss consent) is
  reached from one. It also
  holds what is being written to each target (the drafts): a text with its
  base, or a move's form with the base of each long field, which the
  editors and the form show, so a draft outlives its editor; a pane's
  selection holds the drafts of its open ticket and Back trail
  (`holdTicketDrafts`), and a ticket pane's record holds those of its
  ticket and trail. Files are attached through it one at a time per
  ticket, each its own `attach:` write; it holds what a form reads (a
  connection's link types for the link form; a board's projects, a
  project's issue types and a type's create fields for New ticket), read
  on a person's act and read again by one once it is older than
  `FORM_READ_AGE_MS`, after the screen's Refresh, or after its computer
  connects again. New
  ticket's draft, one per section, is held by the Backlog screen
  (`BacklogScreen.svelte` `createDrafts`), so it outlives the form.
- `stores/trackerSetup.svelte.ts` holds a project's tracker setup and
  its statuses in this window, one read for every surface that holds
  them (the ticket's pins, the Start work form and defaults, Settings >
  Issues > Status groups), read again on `tracker:setup_updated` and on
  reconnect only while held; `editTrackerSetup` is the one save, one at a
  time per project, built from the held setup under its revision, and
  what it settles to is what the computer stored. `TicketPin.svelte` is
  the pin a ticket's field row and section header show.
- `stores/trackerTicketModel.ts` is the pure part: the comment and
  history windows, the rail's order and the sections' by the project's
  pins (`railFields`, `ticketSections`, `withTicketPin`), the side a new link sends, a poker
  round's result, the attachment a file in the text names, the board
  row a ticket shows its points from and how its sprints are named.
- `stores/trackerWriteOutcome.ts` is what a tracker write's answer means
  for the person (`writeOutcome`, worded per subject), the one runner
  of a write (`sendWrite`) and the loss consent rule (`LossConsent`,
  its rules written beside it), shared by the ticket's and the Time
  screen's writes; each write store holds its own consent and revises it
  on every change of a draft.
- `stores/trackerTime.svelte.ts` holds the weeks the Time screen reads,
  one per computer, connection, person and Monday, the five used last:
  the stored read and the refresh when a week shows, Refresh, the
  reconnect's refresh, the reads a write owes its days and their order,
  and the receipts of writes the week has not caught up with. Its rules
  are written at the top of the store. `stores/trackerTimeModel.ts` is
  the pure part: the days of a week, what a day's total leaves out, and
  the By ticket rows (`weekTickets`, derived only while By ticket or the
  form reads them).
- `stores/trackerTimeWrites.svelte.ts` makes the Time screen's writes
  (log, change, delete), one pending write per entry or per ticket and
  day, and holds each outcome, with a toast for one that settles after
  its form and entry are gone. It does not import the weeks: the weeks'
  store takes what a write asks of them (`takeWorklogWrites`), so a
  surface that only logs time loads no week.
- `utils/trackerAttachments.ts` is what saving an attachment does on this
  page (`fileSaveAction`: the ticket's computer writes it, a connected
  browser downloads it up to 256 MB), its bytes, and the images a ticket's
  text shows in place, held in the media cache until the ticket's last
  surface goes.
- `stores/ticketWork.svelte.ts` holds a ticket's Work read, its Ask in
  flight and its Start work run: the plan, the thread it made and each
  step's outcome, with the rules that keep a retry from doing anything
  twice written at the top. `TicketWork.svelte` is the rail's Work
  section, mounted by one line at the end of the rail, and
  `StartWorkForm.svelte` the form it opens; the threads it lists are the
  client's catalog filtered by the link's tracker kind, site and issue,
  which the ticket's subscription names, with no call. A step's row is no
  control of the write it sent: it shows that write only while it is its
  target's latest, and the Status and Assignee fields say what became of
  it.
- `stores/trackerDashboard.svelte.ts` holds the Dashboards screen's
  state, one per screen: the board's dashboards, the one answer the
  screen draws and the one evaluation in flight, the range and Team or
  Mine saved for the next screen, the person or epic narrowed to, and the
  one drill open. It counts again on the board's changes
  (`TrackerBoardState.changes`), at most once every 5 s, and every 10
  minutes; its rules are at the top of the store.
  `stores/trackerDashboardModel.ts` is the pure part: how a number reads
  and its change mark. The charts (`charts/`) draw to their measured
  width from `charts/chartScale.ts`.
- `stores/ticketOpen.ts` is the one opener of a ticket from outside the
  pane (a thread's linked ticket, a pull request's ticket chip); it is in
  the startup graph and reaches the pane's stores through one dynamic
  import.

These stores load with the pane (`ToolPane.svelte` imports it on demand)
or the first ticket pane (`panes/TicketPane.svelte` imports
`TicketPaneBody.svelte` on demand), `TicketScreen.svelte` loads on the
first open of a ticket (`TicketHost.svelte`),
`BacklogScreen.svelte` on the first switch to Backlog,
`DashboardScreen.svelte` on the first switch to Dashboards and
`TimeScreen.svelte` on the first switch to Time.
The always-loaded stores reach them only through
`stores/trackerBoardEvents.ts`, where the board and ticket stores take
`tracker:board_updated`, `tracker:issue_updated` and their gaps, the
setup store takes `tracker:setup_updated`, and the choices store hears
that a computer's board lists may have changed;
`lib/architecture.test.ts` fails when one of them enters the startup
graph.

`TicketFinder.svelte` finds a ticket by key or words on one or more
connections, and `WorklogForm.svelte` logs, changes and deletes one
entry; both serve any surface that logs time.

## Rendering

Each column is its own `LongListVirtualizer`, so the DOM holds the cards
near each column's viewport; what the store holds is each section's loaded
part. A column asks for its next page near the end of its held rows
(`BoardColumn`), never while a filter reads it whole. Rows a patch does not
touch keep their objects, so their cards do not re-render; keep that when
deriving rows (filter into new arrays, never copy rows). Marks are picked
by name or level in `boardFormat.ts`; a board fetches no tracker image.

`ticketEditors.ts` is the one rule for which ticket fields take an
editor in place and what a pick or a typed value sends; the ticket saves
each edit at once, and `TicketMoveForm.svelte` collects them for a move
whose screen takes fields, its pickers asking that move's values.
A card's marks and a Backlog row's are one component (`CardMarks.svelte`)
and open the same editors (`cardEditor`, in `CardFieldEditor.svelte`,
loaded by `CardEditor.svelte` on the first edit from either), and a
typed value is `TicketTypedValue.svelte` on both. A mouse or pen carries
a card or a row (`listCarry.ts`, loaded on the first press of either by
`listPress.ts`, the press that picks either up, one per screen; its
physics in `boardCarryPhysics.ts`, a Backlog row held level): per frame
it writes one transform on a copy on `document.body`, and tells the
screen only when the list under it or the drop slot changes, so no card
or row renders while it is carried (`listCarry.browser.test.ts`,
`backlogScreen.browser.test.ts`); a release counts only over a list as it
is on screen then. On Backlog one virtualizer holds every section's
items, and the rows under a carried one slide by a transform to open its
gap. Under compact neither carries: a card's status mark opens Move to…,
and a row's menu moves it between sprints.

A ticket's Log time opens the Time screen's `WorklogForm.svelte` with the
ticket and today, and the link form (`TicketLinkForm.svelte`) finds the
other ticket with `TicketFinder.svelte`; neither has a second copy here.
Planning poker, the link form and the Log time form load on first use
(`lib/architecture.test.ts` keeps them out of the ticket's chunk).
Every input of a ticket write (an editor, the move form, the link form,
the poker votes) is disabled while its target's write is pending.

`TicketMarkdown.svelte` is the one renderer of a ticket's text, its long
text fields and comments: ChatMarkdown with embedded HTML and
`trackerMedia`, which shows a tracker's `media:` image or attachment as a
chip naming the file (`utils/trackerMedia.ts`). On a ticket's screen
(`TRACKER_MEDIA_CONTEXT`) a name one attachment has shows that file: an
image the app previews (`previewsInApp`, the rule Open on its row uses
too) in place while its box is within 200 px of the viewport, keeping
its box and letting its bytes go once it leaves, any other file with its
size and Save.

A ticket mounts its newest five comments and 20 history changes; each
press of the reveal control mounts 20 more, so the DOM grows only with
what the person asked to see. The Issues browser specs assert the mounted
count (`e2e/tests/issues-ticket-flow.ts`).
