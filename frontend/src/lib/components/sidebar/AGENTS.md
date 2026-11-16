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
- `sidebarListsThread` in `sidebarBuckets.ts` is the one place that
  decides a thread has a row: never archived, never `placement` owned.
  The buckets put an assistant in the Assistants section and every other
  listed thread in its project.
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
- Apply computer visibility before search and group projection. A hidden clone
  must not hide a visible checkout of the same repository on another computer.
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
