// Sidebar thread tree: the BUILD half — node shapes, the sort comparator,
// status bubbling, and buildSidebarThreadTree itself. The VIEW half (flatten,
// the preview cut, the identity cutoffs and the active-thread expand sync)
// lives in `sidebarTreeView.ts` and imports from here; nothing imports back.
//
// Pure logic, no Svelte / DOM imports — table-drivable from unit tests.
//
// Sort order (highest first):
//   1. Pinned front-burner block, then pinned back-burner block.
//      Within each block: the normal status/activity/id comparator below.
//   2. needs-attention (error / pending-approval / awaiting-input / plan-ready / interrupted).
//   3. running (any mode — Working / Planning / Designing / Discussing)
//      and completed (idle + unread).
//   4. paused (reserved for future; no current source emits it).
//   5. idle + read (no pill).
// Within each tier (after pinned): latestActivityAt desc, then thread.id
// localeCompare for stability.
//
// Discussion children bubble:
//   - displayStatus: a child's higher-priority status is shown on the
//     parent row UNLESS the parent itself sits in a more important tier
//     than the child.
//   - latestActivityAt: parent's effective activity is the max of its own
//     `updatedAt` and any descendant's latestActivityAt, so a parent
//     with a freshly-active child surfaces in the sort even if its own
//     row is stale.

import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import {
  resolveEffectiveThreadStatus,
  resolveThreadStatusPill,
  type ThreadStatusPill,
} from './threadStatusPill';
import { isHiddenThreadMode } from './threadModes';

export const DEFAULT_SIDEBAR_TREE_MAX_DEPTH = 2;

export type ThreadStatusSortGroup =
  | 'pinned'
  | 'needs-attention'
  | 'running'
  | 'paused'
  | 'completed'
  | 'idle';

export type NormalThreadStatusSortGroup = Exclude<ThreadStatusSortGroup, 'pinned'>;
export type SidebarPinGroup = 'front' | 'back' | null;

// Running and completed rows share the non-blocking activity tier so
// their relative order stays activity-driven. Plain idle/read rows sit
// below them because they have no status indicator worth surfacing.
export const SORT_GROUP_PRIORITY: Record<ThreadStatusSortGroup, number> = {
  pinned: 3,
  'needs-attention': 2,
  running: 1,
  paused: 1,
  completed: 1,
  idle: 0,
};

// Within-group ordering of live statuses. Used by displayStatus bubbling
// to pick the most important child status, NOT by the tree compare
// (which collapses all needs-attention rows into one tier and sorts by
// activity timestamp). Higher number = higher priority.
const STATUS_PRIORITY: Record<ThreadLiveStatus, number> = {
  error: 100,
  'pending-approval': 90,
  'awaiting-input': 80,
  // A worktree whose setup did not finish is a concrete repair the user
  // has to run, so it outranks the two informational fallbacks below it —
  // the same ordering resolveEffectiveThreadStatus applies.
  'setup-failed': 70,
  'plan-ready': 60,
  running: 50,
  interrupted: 40,
  idle: 0,
};

/**
 * Fields every sidebar row carries, whatever it is a row FOR. A group has
 * no status of its own — `ownLiveStatus` is always 'idle' and `ownStatus`
 * null — but it does carry a bubbled display status, a sort tier and an
 * activity timestamp, because those are exactly what the sort reads.
 */
export interface SidebarTreeNodeBase {
  depth: number;
  children: SidebarTreeNode[];
  /** Live status for this thread alone — does not include child bubbling. */
  ownLiveStatus: ThreadLiveStatus;
  /** Pill for ownLiveStatus. Null when the row would render no pill. */
  ownStatus: ThreadStatusPill | null;
  /**
   * Bubbled live status — the most important among this thread and its
   * descendants, or its own status when nothing more important sits below.
   */
  displayLiveStatus: ThreadLiveStatus;
  /**
   * Pill for displayLiveStatus. When bubbled, the contributing child's
   * pill is reused verbatim so mode-derived labels (Discussing /
   * Designing / Planning) read accurately.
   */
  displayStatus: ThreadStatusPill | null;
  sortGroup: ThreadStatusSortGroup;
  /** Status tier without the pin override; orders rows inside a pin block. */
  normalSortGroup: NormalThreadStatusSortGroup;
  /** Max(own updatedAt, max(child.latestActivityAt)). Unix ms. */
  latestActivityAt: number;
}

export interface SidebarThreadTreeNode extends SidebarTreeNodeBase {
  kind: 'thread';
  thread: Thread;
}

export interface SidebarGroupTreeNode extends SidebarTreeNodeBase {
  kind: 'group';
  group: ThreadGroup;
}

