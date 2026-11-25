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
  window's change never moves an open pane.

These stores load with the pane (`ToolPane.svelte` imports it on demand).
The always-loaded stores reach them only through
`stores/trackerBoardEvents.ts`, where the board store takes
`tracker:board_updated` and its gaps and the choices store hears that a
computer's board lists may have changed; `lib/architecture.test.ts` fails
when one of them enters the startup graph.

## Rendering

Each column is its own `LongListVirtualizer`, so the DOM holds the cards
near each column's viewport; what the store holds is each section's loaded
part. A column asks for its next page near the end of its held rows
(`BoardColumn`), never while a filter reads it whole. Rows a patch does not
touch keep their objects, so their cards do not re-render; keep that when
deriving rows (filter into new arrays, never copy rows). Marks are picked
by name or level in `boardFormat.ts`; no tracker image is fetched.
