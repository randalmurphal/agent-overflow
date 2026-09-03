# Sidebar thread groups

Status: design signed off 2026-09-02, implemented on `main` the same day
(store v76, bindings, sidebar UI, e2e `sidebar-thread-groups.spec.ts`).

## Goal

Let the user gather threads of one project under a named, collapsible
row in the sidebar. Today the only nesting is the discussion tree, whose
top row is itself a thread. A group's top row is not a thread: it has a
name, a chevron, a pin, and nothing else of its own; its activity and
sort position come from its members. It shows no status of its own: a
running member moves the group up the list, it does not light the row.

## Decisions

- **A group is a row in the normal sort, not a third pin tier.** It
  sorts by its members' bubbled status and latest activity exactly the
  way a discussion parent does, so a group with a running member rises
  and a quiet one sinks. The group itself is pinnable to the front or
  back burner like a thread; a pinned group sits in the pin block and
  never consumes a preview slot.
- **One pin per visible row.** Moving a pinned thread into a group
  strips its pin; the group carries the pin from then on. A grouped
  thread cannot be pinned (the schema refuses it, the row hides the pin
  affordance). Leaving a group does not restore a pin. Auto-pin on first
  send never fires for a thread that is already in a group.
- **Collapsed group.** Shows a member count. Its members' status still
  bubbles for the SORT (the same bubbling a discussion parent uses) but
  nothing of it renders on the row (ruling 2026-09-02). A group
  auto-expands when a member becomes the focused thread, the way a
  discussion auto-expands for its participant. Groups start expanded.
- **Preview cut.** A group takes one slot; its members take none (same
  as discussion children). An unpinned group can fall below "Show more"
  like any row.
- **No nesting.** No groups inside groups. A discussion tree moves as a
  unit: grouping a parent brings its children; a child cannot be grouped
  on its own. Render depth inside a group goes to three (group, parent,
  child).
- **One project.** A group belongs to one project and cannot span
  projects. Dropping a thread on a group in another project is a no-op.
- **Ordering inside a group is the normal comparator** (drafts, status
  tier, activity, id). No manual ordering.
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
  folder-plus button beside New Terminal / New Thread. Group row: Rename
  Group, Pin / Unpin / burner move, Archive Threads, Ungroup All, Delete
  Group.
- **Lifecycle.** A new group is named "New Group" and opens inline
  rename immediately. An empty group persists until deleted. Deleting a
  group ungroups its members and never deletes a thread; it honors the
  `confirmDelete` setting. Archiving a member hides it; the group stays
  and the membership survives unarchive. Deleting a project deletes its
  groups. A fork of a grouped thread lands in the same group.
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
- "New thread in group" from the group row. A thread joins a group after
  it exists; the draft-creation path is untouched.
- Group-level actions beyond the list above (no bulk delete of member
  threads, no group colors or icons).

## Design

### Store

- Migration v76 `thread_groups`:

  ```sql
  CREATE TABLE thread_groups (
      id         TEXT PRIMARY KEY,
      project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
      name       TEXT NOT NULL,
      pinned_at  INTEGER,
      pin_group  INTEGER
          CHECK(pin_group IS NULL OR (pinned_at IS NOT NULL AND pin_group IN (0, 1))),
      created_at INTEGER NOT NULL,
      updated_at INTEGER NOT NULL
  );
  CREATE INDEX idx_thread_groups_project ON thread_groups(project_id);
  ALTER TABLE threads ADD COLUMN group_id TEXT
      REFERENCES thread_groups(id) ON DELETE SET NULL
      CHECK(group_id IS NULL OR pinned_at IS NULL);
  CREATE INDEX idx_threads_group ON threads(group_id) WHERE group_id IS NOT NULL;
  ```

  The CHECK is the "one pin per visible row" rule: a grouped thread
  cannot hold a pin, whatever a future caller does. `PinThread` states
  the same rule in its WHERE and reports the miss as a typed
  `ErrThreadGrouped` ("a grouped thread cannot be pinned"), so the CHECK
  is the backstop rather than the message. `ON DELETE SET NULL` is
  "delete group = ungroup", including archived members. The index is
  partial because `group_id` is NULL on nearly every row.
