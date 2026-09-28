# Sidebar thread groups

Status: design signed off 2026-09-02, implemented on `main` the same day
(store v76, bindings, sidebar UI, e2e `sidebar-thread-groups.spec.ts`).
Revised 2026-09-28: groups are their own section and not pinnable;
members pin individually (store v139).

## Goal

Let the user gather threads of one project under a named, collapsible
row in the sidebar. Today the only nesting is the discussion tree, whose
top row is itself a thread. A group's top row is not a thread: it has a
name, a chevron, and nothing else of its own; its activity and sort
position come from its members. It shows no status of its own: a
running member moves the group up its section, it does not light the
row.

## Decisions

- **Groups are their own section, above the pin blocks.** A project's
  groups render together above its front- and back-burner blocks. Inside
  the section a group sorts by its members' bubbled status and latest
  activity exactly the way a discussion parent does, so a group with a
  running member rises and a quiet one sinks. A group is not pinnable.
- **Section dividers.** The top level has four sections: groups, front
  burner, back burner, unpinned. The same thin divider separates each
  pair of adjacent sections that have rows. Drafts sit above every
  section and open none. No dividers render inside a group.
- **Members pin individually.** A thread inside a group pins to the
  front or back burner with the normal thread pin behavior. A thread
  starts unpinned in a group it joins, from the top level or from
  another group. A thread that leaves a group (ungroup, group delete)
  loses its pin; leaving does not restore a pin it held before it
  joined. Auto-pin on first send never fires for a thread that is
  already in a group. A member's pin is never a numbered jump target;
  jumps count top-level front-burner threads only. A member's discussion
  children pin through it, as at the top level.
- **Collapsed group.** Shows a member count. Its members' status still
  bubbles for the SORT (the same bubbling a discussion parent uses) but
  nothing of it renders on the row (ruling 2026-09-02). A group
  remains collapsed when a member becomes focused. Only the focused
  thread appears directly beneath it, even if that thread is a nested
  discussion child. Other open panes do not add rows to this preview.
  Collapsed discussions follow the same rule. Focus changes replace or
  remove the preview without changing saved expansion state. Expanding
  restores the full tree without duplicating the focused row. Groups
  start expanded.
- **Preview cut.** Groups and front- and back-burner pins share the
  project's preview limit with unpinned rows. Groups and pins always
  remain visible when they exceed the limit, leaving no preview slots
  for unpinned rows. Drafts and threads open in panes retain their
  visibility exceptions. A group takes one slot; its members take none
  (same as discussion children).
- **No nesting.** No groups inside groups. A discussion tree moves as a
  unit: grouping a parent brings its children; a child cannot be grouped
  on its own. Render depth inside a group goes to three (group, parent,
  child).
- **One project.** A group belongs to one project and cannot span
  projects. Dropping a thread on a group in another project is a no-op.
- **Ordering inside a group is the normal comparator** (drafts, front
  burner, back burner, status tier, activity, id). No manual ordering.
  A pinned member orders inside its group; for the group's bubbled
  status and sort, members rank by status alone, so a pinned idle member
  never masks a blocked one.
- **Drag and drop.** Dropping a thread on a group row, or on any row
  inside an expanded group, moves it in. Dropping a grouped thread
  anywhere in its own project's list outside a group (a top-level thread
  row, the list background, the show-more footer) ungroups it. Dropping
  on a pane keeps its current meaning (open there) and leaves membership
  alone. A drag carries one thread; multi-thread moves go through the
  context menu.
- **Context menus.** Thread row: "Move to Group" submenu listing the
  project's groups plus "New Group…"; a grouped row also gets "Remove
  from Group". Multi-select gets the same two when every selected thread
  shares one project. Project header: "New Group…", also a hover-revealed
  folder-plus button beside New Thread. Group row: New Thread, Rename
  Group, Archive Threads, Ungroup All, Delete Group. A grouped thread
  row keeps the normal pin affordance and pin menu items.
- **New thread.** The group row's plus button and New Thread menu item open
  the normal draft composer with that group's project and computer. Membership
  persists when the draft materializes and when an emptied draft returns to a
  placeholder. Switching projects clears membership. Deleting the group ungroups
  open placeholders as well as saved threads.