/**
 * A sidebar row: either a thread (the discussion tree's rows) or a thread
 * group (a named container whose top row is not a thread). Discriminate on
 * `kind` — never on the presence of a field.
 */
export type SidebarTreeNode = SidebarThreadTreeNode | SidebarGroupTreeNode;

export interface BuildSidebarThreadTreeInput {
  threads: readonly Thread[];
  /**
   * The project's groups. A TOP-LEVEL thread whose `groupId` names one of
   * them becomes that group's child; a `groupId` naming a group that is not
   * here (deleted, or filtered out by search) leaves the thread at the top
   * level rather than dropping it. Groups with no members still render —
   * an empty group persists until it is deleted.
   */
  groups?: readonly ThreadGroup[];
  statusOf?: (thread: Thread) => ThreadLiveStatus;
  liveStatusOf?: (threadId: string) => ThreadLiveStatus;
  /**
   * Newest activity timestamp for a thread. Callers pass the threads
   * store's getThreadLiveActivityAt so streaming bumps (which live in a
   * per-thread box, not on the row) still drive the activity sort.
   * Defaults to the row's own updatedAt.
   */
  activityOf?: (thread: Thread) => number;
  maxDepth?: number;
}

/**
 * statusPriority — null collapses to 0 (lowest) so an idle+read thread
 * loses the bubble check against any child that has any pill. Exported for
 * `sidebarTreeView.ts`'s rollup, which ranks the same statuses.
 */
export function statusPriority(status: ThreadLiveStatus): number {
  return STATUS_PRIORITY[status] ?? 0;
}

function getNormalStatusSortGroup(
  liveStatus: ThreadLiveStatus,
  status: ThreadStatusPill | null,
): NormalThreadStatusSortGroup {
  switch (liveStatus) {
    case 'error':
    case 'pending-approval':
    case 'awaiting-input':
    case 'plan-ready':
    case 'setup-failed':
    case 'interrupted':
      return 'needs-attention';
    case 'running':
      return 'running';
    case 'idle':
      return status === null ? 'idle' : 'completed';
    default:
      // Defensive fallback — a future ThreadLiveStatus enum member
      // should not silently land in `undefined`. Tiering as idle keeps
      // it visible without claiming attention it hasn't earned.
      return 'idle';
  }
}

function getStatusSortGroup(
  thread: Thread,
  liveStatus: ThreadLiveStatus,
  status: ThreadStatusPill | null,
): ThreadStatusSortGroup {
  if (thread.pinnedAt != null) return 'pinned';
  return getNormalStatusSortGroup(liveStatus, status);
}

/** The pin fields a row carries. Thread and ThreadGroup both satisfy it. */
export interface SidebarPinnable {
  pinnedAt?: number;
  pinGroup?: number;
}

export function sidebarPinGroup(row: SidebarPinnable): SidebarPinGroup {
  if (row.pinnedAt == null) return null;
  return row.pinGroup === 1 ? 'back' : 'front';
}

/** The row a node renders: its thread, or its group. Both carry pin fields. */
export function sidebarNodeRow(node: SidebarTreeNode): Thread | ThreadGroup {
  return node.kind === 'thread' ? node.thread : node.group;
}

/** Stable id for a node of either kind — the each-block key and sort tie-break. */
export function sidebarTreeNodeId(node: SidebarTreeNode): string {
  return node.kind === 'thread' ? node.thread.id : node.group.id;
}

export function sidebarNodePinGroup(node: SidebarTreeNode): SidebarPinGroup {
  return sidebarPinGroup(sidebarNodeRow(node));
}

/** A thread node the user is actively composing. Groups are never drafts. */
export function isDraftNode(node: SidebarTreeNode): boolean {
  return node.kind === 'thread' && node.thread.isDraft === true;
}

function resolveLatestActivityAt(
  ownActivityAt: number,
  children: readonly SidebarTreeNode[],
): number {
  let latest = ownActivityAt;
  for (const child of children) {
    if (child.latestActivityAt > latest) latest = child.latestActivityAt;
  }
  return latest;
}

/**
 * Pick the displayed status for a parent row given its own status and
 * its children. The parent keeps
 * its own status when it has a strictly higher group priority AND it's
 * not in passive rows (so an idle/read parent surfaces a "running"
 * child instead of dominating it). Otherwise the most-important child
 * status wins, including the contributing child's pill verbatim.
 */
