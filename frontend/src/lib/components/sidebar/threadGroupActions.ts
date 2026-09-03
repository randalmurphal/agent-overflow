// Thread-group action handlers.
//
// Shaped like threadRowActions.ts and for the same reason: plain TS, no
// runes and no component context, so the group row, the context menus and
// the drop targets all drive one tested code path instead of three copies
// of "await the RPC, then reconcile the store".
//
// Every action reconciles from the RPC's OWN response rather than from
// what the caller asked for — the backend trims names and strips a pin on
// grouping, so the returned row is the only truthful copy. The matching
// `thread-group:updated` / `thread:updated` events arrive right after and
// re-apply the same values, which is a no-op.
//
// Failures surface as a toast and a falsy return. A caller that has UI to
// roll back (an inline rename) checks the return; the rest can ignore it.

import {
  CreateThreadGroup,
  DeleteThreadGroup,
  PinThreadGroup,
  RenameThreadGroup,
  SetThreadGroup,
  SetThreadGroupPinGroup,
  UnpinThreadGroup,
} from '../../stores/bindings';
import {
  removeThreadGroup,
  requestGroupRename,
  upsertThreadGroup,
} from '../../stores/threadGroups.svelte';
import {
  clearThreadGroupMembership,
  updateThreadGroupState,
} from '../../stores/threads.svelte';
import { expandProject } from '../../stores/sidebar.svelte';
import { setThreadFilterQuery } from '../../stores/threadFilter.svelte';
import { addToast } from '../../stores/toast.svelte';
import { userFacingError } from '../../utils/userFacingError';
import type { Thread, ThreadGroup } from '../../types/models';
import { PIN_GROUP_BACK, PIN_GROUP_FRONT } from './threadRowActions';

/** The name a group is born with; the row opens inline rename on top of it. */
export const NEW_THREAD_GROUP_NAME = 'New Group';

function reportGroupFailure(what: string, err: unknown): void {
  console.error(`Failed to ${what}:`, err);
  addToast('error', userFacingError(err));
}

async function createThreadGroup(projectId: string, name: string): Promise<ThreadGroup | null> {
  try {
    const created = await CreateThreadGroup(projectId, name) as ThreadGroup;
    upsertThreadGroup(created);
    return created;
  } catch (err) {
    reportGroupFailure('create thread group', err);
    return null;
  }
}

/**
 * Create, and ask the new row to open inline rename over the "New Group"
 * placeholder. The request is made HERE, never by a caller: the row picks
 * it up on mount or, when it mounted first, when the request lands (the
 * door is reactive).
 */
export async function createThreadGroupAction(
  projectId: string,
  name: string = NEW_THREAD_GROUP_NAME,
): Promise<ThreadGroup | null> {
  const created = await createThreadGroup(projectId, name);
  if (created) requestGroupRename(created.id);
  return created;
}

/**
 * Create and move threads in, then ask for the rename. The order is the
 * point: the move re-sorts the group by its members' activity, a keyed-each
 * reorder moves the row's DOM node, and a moved node blurs the input inside
 * it — and blur commits the rename. An editor opened before the move
 * closed itself on "New Group" every time the RPC was slow enough.
 */
export async function createThreadGroupAndMoveAction(
  projectId: string,
  threadIds: readonly string[],
): Promise<ThreadGroup | null> {
  const created = await createThreadGroup(projectId, NEW_THREAD_GROUP_NAME);
  if (!created) return null;
  await moveThreadsToGroupAction(threadIds, created.id);
  requestGroupRename(created.id);
  return created;
}

/**
 * Create from the project header (its menu item and its folder-plus
 * button). Clears the sidebar search first and expands the project: a new
 * group is empty and named "New Group", so an active query drops it from
 * the project's bucket and a collapsed project never mounts the row, and
 * either way the rename the user is about to type into would never open.
 */
export async function newThreadGroupInProject(projectId: string): Promise<ThreadGroup | null> {
  setThreadFilterQuery('');
  expandProject(projectId);
  return createThreadGroupAction(projectId);
}

/**
 * Rename. A blank or unchanged name is not sent: the backend rejects a
 * blank one, and turning "the user pressed Enter on the same text" into an
 * error toast would be wrong.
 */
export async function renameThreadGroupAction(
  group: ThreadGroup,
  name: string,
): Promise<ThreadGroup | null> {
  const trimmed = name.trim();
  if (!trimmed || trimmed === group.name) return null;
  try {
    const renamed = await RenameThreadGroup(group.id, trimmed) as ThreadGroup;
    upsertThreadGroup(renamed);
    return renamed;
  } catch (err) {
    reportGroupFailure('rename thread group', err);
    return null;
  }
}

/**
 * Delete. Deleting a group never deletes a thread: SQLite nulls each
 * member's `group_id` (ON DELETE SET NULL) and the members return to the
 * project's top level. That half emits no thread rows, so the local
 * membership is cleared here.
 */
export async function deleteThreadGroupAction(groupId: string): Promise<boolean> {
  try {
    await DeleteThreadGroup(groupId);
    removeThreadGroup(groupId);
    clearThreadGroupMembership(groupId);
    return true;
  } catch (err) {
    reportGroupFailure('delete thread group', err);
    return false;
  }
}

export async function pinThreadGroupAction(groupId: string): Promise<ThreadGroup | null> {
  try {
    const pinned = await PinThreadGroup(groupId) as ThreadGroup;
    upsertThreadGroup(pinned);
    return pinned;
  } catch (err) {
    reportGroupFailure('pin thread group', err);
    return null;
  }
}

export async function unpinThreadGroupAction(groupId: string): Promise<ThreadGroup | null> {
  try {
    const unpinned = await UnpinThreadGroup(groupId) as ThreadGroup;
    upsertThreadGroup(unpinned);
    return unpinned;
  } catch (err) {
    reportGroupFailure('unpin thread group', err);
    return null;
  }
}

export async function setThreadGroupPinGroupAction(
  groupId: string,
  pinGroup: typeof PIN_GROUP_FRONT | typeof PIN_GROUP_BACK,
): Promise<ThreadGroup | null> {
  try {
    const moved = await SetThreadGroupPinGroup(groupId, pinGroup) as ThreadGroup;
    upsertThreadGroup(moved);
    return moved;
  } catch (err) {
    reportGroupFailure('move pinned thread group', err);
    return null;
  }
}

/**
 * Move threads into a group. The response carries every row the call
 * touched — discussion children follow their root, and a moved row comes
 * back unpinned because a grouped thread cannot hold a pin.
 */
export async function moveThreadsToGroupAction(
  threadIds: readonly string[],
  groupId: string,
): Promise<boolean> {
  return setThreadGroupMembership(threadIds, groupId, 'move threads into group');
}

/** Ungroup: the same RPC with an empty group id. */
export async function removeThreadsFromGroupAction(
  threadIds: readonly string[],
): Promise<boolean> {
  return setThreadGroupMembership(threadIds, '', 'remove threads from group');
}

async function setThreadGroupMembership(
  threadIds: readonly string[],
  groupId: string,
  what: string,
): Promise<boolean> {
  if (threadIds.length === 0) return false;
  try {
    const rows = await SetThreadGroup([...threadIds], groupId) as Thread[];
    updateThreadGroupState(rows);
    return true;
  } catch (err) {
    reportGroupFailure(what, err);
    return false;
  }
}