- **Lifecycle.** A new group is named "New Group" and opens inline
  rename immediately. An empty group persists until deleted. Deleting a
  group ungroups its members, clears their pins, and never deletes a
  thread; it honors the `confirmDelete` setting. Archiving a member hides it; the group stays
  and the membership survives unarchive. Deleting a project deletes its
  groups. A fork of a grouped thread lands in the same group, unpinned:
  the source's pin is its own.
- **Search.** A group shows when its name matches (all members shown)
  or when any member matches (only matching members shown). A group with
  no match is hidden.
- **Vocabulary.** "Group" is this feature. The two pin tiers keep their
  UI names (front burner, back burner); their column stays `pin_group`.
  `docs/GLOSSARY.md` carries both entries.

## Non-goals

- Groups inside groups.
- Manual ordering inside a group.
- Multi-thread drag (a drag carries one thread).
- Cross-project groups.
- Group-level actions beyond the list above (no bulk delete of member
  threads, no group colors or icons).

## Design

### Store

- Migration v76 created `thread_groups` and `threads.group_id`
  (`REFERENCES thread_groups(id) ON DELETE SET NULL`, partial index
  `idx_threads_group`). Migration v139 removed the v76 rule that a grouped
  thread holds no pin: it rebuilds `threads` without
  `CHECK(group_id IS NULL OR pinned_at IS NULL)` and drops
  `thread_groups.pin_group` and `thread_groups.pinned_at`. The current
  table:

  ```sql
  CREATE TABLE thread_groups (
      id         TEXT PRIMARY KEY,
      project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
      name       TEXT NOT NULL,
      created_at INTEGER NOT NULL,
      updated_at INTEGER NOT NULL
  );
  ```

  A name is unique per project without case (v105). `ON DELETE SET NULL`
  is "delete group = ungroup", including archived members.
- `store.ThreadGroup{ID, ProjectID, Name, CreatedAt, UpdatedAt}`,
  JSON-tagged like `Thread`. `store.Thread.GroupID string
  \`json:"groupId,omitempty"\`` carries membership.
- `internal/store/thread_groups.go`: `ListThreadGroups`,
  `CreateThreadGroup(projectID, name)`, `RenameThreadGroup`,
  `DeleteThreadGroup`, and `SetThreadGroup(threadIDs []string, groupID
  string)`. Members pin through the thread pin primitives (`PinThread`,
  `UnpinThread`, `SetThreadPinGroup`), which do not look at `group_id`.