function resolveDisplay(
  ownLiveStatus: ThreadLiveStatus,
  ownPill: ThreadStatusPill | null,
  ownGroup: ThreadStatusSortGroup,
  children: readonly SidebarTreeNode[],
): { displayLiveStatus: ThreadLiveStatus; displayStatus: ThreadStatusPill | null } {
  if (children.length === 0) {
    return { displayLiveStatus: ownLiveStatus, displayStatus: ownPill };
  }

  // Single-pass max — highest sort tier first, then highest live-status
  // priority within that tier, without allocating a sorted copy.
  let topChild: SidebarTreeNode | null = null;
  let topGroupPriority = -1;
  let topPriority = -1;
  for (const child of children) {
    const groupPriority = SORT_GROUP_PRIORITY[child.sortGroup];
    const priority = statusPriority(child.displayLiveStatus);
    if (groupPriority > topGroupPriority || (groupPriority === topGroupPriority && priority > topPriority)) {
      topGroupPriority = groupPriority;
      topPriority = priority;
      topChild = child;
    }
  }
  if (!topChild) {
    return { displayLiveStatus: ownLiveStatus, displayStatus: ownPill };
  }

  const childGroup = topChild.sortGroup;
  const ownIsPassive = ownGroup === 'paused' || ownGroup === 'completed' || ownGroup === 'idle';
  if (SORT_GROUP_PRIORITY[ownGroup] > SORT_GROUP_PRIORITY[childGroup] && !ownIsPassive) {
    return { displayLiveStatus: ownLiveStatus, displayStatus: ownPill };
  }
  return { displayLiveStatus: topChild.displayLiveStatus, displayStatus: topChild.displayStatus };
}

/**
 * compareTreeNodes — drafts pinned to the very top (newest createdAt
 * first), then front/back pin blocks (normal comparator within each),
 * then sort group, activity desc, and id for stability. Drafts
 * outrank pinned because the user is actively composing them and needs
 * them surfaced regardless of pin state.
 */
function compareTreeNodes(left: SidebarTreeNode, right: SidebarTreeNode): number {
  // Drafts are a THREAD concept: a group is a container the user curates,
  // never something being composed, so it never wins the draft block.
  const leftDraft = isDraftNode(left);
  const rightDraft = isDraftNode(right);
  if (leftDraft !== rightDraft) return leftDraft ? -1 : 1;
  if (leftDraft && rightDraft && left.kind === 'thread' && right.kind === 'thread') {
    if (right.thread.createdAt !== left.thread.createdAt) {
      return right.thread.createdAt > left.thread.createdAt ? 1 : -1;
    }
    if (left.thread.id === right.thread.id) return 0;
    return left.thread.id < right.thread.id ? 1 : -1;
  }

  const leftPinGroup = sidebarNodePinGroup(left);
  const rightPinGroup = sidebarNodePinGroup(right);
  if (leftPinGroup !== rightPinGroup) {
    if (leftPinGroup === null) return 1;
    if (rightPinGroup === null) return -1;
    return leftPinGroup === 'front' ? -1 : 1;
  }

  const leftSortGroup = leftPinGroup === null ? left.sortGroup : left.normalSortGroup;
  const rightSortGroup = rightPinGroup === null ? right.sortGroup : right.normalSortGroup;
  const groupCmp = SORT_GROUP_PRIORITY[rightSortGroup] - SORT_GROUP_PRIORITY[leftSortGroup];
  if (groupCmp !== 0) return groupCmp;

  if (right.latestActivityAt !== left.latestActivityAt) {
    return right.latestActivityAt > left.latestActivityAt ? 1 : -1;
  }

  // Lexicographic tie-break for stability. Plain `<` / `>` is faster
  // than localeCompare for ASCII UUIDs (the only id source) and the
  // ordering only needs to be stable, not locale-correct.
  const leftId = sidebarTreeNodeId(left);
  const rightId = sidebarTreeNodeId(right);
  if (leftId === rightId) return 0;
  return leftId < rightId ? 1 : -1;
}

/**
 * buildSidebarThreadTree — group threads by parentThreadId, recursively
 * sort each level, and bubble status / activity from descendants.
 *
 * Top-level: any thread with no parentThreadId, OR whose parent is
 * absent from the input (orphaned children promote rather than vanish).
 *
 * Depth cap: grandchildren beyond `maxDepth` collapse — they appear in
 * the input but won't be emitted as children of any node. This keeps the
 * indented display readable in narrow sidebars (forge ships at 2).
 *
 * Groups: a top-level thread whose `groupId` names one of `input.groups`
 * is built at depth 1 under that group's node instead (its own discussion
 * children then land at depth 2 — the three-row render depth the spec
 * allows inside a group). Group nodes sort among top-level rows by the
 * same comparator, so a group with a running member rises exactly the way
 * a discussion parent does.
 */
