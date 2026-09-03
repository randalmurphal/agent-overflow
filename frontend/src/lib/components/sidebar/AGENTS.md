# components/sidebar/

Projects, threads and thread groups in the left rail. The directory owns
rows and menus; the tree they render is computed in `utils/`, and every
mutation goes through the two action modules below rather than a binding
call inside a component.

## The tree is two files

- `utils/sidebarTree.ts` — the BUILD half: the `SidebarTreeNode`
  discriminated union (`kind: 'thread' | 'group'`), the pin/sort/status
  vocabulary, status bubbling and `buildSidebarThreadTree`.
- `utils/sidebarTreeView.ts` — the VIEW half: `flattenSidebarThreadTree`,
  the preview cut and its reveal step (`previewSidebarThreads`,
  `nextSidebarThreadRevealLimit`), `rollupDisplayStatus`, the identity
  cutoffs (`sameSidebarVisibleNodes`, `sameThreadStatusPill`) and
  `syncExpandedTreeForActiveThread`.

The dependency runs one way: the view imports the builder, never the
reverse, and there is no re-export shim — import from the half that owns
what you need. Suites match the files (`sidebarTree.test.ts`,
`sidebarTreeView.test.ts`).

`sameSidebarVisibleNodes` is load-bearing for streaming performance:
`ProjectThreadList` returns its PREVIOUS array when it holds, which is
what stops the animated each-block re-running its FLIP measure pass on
every activity beat. `latestActivityAt` is deliberately not compared —
which is why NO row may take its time stamp as a prop off the node. Both
`ThreadRow` and `ThreadGroupRow` read live activity from the threads
store instead (a group folds the max over its members), so the label
keeps moving while the node array holds still.

