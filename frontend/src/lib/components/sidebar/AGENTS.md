# components/sidebar/

The sidebar projects assistants, threads, groups, and repositories from
their entity stores. `Sidebar.svelte` owns the top-level render boundary;
`ProjectsSection.svelte` holds the Assistants section
(`AssistantsSection.svelte`) above the Projects section in one scroll
area. `SidebarTreeList.svelte` renders the rows of one tree: the
assistants' cards, and each project's inside `ProjectThreadList.svelte`,
which adds the project's empty state and drop targets.

## Rows and actions

- A thread renders flat under its project or group. Keep one row grammar
  for normal and grouped entries. Shared controls such as
  `ThreadRowPinButton` receive callbacks; they do not own entities.
- Groups are not pinnable. A group's members pin like top-level threads, but
  never as numbered jump targets.
- An assistant is one row in the Assistants section, `ThreadRow`'s agent
  variant: its glyph in the pin column, its state line
  (`stores/threadWakes.svelte.ts`) and projects line under the title, and
  no pin or group item. Its past threads open from Previous Threads…
  (`PreviousThreadsDialog.svelte`, `stores/assistantPastThreads.svelte.ts`).
  Rules: [standing agents](../../../../../docs/specs/standing-agents.md#sidebar).
- Each assistant follows the Assistants setting of the computer it lives
  on (`assistantsOn` in `stores/assistants.svelte.ts`): while that
  computer has it off, nothing of its assistants renders, New Assistant…
  for it is gone or disabled, and the assistant dialogs close and do not
  open. Any surface that lists threads leaves out those under an
  assistant with `hiddenAssistantThread`. Rules:
  [standing agents](../../../../../docs/specs/standing-agents.md#turning-assistants-off).
- `sidebarListsThread` in `sidebarBuckets.ts` is the one place that
  decides a thread has a row: never archived, never `placement` owned.
  The buckets put an assistant in the Assistants section and every other
  listed thread in its project.
- A computer's project headers, its project threads and its groups list
  only while `catalogTreeListed` says so; until then it shows the loading
  row alone, with no header and no New Thread button. `treeListed` in
  `ProjectsSection.svelte` is the only reader of that gate here, so what a
  held computer shows is one decision. The rule is in
  [stores/AGENTS.md](../../stores/AGENTS.md#computer-owned-and-frontend-owned-state).
- An entry is three separate things, and `listedProjects` in
  `ProjectsSection.svelte` is the one derivation that decides all of them
  with everything else a header shows or the list sorts by. Its key is the
  repository key of the computer's chosen checkout (`projectEntryKey`),
  which no listing, filter, attach or detach changes, so the each block
  never remounts a row that is still the same entry. Its representative,
  which gives the label, the action target and the manual position, is
  elected over the members whose tree is listed, filter ignored, so it can
  change only at a listing. What it counts, how it is dated and what a
  header says about its members, its identity warning and its recheck
  included, come from the members the filter also shows. Nothing reads a
  per-entry fact out of a global keyed by one member's id: the entry's live
  activity is `activityOf`, over the members it shows, because a streaming
  beat bumps a box keyed by its own project id.
- Disclosure needs none of the three. Collapse and the show-more limit stay
  persisted by project id and are read and written over an entry's members
  (`stores/sidebar.svelte.ts`), so what a user set survives every
  representative change with no key to migrate.
- A row's worker count and needs-you badge read
  `stores/threadWorkerSummaries.svelte.ts`.
- `threadRowActions.ts` and `threadGroupActions.ts` own mutations from this
  directory. Reconcile rows from each RPC response.
- Inline rename stays in the owning row. Context menus delegate through the
  row's handler and use the shared menu primitives.
- Both project New Thread entry points use `openDraftThreadForProject` and must
  reveal an already-mounted pane, including after compact Back navigation.
- Group creation clears filtering and expands the project before requesting
  rename so the new row can mount. Request rename after create-and-move sorting.

## Drag and drop

A drag carries one thread. Use `utils/threadDragPayload.ts` and include its
project and current group in the in-process drag record; the `DataTransfer`
carries the thread id alone, since any drop target, a web page in a browser
pane included, can read it. Read the record during `dragover` too, because
browsers do not expose `DataTransfer` payloads consistently.
A sidebar drag sets its own data type only, never `text/plain`: plain text
makes every text field a drop target for the row.

Clear the record on every drop target as well as `dragend`; the source row may
unmount before `dragend`. Groups accept same-project threads not already in the
group, never one whose payload is `agentBound` (a standing agent's own row),
and stop propagation. An assistant row is not a drop target. The list
background accepts grouped threads for
ungrouping. Keep hover ownership in `ProjectThreadList` so child rows illuminate
their group and rejected group targets clear the background affordance.

## Persisted presentation

Sidebar expansion and sorting live in `stores/sidebar.svelte.ts`. Group
collapse stores explicitly closed IDs.
Synchronizing one project's tree must not remove another project's state.

## Connected computers

- Capture the selected computer when opening Add Project or Thread from PR.
  Browse, validation, duplicate checks, and submission stay on that computer.
- Late dialog replies may update their captured entities but do not navigate
  after dismissal or target change.
- Directory browsing distinguishes a missing path from an RPC error. Clear a
  committable selection while typing or navigating until a successful listing
  confirms it.
- Computer filters affect sidebar visibility only. They do not change transport
  subscriptions, execution routing, or mounted panes.
- Apply computer visibility by member, before search and before an entry's
  aggregates, and never by regrouping the rows that pass. A hidden clone must
  not hide a visible checkout of the same repository on another computer, and
  must not change which entry that checkout belongs to.
- Usage and system statistics retain explicit computer identity. Do not present
  cached offline data as current totals or combine separately priced account
  reports as one host-local value.

`ToolsRow.svelte` lists `panes/toolPaneMeta.ts` in order and opens through
`toolPanes.svelte#openTool`; the Workflows count comes from its own store.
Each tool is a `role="button"` element styled by
`primitives/buttonClasses.ts`, never a native `<button>`, because Firefox
starts no drag from one. It drags its kind under `TOOL_PANE_DRAG_MIME` with
the same in-flight record pattern, so no thread drop target reads it.

Compact rows expose a visible menu button and use the shared long-press bridge.
Do not add row-specific touch handlers or enable dragging in compact mode.
