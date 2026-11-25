import { invalidateReplicaCatalog } from '../replica/session';
import { computerCatalogWriter } from './computerCatalogWriter';
import { computerCatalog, retainUnavailableComputerRows, type CatalogPart, type ComputerRows } from './computerRows';
import { settleCatalogAnswers } from './catalogLoad.svelte';
import { forgetThreadGroup, noteThreadGroup, threadGroupBackend } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
// Thread groups: the named, collapsible sidebar rows that gather threads
// of one project. Entity-keyed on the GROUP, not on the sidebar surface
// that renders it — the tree builder, the context menus and the drop
// targets all derive from this one list.
//
// Lifecycle mirrors `threads.svelte.ts`: a wholesale load beside the
// threads it gathers and the projects those hang from, as one round
// (./sidebarCatalogs.ts), at boot, on a
// hello, on the transport-gap resync path and on each retry, and per-row
// patches from the `thread-group:updated` channel in between. The list is small
// (tens of rows at most) and changes rarely, so there is no per-entity
// signal box here — a group write is a real membership/order change and
// re-deriving the sidebar for it is correct.

import { ListThreadGroups } from './bindings';
import { onBackendDetached } from '../transport/backends';
import { clearThreadGroupMembership } from './threads.svelte';
import { collapseThreadList } from './sidebar.svelte';
import type { ThreadGroup } from '../types/models';
import type { ThreadGroupUpdateEvent } from '../types/events';

let threadGroups: ThreadGroup[] = $state.raw([]);
const catalogWriter = computerCatalogWriter('groups', () => threadGroups, (rows) => { threadGroups = rows; }, (row) => threadGroupBackend(row.id), { rowsAreValues: true });

// Shared empty bucket so a project with no groups hands every reader the
// same reference — an identity cutoff downstream can't be defeated by a
// freshly allocated []. Exported because the sidebar's map lookups need the
// same reference for their `??` fallback.
export const NO_GROUPS: readonly ThreadGroup[] = Object.freeze([]);

// Per-project buckets, rebuilt only when the source array identity
// changes and re-using the PREVIOUS bucket array whenever its members
// are element-identical. ProjectsSection / SidebarTreeList read this
// per render, so a group write in project A must not re-mint project B's
// array and wake its tree derived.
let bucketSource: ThreadGroup[] | null = null;
let buckets = new Map<string, readonly ThreadGroup[]>();

function projectBuckets(): Map<string, readonly ThreadGroup[]> {
  const all = threadGroups;
  if (bucketSource === all) return buckets;
  const next = new Map<string, ThreadGroup[]>();
  for (const group of all) {
    if (!group.projectId) continue;
    const bucket = next.get(group.projectId);
    if (bucket) bucket.push(group);
    else next.set(group.projectId, [group]);
  }
  const reconciled = new Map<string, readonly ThreadGroup[]>();
  for (const [projectId, bucket] of next) {
    const prev = buckets.get(projectId);
    const unchanged = prev !== undefined
      && prev.length === bucket.length
      && bucket.every((group, i) => prev[i] === group);
    reconciled.set(projectId, unchanged ? prev : bucket);
  }
  bucketSource = all;
  buckets = reconciled;
  return buckets;
}

export function getThreadGroups(): readonly ThreadGroup[] {
  return threadGroups;
}

/** The project's groups, in backend order. Stable identity while unchanged. */
export function getThreadGroupsForProject(projectId: string): readonly ThreadGroup[] {
  if (!projectId) return NO_GROUPS;
  return projectBuckets().get(projectId) ?? NO_GROUPS;
}

export function getThreadGroupById(id: string): ThreadGroup | undefined {
  return threadGroups.find((group) => group.id === id);
}

/**
 * Drop every group a detached backend owned, for the reason
 * `threads.svelte.ts` states about its rows: the entity index has already
 * forgotten the machine, so a group row left in the sidebar would send its
 * rename, its pin and its delete to the page's own backend.
 */
export function dropThreadGroupsForDetachedBackend(ids: readonly string[]): void {
  if (ids.length === 0) return;
  const gone = new Set(ids);
  const kept = threadGroups.filter((group) => !gone.has(group.id));
  if (kept.length === threadGroups.length) return;
  threadGroups = kept;
}