export function buildSidebarThreadTree(input: BuildSidebarThreadTreeInput): SidebarTreeNode[] {
  const maxDepth = input.maxDepth ?? DEFAULT_SIDEBAR_TREE_MAX_DEPTH;
  const visibleThreads = input.threads.filter((thread) => !isHiddenThreadMode(thread.mode));
  const threadsById = new Map(visibleThreads.map((t) => [t.id, t] as const));
  const childIdsByParent = new Map<string, string[]>();

  for (const thread of visibleThreads) {
    const parentId = thread.parentThreadId;
    if (!parentId) continue;
    if (!threadsById.has(parentId)) continue;
    const list = childIdsByParent.get(parentId) ?? [];
    list.push(thread.id);
    childIdsByParent.set(parentId, list);
  }

  const buildNode = (thread: Thread, depth: number): SidebarTreeNode => {
    const children =
      depth >= maxDepth
        ? []
        : (childIdsByParent.get(thread.id) ?? [])
            .map((id) => threadsById.get(id))
            .filter((t): t is Thread => t !== undefined)
            .map((child) => buildNode(child, depth + 1))
            .sort(compareTreeNodes);

    const ownLiveStatus = input.statusOf
      ? input.statusOf(thread)
      : resolveEffectiveThreadStatus(thread, input.liveStatusOf?.(thread.id) ?? 'idle');
    const ownPill = resolveThreadStatusPill(thread, ownLiveStatus);
    const ownGroup = getStatusSortGroup(thread, ownLiveStatus, ownPill);
    const display = resolveDisplay(ownLiveStatus, ownPill, ownGroup, children);
    const latestActivityAt = resolveLatestActivityAt(
      input.activityOf ? input.activityOf(thread) : (thread.updatedAt ?? 0),
      children,
    );

    return {
      kind: 'thread',
      thread,
      depth,
      children,
      ownLiveStatus,
      ownStatus: ownPill,
      displayLiveStatus: display.displayLiveStatus,
      displayStatus: display.displayStatus,
      sortGroup: getStatusSortGroup(thread, display.displayLiveStatus, display.displayStatus),
      normalSortGroup: getNormalStatusSortGroup(display.displayLiveStatus, display.displayStatus),
      latestActivityAt,
    };
  };

  const buildGroupNode = (group: ThreadGroup, children: SidebarTreeNode[]): SidebarTreeNode => {
    // A group has no status of its own, so the display resolve always
    // yields the most important member status (an 'idle' own group is
    // passive, so no member is ever suppressed).
    const display = resolveDisplay('idle', null, 'idle', children);
    return {
      kind: 'group',
      group,
      depth: 0,
      children,
      ownLiveStatus: 'idle',
      ownStatus: null,
      displayLiveStatus: display.displayLiveStatus,
      displayStatus: display.displayStatus,
      sortGroup: group.pinnedAt != null
        ? 'pinned'
        : getNormalStatusSortGroup(display.displayLiveStatus, display.displayStatus),
      normalSortGroup: getNormalStatusSortGroup(display.displayLiveStatus, display.displayStatus),
      // An empty group has nothing to bubble, so it sorts on its own last
      // write — which is when it was created or renamed.
      latestActivityAt: resolveLatestActivityAt(
        children.length === 0 ? (group.updatedAt ?? 0) : 0,
        children,
      ),
    };
  };

  const topLevel = visibleThreads.filter((thread) => {
    const parentId = thread.parentThreadId;
    if (!parentId) return true;
    return !threadsById.has(parentId);
  });

  const groups = input.groups ?? [];
  if (groups.length === 0) {
    return topLevel.map((t) => buildNode(t, 0)).sort(compareTreeNodes);
  }

  const groupsById = new Map(groups.map((group) => [group.id, group] as const));
  const membersByGroup = new Map<string, Thread[]>();
  const nodes: SidebarTreeNode[] = [];
  for (const thread of topLevel) {
    const groupId = thread.groupId;
    if (groupId && groupsById.has(groupId)) {
      const bucket = membersByGroup.get(groupId);
      if (bucket) bucket.push(thread);
      else membersByGroup.set(groupId, [thread]);
      continue;
    }
    nodes.push(buildNode(thread, 0));
  }
  for (const group of groups) {
    const members = (membersByGroup.get(group.id) ?? [])
      .map((thread) => buildNode(thread, 1))
      .sort(compareTreeNodes);
    nodes.push(buildGroupNode(group, members));
  }
  return nodes.sort(compareTreeNodes);
}