- `SetThreadGroup` changes membership on existing threads. One
  transaction; for each id it runs

  ```sql
  UPDATE threads
     SET group_id = ?,
         pinned_at = CASE WHEN group_id IS ? THEN pinned_at END,
         pin_group = CASE WHEN group_id IS ? THEN pin_group END
   WHERE ((id = ? AND COALESCE(parent_thread_id, '') = '') OR parent_thread_id = ?)
     AND project_id = (SELECT project_id FROM thread_groups WHERE id = ?)
   RETURNING id
  ```

  The pin clears only on a row whose group changes: a row already in the
  destination keeps its pin. The root id must be among the returned rows
  or the whole call rolls back with a typed refusal: `ErrThreadGroupGone`
  (deleted or cross-project group), `ErrThreadGone` (deleted thread), or
  `ErrThreadNotRoot` (a discussion child named alone; a child named
  beside its own root is carried by the root's disjunct). Empty
  `groupID` is ungroup: the same WHERE with `SET group_id = NULL` and the
  pin cleared only where `group_id` was set, because a bulk selection can
  name never-grouped rows too and their pins are theirs to keep. The
  touched rows are read back inside the transaction and returned,
  children included.
- `DeleteThreadGroup` clears the members' pins, active and archived, in
  the transaction that deletes the group, and returns the member rows as
  they now stand.
- `ApplyThreadOrganize` (agent `thread_update`) runs the group move
  before the pin, so a patch that sets both lands the pin inside the new
  group.
- A returning conversation transfer arrives ungrouped; the local row's
  pin survives only if the row was not in a group.
- Fork copies `group_id` from the source row (all fork paths), never the
  pin.

### Backend API

`internal/threadapp/groups.go` wraps the store; `internal/app/app_thread_group_bindings.go`
binds:

| Method | Returns |
|---|---|
| `ListThreadGroups()` | `[]store.ThreadGroup` |
| `CreateThreadGroup(projectID, name)` | `store.ThreadGroup` |
| `RenameThreadGroup(id, name)` | `store.ThreadGroup` |
| `DeleteThreadGroup(id)` | error |
| `SetThreadGroup(threadIDs, groupID)` | `[]store.Thread` (every row the call touched, children included) |

Every one of these is a wire RPC, so each carries a `//ao:scope` and a
route (`internal/transport/AGENTS.md`). `ListThreadGroups` is
`threads:read` and `//ao:route all`, because a group belongs to one
backend's project and the unified sidebar merges what every attached
backend answers. The rest are `threads:operate`: `CreateThreadGroup`
takes the inferred `project` route off its `projectID` parameter, and the
others declare `//ao:route home`, because a GROUP id is neither a thread
nor a project id and nothing infers a route from one. That includes
`SetThreadGroup`, whose first parameter is a slice of thread ids.
`methods_gen.go` is regenerated with them and its in-sync test passes.
Names are trimmed and non-empty; a blank rename is rejected.

### Events

- New channel `thread-group:updated`, payload
  `{action: "create" | "patch" | "delete", group: ThreadGroup}` (`delete`
  carries the id in `group.id`). Every group RPC emits it after the
  write so a second client stays current. `DeleteThreadGroup` first
  emits `thread:updated` `full` for every former member (ungrouped and
  unpinned), then the `delete` frame.
- `SetThreadGroup` emits `thread:updated` with `action: "full"` and
  the full row for every touched thread. `PinThread`, `UnpinThread`, and
  `SetThreadPinGroup` gain the same emit; that closes the pre-existing
  gap where a second client showed stale pin state.

### Frontend

- `types/models.ts`: `ThreadGroup`; `Thread.groupId?: string`.
- `stores/threadGroups.svelte.ts`: `getThreadGroups()`,
  `getThreadGroupsForProject(projectId)`, load at boot beside
  `ListThreads`, and the `thread-group:updated` handler registered in
  `stores/events.ts`. Group RPC helpers live in
  `components/sidebar/threadGroupActions.ts`, shaped like
  `threadRowActions.ts`, and reconcile the store from each RPC response.
- `stores/sidebar.svelte.ts`: `sidebar:collapsedGroups` set (groups
  default expanded, so the persisted set holds the collapsed ids).
- `utils/sidebarTree.ts` owns the tree, sorting and status rollup.
  `utils/sidebarTreeView.ts` projects visible rows, applies the preview
  limit and prunes expansion IDs for discussion rows that became leaves.
  Collapsed containers project only their focused descendant at one level
  below the container, with that thread's own status and no nested expansion
  controls. The preview retains the owning group for drag-and-drop actions.
  Groups and discussions containing open threads remain above the preview
  cut. Rendering does not write group collapse state.
- Search: `ProjectsSection` buckets threads and groups in ONE derivation,
  because the two filters are coupled — a thread survives when it matches
  or when its group's NAME matched, so a name match pulls the whole
  membership back into the thread bucket the builder already reads. The
  builder therefore takes only `groups`; there is no second "unfiltered
  members" input. A group that neither matches by name nor keeps a member
  is dropped from its project's group list, and a project with a
  surviving group is a visible project.
- `components/sidebar/ThreadGroupRow.svelte`: same 24px row grammar as
  `ThreadRow` (chevron, title; no pin gutter, timestamp, status dot or
  label), folder glyph before the name. The chevron centres on the
  top-level pin column, the member rail drops from its centre, members'
  pins centre under the folder glyph and their titles align with the
  group name (`utils/sidebarRowMetrics.ts` derives each offset), member count when
  collapsed, inline rename on double-click / F2, its own context menu
  (`ThreadGroupContextMenu.svelte`). Members render through `ThreadRow`
  at `indent = depth + 1` and reserve the pin gutter inside the group
  rail. Shared row dimensions live in `utils/sidebarRowMetrics.ts`.
  `ThreadRowPinButton` takes its writes as `onToggle` / `onCycleBurner`
  closures; its hover text says that right-click on a pin toggles front
  and back burner. `flattenSidebarThreadTree` marks pin targets
  (`isPinTarget`: top-level threads and direct members) and section
  starts (`startsSection`). A blank inline rename CANCELS rather than round-trips
  to be rejected. A brand-new group opens rename on mount through
  `requestGroupRename` / `consumePendingGroupRename` in
  `stores/threadGroups.svelte.ts` — the creator cannot open it, because
  the row does not exist when the RPC returns.
- Drag payload gains `projectId` and `groupId` (the thread's current
  group, if any). Because `DataTransfer.getData` is empty during
  `dragover` in every real browser and the group targets have to decide
  THERE, the source row also records the in-flight payload in-process
  (`beginThreadRowDrag` / `endThreadRowDrag`) and targets read it through
  `threadDragPayloadForEvent`, which prefers the DataTransfer and falls
  back to the record; `effectAllowed` widens from `copy` to `copyMove` so
  a group target's `dropEffect = 'move'` is honored (the pane drop still
  answers `copy`). The hover state lives in `ProjectThreadList`
  (`dropTargetGroupId`), not on the row, so a member row lights its GROUP;
  while a group is lit the container's dashed ungroup outline stays down.
  `ThreadGroupRow` and member rows accept a thread drop
  from the same project (`dropEffect = 'move'`, row highlight while
  hovered). `ProjectThreadList`'s container handles a drop that reached
  it from no group target: if the payload has a `groupId` and the
  project matches, ungroup. While a grouped thread is dragged over that
  container outside any group, the container shows a subtle inset
  dashed outline so the ungroup target is visible. Drops from another
  project set `dropEffect = 'none'`.
