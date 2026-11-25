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
  (`BoardView.screen`); the store asks for no sections and holds what it
  answers.
- `stores/trackerBoardModel.ts` is the pure part: the patch rule (replayed
  in Vitest from `internal/trackerapp/testdata/board_patch_cases.json`, which
  the Go model writes), the grouping of sprint rows into columns
  (`screenColumns`) and the Only mine and Search match (`cardMatches`).
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
  and the compact column for the session. Only a pick is saved; another
  window's change never moves an open pane. Its selection also holds the
  open ticket, its subscription and the trail Back steps through, and
  decides whether the board is shown (`boardShown`: the pane is on screen
  and no ticket covers it), which is the board's vote. The ticket's
  placement, its folds and the comment order are remembered here too.
- `stores/trackerTicket.svelte.ts` holds each open ticket's subscription,
  one per ticket for every surface showing it: subscribe with the vote,
  the `tracker:issue_updated` fold, the one view read in flight, the gap
  read, Refresh and the earlier comments held by page with the token that
  read each, which an applied edit of one of their comments reads again
  and an applied delete takes the comment out of.
- `stores/trackerTicketWrites.svelte.ts` makes every write and picker read
  of the ticket screen and holds each write's outcome by ticket and target:
  one pending write per target, Save anyway as the person's second press,
  and a toast for a write that settles after its ticket closed.
- `stores/trackerTicketModel.ts` is the pure part: what a write's answer
  means for the person (`writeOutcome`), the comment and history windows,
  the rail's order and the board row a ticket shows its points from.

These stores load with the pane (`ToolPane.svelte` imports it on demand),
and `TicketScreen.svelte` loads on the first open of a ticket.
The always-loaded stores reach them only through
`stores/trackerBoardEvents.ts`, where the board and ticket stores take
`tracker:board_updated`, `tracker:issue_updated` and their gaps and the
choices store hears that a computer's board lists may have changed;
`lib/architecture.test.ts` fails when one of them enters the startup
graph.

## Rendering

Each column is its own `LongListVirtualizer`, so the DOM holds the cards
near each column's viewport; what the store holds is each section's loaded
part. A column asks for its next page near the end of its held rows
(`BoardColumn`), never while a filter reads it whole. Rows a patch does not
touch keep their objects, so their cards do not re-render; keep that when
deriving rows (filter into new arrays, never copy rows). Marks are picked
by name or level in `boardFormat.ts`; no tracker image is fetched.

`ticketEditors.ts` is the one rule for which ticket fields take an
editor in place and what a pick or a typed value sends; the ticket saves
each edit at once, and `TicketMoveForm.svelte` collects them for a move
whose screen takes fields, its pickers asking that move's values.

`TicketMarkdown.svelte` is the one renderer of a ticket's text, its long
text fields and comments: ChatMarkdown with embedded HTML and
`trackerMedia`, which shows a tracker's `media:` image or attachment as a
chip naming the file (`utils/trackerMedia.ts`).

A ticket mounts its newest five comments and 20 history changes; each
press of the reveal control mounts 20 more, so the DOM grows only with
what the person asked to see. The Issues browser specs assert the mounted
count (`e2e/tests/issues-ticket-flow.ts`).