The flatten also stamps each visible node with `ownerGroupId`, the group
it renders INSIDE (null at the top level, including a group's own row).
That, not a row's `groupId` field, is the member drop target's identity:
`groupId` is backend data nobody has checked against what is on screen.

## Row grammar

`ThreadRow` and `ThreadGroupRow` are siblings, not two widgets. Both are
24px (`h-6`), both open with the reserved pin gutter (except a row inside
a group, below), both take their numbers and their relative-time label
from `utils/sidebarRowMetrics.ts` (`PIN_SLOT_PX`, `INDENT_PX`,
`sidebarRowPaddingLeftPx`, `sidebarTimeLabel`). Never restate those
constants at a call site: a group row and the member rows under it have
to line up to the pixel.

Left to right: pin gutter (absolute, inside the padding, so it costs the
flex row no gap) · chevron · glyph · status dot (thread rows only; a group
row shows no status, its members' status only moves it in the sort) ·
title · right slot. The sidebar shows NO status text (ruling 2026-09-02):
`utils/threadStatusPill.ts` owns the visual grammar, `label` is the dot's
accessible name, and Completed / Plan Ready ring the row shell
(`ringClass`), a ring the keyboard cursor's own ring overrides.
`sidebarTimeLabel` carries no clock dependency of its own, so a row using
it reads `getMinuteNow()` in the same `$derived`.

A member row renders behind a group rail: `ProjectThreadList` gives the
member wrapper `ml-8 border-l` (the line drops from the centre of the
group row's chevron) and a 1px pad-and-pull-back so the per-row segments
join across the list's `gap-px`. Rows inside a group pass `inGroup` and
reserve NO pin gutter: nothing pins there, and the empty 24px pushed the
Completed ring and the dot away from the rail.

`ThreadRowPinButton` serves both rows. It owns the affordance and never
the entity: the WRITES arrive as `onToggle` / `onCycleBurner` closures
and the aria labels as `pinLabel` / `unpinLabel`, so the thread row hands
it `threadRowActions` and the group row `threadGroupActions`.

One pin per visible row. `ThreadRow` hides the affordance for a nested
discussion child (the parent is the pin target for that subtree) and for
a GROUPED thread (the group carries the pin, and the schema refuses one
on the row); `ThreadContextMenu` drops the pin items on the same rule.

## Actions and menus

`threadRowActions.ts` and `threadGroupActions.ts` are the only callers of
the thread / group RPCs from this directory. Both reconcile the store
from the RPC's OWN response, because the backend trims names and strips a
pin on grouping — the returned row is the only truthful copy.

Menus compose `primitives/` (`Popover` + `Menu` + `MenuItem` /
`MenuSubmenuItem` / `MenuDivider`) and delegate inline rename back to the
owning row through an `onRename` prop, so the `<input>` renders in place
of the label. Every row menu passes `Popover`'s `dismissOnAnchorClick`:
the anchor is the ROW, not a trigger, and without it a left click on the
row (its chevron, a header button) was swallowed by the anchor exemption
and the menu sat there. The project menu anchors to the header LINE
(`headerEl`), never the project container, whose bottom edge is the last
thread's.

The project header creates a group two ways, its "New Group…" menu item
and its folder-plus button, and both call `newThreadGroupInProject`: it
clears the sidebar search and expands the project BEFORE the create,
because a row the filter hides or a collapsed project never mounts is a
row that never opens its rename.

A freshly created group is named "New Group" and opens rename on mount.
The row does not exist when the RPC returns, so the id is left in
`stores/threadGroups.svelte.ts` (`requestGroupRename`) and
`ThreadGroupRow` takes it in a mount effect (`consumePendingGroupRename`).
One id, cleared on consume.

The door is `$state`, so a request that lands after the row mounted still
opens the editor, and only `threadGroupActions` asks. Create-and-move
(`createThreadGroupAndMoveAction`, the thread menu's "New Group…") asks
AFTER the move: the move re-sorts the group by its members' activity, the
keyed-each reorder moves the row's DOM node, a moved node blurs the input
inside it, and blur commits the rename — an editor opened before the move
closed itself on "New Group" whenever the RPC was slow (e2e, 2026-09-02).
A row that has to move again while its editor is open still loses it; the
list does not hold position for an edit.

`ThreadGroupContextMenu` derives its membership from the threads store,
not from the ids the row rendered: those are search-filtered, and
"Archive Threads (n)" over the visible subset does a fraction of what it
says.

## Drag and drop

A drag carries ONE thread. `ThreadRow` is the only source; the payload
(`utils/threadDragPayload.ts`) carries `projectId` and the thread's
current `groupId` because a drop can land somewhere that never rendered
the row.

`DataTransfer.getData` is empty during `dragover` in every real browser,
and the group targets have to decide there — so the source also records
the payload in-process (`beginThreadRowDrag` / `endThreadRowDrag`) and
targets read it through `threadDragPayloadForEvent`, which prefers the
DataTransfer and falls back to the record. Both ends live in one
document; this is not a store, it is the drag itself.

Every drop target clears the record as well as the source's `dragend`.
`dragend` fires on the SOURCE ROW, and a row unmounts mid-drag whenever
its project collapses or a search is typed — then no dragend ever comes.

Two targets share `ProjectThreadList`:

- A GROUP — `ThreadGroupRow`'s shell, or the wrapper around any member
  row inside it — takes a thread from the same project that is not
  already a member, and `stopPropagation`s so the background never sees a
  drop a group handled.
- The list BACKGROUND takes a grouped thread of its own project and
  ungroups it.

The hover state is owned by `ProjectThreadList`, not by the row, because
a member row must light its GROUP rather than itself. It is two facts:
`dropTargetGroupId` is the group UNDER THE POINTER whether or not that
group would take the payload, and `dropTargetAccepts` is what lights the
row. A group that refuses — the payload is already its member — still
owns the pointer, and it `stopPropagation`s, so the container behind
stops hearing `dragover` entirely: without the refusal being reported,
the background's dashed ungroup outline stays lit over a drop that
ungroups nothing. The outline is down whenever any group has the pointer.
Enter/leave uses a depth counter on the group shell and a `relatedTarget`
containment check on the container: a plain leave handler drops the
highlight the moment the pointer crosses a child element.

## Persisted UI state

`stores/sidebar.svelte.ts` owns the localStorage keys. Two of them are
inverses, deliberately:

| Key | Holds |
|---|---|
| `sidebar:expandedDiscussions` | discussion ids the user OPENED (closed by default) |
| `sidebar:collapsedGroups` | group ids the user CLOSED (open by default) |

A group the user just made must show what is in it, so only explicit
collapses persist.

Both sets are global across projects while `syncExpandedTreeForActiveThread`
runs once PER PROJECT, so neither may drop an id this project's tree does
not contain. The collapsed set is never pruned at all. The expanded set
prunes only ids naming a thread of the tree in hand that is no longer
expandable; pruning against the whole set instead had each project's pass
delete the other's, and two expanded projects converged on empty.
