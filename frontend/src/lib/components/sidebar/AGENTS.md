# components/sidebar/

The sidebar projects threads, discussions, groups, and repositories from their
entity stores. `Sidebar.svelte` owns the top-level render boundary;
`ProjectThreadList.svelte` owns each project's visible tree.

## Rows and actions

- Keep one row grammar for normal, grouped, and discussion entries. Shared
  controls such as `ThreadRowPinButton` receive callbacks and labels; they do
  not own entities.
- Nested discussion children and grouped threads do not expose independent pin
  actions. The parent discussion or group is the pin target.
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
project and current group. During `dragover`, read the in-process drag record
because browsers do not expose `DataTransfer` payloads consistently.

Clear the record on every drop target as well as `dragend`; the source row may
unmount before `dragend`. Groups accept same-project threads not already in the
group and stop propagation. The list background accepts grouped threads for
ungrouping. Keep hover ownership in `ProjectThreadList` so child rows illuminate
their group and rejected group targets clear the background affordance.

## Persisted presentation

Sidebar expansion and sorting live in `stores/sidebar.svelte.ts`. Discussion
expansion stores opened IDs; group collapse stores explicitly closed IDs.
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

Compact rows expose a visible menu button and use the shared long-press bridge.
Do not add row-specific touch handlers or enable dragging in compact mode.