- `ThreadRow` keeps the pin affordance and pin menu items on grouped
  rows; `ThreadGroupRow` has none. `ThreadContextMenu` adds the "Move to Group"
  submenu (`MenuSubmenuItem`) and "Remove from Group";
  `ProjectContextMenu` adds "New Group…" and `ProjectItem` a folder-plus
  header button; both run `newThreadGroupInProject` (clear the search,
  expand the project, create). Multi-select menu shows the
  group items only when all selected threads share one project.
- `autoPinNewThread` is a no-op for a thread with a `groupId`, so a fork
  inside a group (which inherits the group) starts unpinned; the
  first-send pre-check inherits the same guard. No backend path
  auto-pins.

## Verification

- Store tests: cross-project `SetThreadGroup` is refused and rolls
  back with typed errors (group gone, thread gone, child named as root);
  joining a group, or moving between groups, strips the pin, a repeat
  move into the same group keeps it, and ungrouping clears the pins of
  rows that left a group but keeps those of never-grouped rows in the
  same selection; a grouped thread pins, changes burner and unpins;
  `ApplyThreadOrganize` with group and pin lands the pin inside the
  group; migration v139 applies over a populated store and leaves every
  schema object but the removed CHECK and group pin columns unchanged;
  `DeleteThreadGroup` nulls `group_id` and clears pins on active and
  archived members; project delete cascades; children follow the root;
  fork copies `group_id` and not the pin; a returning transfer of a
  grouped, pinned thread arrives ungrouped and unpinned.
- `sidebarTree` / `sidebarTreeView` tests: groups sort above pins and
  among themselves by bubbled status and activity; members order by pin
  block and a pinned idle member does not mask a blocked one; section
  dividers for every combination of sections, none inside a group; pin
  targets, including a collapsed group's preview row; groups stay above
  the preview cut; a group takes one preview slot and its
  members none; collapsed and expanded flatten shapes; search by group
  name pulls all members; focused descendants remain visible under
  collapsed groups and discussions;
  `sameSidebarVisibleNodes` distinguishes a group from a thread node at
  the same index.
- Component tests: group row renders count when collapsed and members
  when expanded; context menus show the right items for top-level,
  grouped, child, and multi-select rows; a thread drop on a group calls
  `SetThreadGroup` with that id; an ungroup drop calls it with `""`; a
  cross-project drop calls nothing.
- Transport: `methods_gen_test` classification passes for the new
  methods.
- Live: create a group from the project menu, rename inline, drag
  threads in and out, pin a member to both burners, collapse with a
  focused member and see only that member beneath the group, search by group
  name, delete the group and see members return to the list, second connected
  client follows every change.
