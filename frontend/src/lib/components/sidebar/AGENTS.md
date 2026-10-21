# components/sidebar/

The sidebar projects assistants, threads, groups, and repositories from
their entity stores. `Sidebar.svelte` owns the top-level render boundary;
`ProjectsSection.svelte` holds the Assistants section
(`AssistantsSection.svelte`) above the Projects section in one scroll
area. `SidebarTreeList.svelte` renders the rows of one tree: the
assistants' cards, and each project's inside `ProjectThreadList.svelte`,
which adds the project's empty state and drop targets.

## Rows and actions

- A thread renders flat under its project or group, except a standing
  agent's spawned threads whose `placement` is nested, which nest under
  the agent row by spawn lineage. Keep one row grammar for normal, grouped and nested entries. Shared controls such as
  `ThreadRowPinButton` receive callbacks; they do not own entities.
- Groups are not pinnable. A group's members pin like top-level threads, but
  never as numbered jump targets.
- An assistant's card and its row in each project are `ThreadRow`'s agent
  variant, not components of their own: they use the group row's geometry,
  fold what they hold on their chevron (`sidebar:collapsedAgents`, keyed
  by `agentContainerKey`), and offer no pin or group item. The card's
  state line reads `stores/threadWakes.svelte.ts`; each row's `N done` footer
  (`AgentDoneFooter.svelte`) counts the settled idle children the view folds
  (`utils/sidebarTreeView.ts`) and reads the archived ones from
  `stores/agentArchivedChildren.svelte.ts`. Groups and agent rows share one
  member bound (`boundContainerMembers`, `ContainerShowMore.svelte`).
  Rules: [standing agents](../../../../../docs/specs/standing-agents.md#sidebar).
- `sidebarBuckets.ts` decides where each thread lists: an assistant's
  helpers and past threads on its card (`cardOwnerAgentId`), everything
  else in its own project, nested under the assistant's row there when
  its placement is nested. The tree builder runs per card list and per
  project with the assistants it is handed.
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
group, never one whose payload is `agentBound` (a standing agent's own row),
and stop propagation. An agent row and its member rows are not drop
targets. The list background accepts grouped threads for
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

Compact rows expose a visible menu button and use the shared long-press bridge.
Do not add row-specific touch handlers or enable dragging in compact mode.