onBackendDetached(({ threadGroupIds }) => dropThreadGroupsForDetachedBackend(threadGroupIds));

function commitGroupRows(result: ComputerRows<ThreadGroup>): void {
  threadGroups = retainUnavailableComputerRows(threadGroups, result, (row) => threadGroupBackend(row.id));
  settleCatalogAnswers('groups', result.answered);
}

/**
 * How the group catalog is read from each computer and committed: only
 * beside the threads it gathers and the projects those hang from, as one
 * round (./sidebarCatalogs.ts).
 */
export function groupCatalogPart(): CatalogPart<ThreadGroup> {
  return {
    read: async () => await ListThreadGroups() as ThreadGroup[],
    note: (row, backend) => noteThreadGroup(row.id, backend),
    commit: commitGroupRows,
    catalog: computerCatalog('groups'),
  };
}

/**
 * Replace a row this client holds with a call's answer. A row it no longer
 * holds stays gone: the group was deleted while the call was out, and its
 * deletion forgot which computer owned it.
 */
export function replaceThreadGroup(group: ThreadGroup): void {
  if (!group?.id) return;
  catalogWriter.mutate(threadGroupBackend(group.id), (rows) => rows.map((existing) => existing.id === group.id ? group : existing));
}

/** Insert or replace one row: a frame's, or the answer of the call that created it. */
export function upsertThreadGroup(group: ThreadGroup): void {
  if (!group?.id) return;
  catalogWriter.mutate(threadGroupBackend(group.id), (rows) => {
    const index = rows.findIndex((existing) => existing.id === group.id);
    if (index === -1) return [...rows, group];
    const next = rows.slice();
    next[index] = group;
    return next;
  });
}

/**
 * The group was deleted: drop its row and its members' membership from its
 * computer's catalogs, then forget which computer owned it. The delete
 * emits no thread rows (SQLite nulls group_id itself), so the membership
 * is cleared here for the caller and for a client that only saw the frame.
 * Forgetting comes last so both reach the owner's catalogs.
 */
export function deleteThreadGroupLocal(id: string): void {
  if (!id) return;
  collapseThreadList(id);
  // The computer it is noted under, or home for a row never noted, which is
  // where the catalog writer files such a row.
  const backend = threadGroupBackend(id) ?? HOME_BACKEND;
  invalidateReplicaCatalog(backend, 'groups');
  catalogWriter.mutate(backend, (rows) => rows.filter((group) => group.id !== id));
  clearThreadGroupMembership(id);
  forgetThreadGroup(id);
}

/**
 * thread-group:updated handler. Membership does NOT ride this channel —
 * the backend emits thread:updated (`action: 'full'`) for every thread
 * row a group write touched, so the thread registry stays the one owner
 * of `groupId`.
 */
export function applyThreadGroupUpdated(evt: ThreadGroupUpdateEvent): void {
  if (!evt?.group?.id) return;
  if (evt.action === 'delete') {
    deleteThreadGroupLocal(evt.group.id);
    return;
  }
  upsertThreadGroup(evt.group);
}

/**
 * Pending inline rename. A group is born named "New Group" and opens rename
 * on top of it, but the row that has to do the opening does not exist yet
 * when the RPC returns — so the creator leaves the id here and the row picks
 * it up on mount.
 *
 * One id, not a set: a create is a single user gesture, and holding more
 * would mean a row somewhere off-screen silently owed an edit forever.
 *
 * `$state` so the row's effect re-runs when the request lands AFTER the
 * row mounted. Create-and-move asks only once the move has settled the
 * row's position: the editor opens focused, and a keyed-each reorder moves
 * the row's DOM node, which blurs the input and blur commits the rename.
 */
let pendingGroupRenameId = $state<string | null>(null);

export function requestGroupRename(groupId: string): void {
  pendingGroupRenameId = groupId || null;
}

/** True once, for the group that asked. Clears the request. */
export function consumePendingGroupRename(groupId: string): boolean {
  if (!groupId || pendingGroupRenameId !== groupId) return false;
  pendingGroupRenameId = null;
  return true;
}

/** Test helper: drops every row and the bucket cache. */
export function resetThreadGroupsForTest(): void {
  threadGroups = [];
  bucketSource = null;
  buckets = new Map();
  pendingGroupRenameId = null;
}