- `store.ThreadGroup{ID, ProjectID, Name, PinnedAt *int64, PinGroup *int, CreatedAt, UpdatedAt}`,
  JSON-tagged like `Thread`. `store.Thread` gains `GroupID string
  \`json:"groupId,omitempty"\``; every positional scan and INSERT is
  updated.
- `internal/store/thread_groups.go`: `ListThreadGroups`,
  `CreateThreadGroup(projectID, name)`, `RenameThreadGroup`,
  `DeleteThreadGroup`, `PinThreadGroup`, `UnpinThreadGroup`,
  `SetThreadGroupPinGroup` (the last three mirror the thread pin
  primitives, including "pin never touches updated_at"), and
  `SetThreadGroup(threadIDs []string, groupID string)`.
- `SetThreadGroup` is the one writer of `threads.group_id`. One
  transaction; for each id it runs

  ```sql
  UPDATE threads
     SET group_id = ?, pinned_at = NULL, pin_group = NULL
   WHERE ((id = ? AND COALESCE(parent_thread_id, '') = '') OR parent_thread_id = ?)
     AND project_id = (SELECT project_id FROM thread_groups WHERE id = ?)
   RETURNING id
  ```

  and the root id must be among the returned rows or the whole call
  rolls back with a typed refusal: `ErrThreadGroupGone` (deleted or
  cross-project group), `ErrThreadGone` (deleted thread), or
  `ErrThreadNotRoot` (a discussion child named alone; a child named
  beside its own root is carried by the root's disjunct). Empty
  `groupID` is ungroup: the same WHERE with `SET group_id = NULL` only,
  because a bulk selection can name ungrouped rows too and their pins are
  theirs to keep. The touched rows are read back inside the transaction
  and returned, children included.
- Fork copies `group_id` from the source row (all fork paths).

### Backend API

`internal/threadapp/groups.go` wraps the store; `internal/app/app_thread_group_bindings.go`
binds:

| Method | Returns |
|---|---|
| `ListThreadGroups()` | `[]store.ThreadGroup` |
| `CreateThreadGroup(projectID, name)` | `store.ThreadGroup` |
| `RenameThreadGroup(id, name)` | `store.ThreadGroup` |
| `DeleteThreadGroup(id)` | error |
| `PinThreadGroup(id)` / `UnpinThreadGroup(id)` / `SetThreadGroupPinGroup(id, group)` | `store.ThreadGroup` |
| `SetThreadGroup(threadIDs, groupID)` | `[]store.Thread` (every row the call touched, children included) |

None touch local FS, processes, or settings, so none are
`LocalOnlyMethods`; `methods_gen.go` is regenerated and the
classification test passes. Names are trimmed and non-empty; a blank
rename is rejected.

### Events

- New channel `thread-group:updated`, payload
  `{action: "create" | "patch" | "delete", group: ThreadGroup}` (`delete`
  carries the id in `group.id`). Every group RPC emits it after the
  write so a second client stays current.
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
- `utils/sidebarTree.ts` + `utils/sidebarTreeView.ts` (AS BUILT: split in
  two, against the design's "one file" call. The group work pushed the
  module past 800 lines, and the seam turned out to be clean — the VIEW
  half needs `statusPriority` and `isDraftNode` and nothing else, both of
  which are now exported with a one-line doc. `sidebarTree.ts` keeps the
  node types, the sort vocabulary, `resolveDisplay`, `compareTreeNodes`
  and `buildSidebarThreadTree`; `sidebarTreeView.ts` takes
  `flattenSidebarThreadTree`, `previewSidebarThreads`,
  `nextSidebarThreadRevealLimit`, `rollupDisplayStatus`,
  `sameSidebarVisibleNodes`, `sameThreadStatusPill`,
  `toggleSidebarTreeThreadExpansion` and
  `syncExpandedTreeForActiveThread`. The view imports the builder, never
  the reverse; no re-export shim; the test file split the same way):
  `SidebarTreeNode` becomes a discriminated union,
  `{kind: 'thread', thread, …}` and `{kind: 'group', group, …}`, sharing
  the status, activity, sort, depth, and children fields. The
  builder takes `groups` beside `threads`; a top-level thread whose
  `groupId` names a group in the input becomes that group's child. A
  group node has no own status (`ownLiveStatus: 'idle'`, `ownStatus:
  null`), bubbles display status from members by the existing
  `resolveDisplay`, and takes `latestActivityAt` as the max of members
  (its own `updatedAt` when empty). `sidebarPinGroup` reads pin fields
  from either shape. `previewSidebarThreads`, `flattenSidebarThreadTree`,
  `rollupDisplayStatus`, `syncExpandedTreeForActiveThread`, and
  `sameSidebarVisibleNodes` handle both kinds; the active-thread expand
  sync removes the containing group from the collapsed set.
- Search: `ProjectsSection` buckets threads and groups in ONE derivation,
  because the two filters are coupled — a thread survives when it matches
  or when its group's NAME matched, so a name match pulls the whole
  membership back into the thread bucket the builder already reads. The
  builder therefore takes only `groups`; there is no second "unfiltered
  members" input. A group that neither matches by name nor keeps a member
  is dropped from its project's group list, and a project with a
  surviving group is a visible project.
- `components/sidebar/ThreadGroupRow.svelte`: same 24px row grammar as
  `ThreadRow` (pin gutter, chevron, title, relative time; no status dot
  or label), folder glyph before the name, member count when collapsed,
  inline rename on double-click / F2, its own context menu
  (`ThreadGroupContextMenu.svelte`). Members render through `ThreadRow`
  at `indent = depth + 1`. The shared numbers and the relative-time
  helper move to `utils/sidebarRowMetrics.ts` so the two rows cannot
  drift, and `ThreadRowPinButton` is generalised to serve both: its
  writes arrive as `onToggle` / `onCycleBurner` closures and its aria
  labels as `pinLabel` / `unpinLabel`, replacing the thread-only
  `buildCtx` prop. A blank inline rename CANCELS rather than round-trips
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
- `ThreadRow` hides the pin affordance and the pin menu items when
  `thread.groupId` is set; `ThreadContextMenu` adds the "Move to Group"
  submenu (`MenuSubmenuItem`) and "Remove from Group";
  `ProjectContextMenu` adds "New Group…" and `ProjectItem` a folder-plus
  header button; both run `newThreadGroupInProject` (clear the search,
  expand the project, create). Multi-select menu shows the
  group items only when all selected threads share one project.
- `autoPinNewThread` is a no-op for a thread with a `groupId`, so a fork
  inside a group (which inherits the group) starts without a failed-pin
  toast; the first-send pre-check inherits the same guard.

## Verification

- Store tests: cross-project `SetThreadGroup` is refused and rolls
  back with typed errors (group gone, thread gone, child named as root);
  grouping strips the pin and ungrouping keeps the pins of ungrouped rows
  in the same selection; `PinThread` on a grouped row is `ErrThreadGrouped`
  and the raw CHECK still refuses a grouped+pinned write; migration v76
  applies over a populated, pinned `threads` table;
  `DeleteThreadGroup` nulls `group_id` on active and archived
  members; project delete cascades; children follow the root; fork
  copies `group_id`.
- `sidebarTree` / `sidebarTreeView` tests: group sorts by bubbled status and activity; a
  pinned group sits in its block; a group takes one preview slot and its
  members none; collapsed and expanded flatten shapes; search by group
  name pulls all members; active member un-collapses its group;
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
  threads in and out, pin the group to both burners, collapse with a
  running member and see the dot, search by group name, delete the group
  and see members return to the list, second connected client follows
  every change.
