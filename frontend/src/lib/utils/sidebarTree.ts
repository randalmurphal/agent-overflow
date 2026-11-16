// Sidebar thread tree: the BUILD half — node shapes, the sort comparator,
// status bubbling, and buildSidebarThreadTree itself. The VIEW half (flatten,
// the preview cut, the identity cutoffs and discussion expansion cleanup)
// lives in `sidebarTreeView.ts` and imports from here; nothing imports back.
//
// Pure logic, no Svelte / DOM imports — table-drivable from unit tests.
//
// Sort order (highest first):
//   0. Drafts, newest first.
//   1. Groups, a section of their own at the top level. Groups are not
//      pinnable; within the section they take the normal comparator below.
//   2. Pinned front-burner block, then pinned back-burner block, then
//      unpinned rows. Every block, a group's members included, orders by
//      the status tiers below.
// Status tiers (a pin is not one):
//   a. needs-attention (error / pending-approval / awaiting-input /
//      setup-failed / plan-ready / interrupted).
//   b. running (any mode — Working / Planning / Designing / Discussing).
//   c. completed (idle + unread). Below running because opening the row
//      reads it and drops it to idle, and that move must not cross the
//      running rows.
//   d. idle + read (no pill).
// Within each tier: latestActivityAt desc, then id for stability.
//
// Discussion children bubble:
//   - displayStatus: the parent shows its most important child's status
//     unless its own ranks strictly higher, by tier and then by live
//     status.
//   - latestActivityAt: parent's effective activity is the max of its own
//     `updatedAt` and any descendant's latestActivityAt, so a parent
//     with a freshly-active child surfaces in the sort even if its own
//     row is stale.

import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread, ThreadGroup } from '../types/models';
import {
  resolveEffectiveThreadStatus,
  lastKnownThreadStatusPill,
  resolveThreadStatusPill,
  type ThreadStatusPill,
} from './threadStatusPill';
import { isHiddenThreadMode } from './threadModes';

export const DEFAULT_SIDEBAR_TREE_MAX_DEPTH = 2;

/** A row's status tier. A pin orders rows separately and is never a tier. */
export type ThreadStatusSortGroup =
  | 'needs-attention'
  | 'running'
  | 'completed'
  | 'idle';

export type SidebarPinGroup = 'front' | 'back' | null;

const SORT_GROUP_PRIORITY: Record<ThreadStatusSortGroup, number> = {
  'needs-attention': 3,
  running: 2,
  completed: 1,
  idle: 0,
};

// Ordering of live statuses inside one tier. Used by displayStatus
// bubbling to pick the most important status, NOT by the tree compare
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
 * activity timestamp, because those are exactly what the sort reads
 * inside the group section.
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
  /** Status tier of the displayed status; orders rows inside a pin block. */
  sortGroup: ThreadStatusSortGroup;
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
  /**
   * The name of the thread's computer while it is unreachable, else null.
   * Its status is the last one reported and is shown still and attributed
   * (lastKnownThreadStatusPill), in its own row and wherever it bubbles to.
   */
  unreachableMachineOf?: (thread: Thread) => string | null;
  maxDepth?: number;
}

/** A missing entry collapses to 0, the rank of idle. */
function statusPriority(status: ThreadLiveStatus): number {
  return STATUS_PRIORITY[status] ?? 0;
}

function getStatusSortGroup(
  liveStatus: ThreadLiveStatus,
  status: ThreadStatusPill | null,
): ThreadStatusSortGroup {
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

export function sidebarPinGroup(thread: Pick<Thread, 'pinnedAt' | 'pinGroup'>): SidebarPinGroup {
  if (thread.pinnedAt == null) return null;
  return thread.pinGroup === 1 ? 'back' : 'front';
}

/** Stable id for a node of either kind — the each-block key and sort tie-break. */
export function sidebarTreeNodeId(node: SidebarTreeNode): string {
  return node.kind === 'thread' ? node.thread.id : node.group.id;
}

/** A group is never pinned; a thread node's burner is its thread's. */
export function sidebarNodePinGroup(node: SidebarTreeNode): SidebarPinGroup {
  return node.kind === 'thread' ? sidebarPinGroup(node.thread) : null;
}

/**
 * The top-level section a node sorts and renders in. Drafts sit above
 * every section and belong to none, so they draw no section divider.
 */
export type SidebarSection = 'groups' | 'front' | 'back' | 'unpinned';

export function sidebarNodeSection(node: SidebarTreeNode): SidebarSection | null {
  if (isDraftNode(node)) return null;
  if (node.kind === 'group') return 'groups';
  return sidebarNodePinGroup(node) ?? 'unpinned';
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

type RankedStatus = Pick<SidebarTreeNodeBase, 'sortGroup' | 'displayLiveStatus'>;

/**
 * Whether `a`'s displayed status ranks strictly above `b`'s: by tier,
 * then by live-status priority inside the tier. The one rank for status
 * bubbling and for the rollup in `sidebarTreeView.ts`. Pins play no part.
 */
export function displayStatusOutranks(a: RankedStatus, b: RankedStatus): boolean {
  const tierCmp = SORT_GROUP_PRIORITY[a.sortGroup] - SORT_GROUP_PRIORITY[b.sortGroup];
  if (tierCmp !== 0) return tierCmp > 0;
  return statusPriority(a.displayLiveStatus) > statusPriority(b.displayLiveStatus);
}

/**
 * Pick the displayed status for a parent row. The parent keeps its own
 * status only when it outranks its top child; otherwise the child's
 * status shows, its pill reused verbatim so mode-derived labels read
 * accurately. A pinned idle member therefore never masks a running one.
 */
function resolveDisplay(
  ownLiveStatus: ThreadLiveStatus,
  ownPill: ThreadStatusPill | null,
  children: readonly SidebarTreeNode[],
): { displayLiveStatus: ThreadLiveStatus; displayStatus: ThreadStatusPill | null } {
  let top: SidebarTreeNode | null = null;
  for (const child of children) {
    if (top === null || displayStatusOutranks(child, top)) top = child;
  }
  const own = { sortGroup: getStatusSortGroup(ownLiveStatus, ownPill), displayLiveStatus: ownLiveStatus };
  if (top === null || displayStatusOutranks(own, top)) {
    return { displayLiveStatus: ownLiveStatus, displayStatus: ownPill };
  }
  return { displayLiveStatus: top.displayLiveStatus, displayStatus: top.displayStatus };
}

/**
 * compareTreeNodes — drafts pinned to the very top (newest createdAt
 * first), then groups, then front/back pin blocks (normal comparator
 * within each), then sort group, activity desc, and id for stability.
 * Drafts outrank everything because the user is actively composing them
 * and needs them surfaced regardless of pin state.
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

  // Groups form their own section above the pin blocks. Only the top level
  // holds groups, so inside a group or discussion this never decides.
  if (left.kind !== right.kind) return left.kind === 'group' ? -1 : 1;

  const leftPinGroup = sidebarNodePinGroup(left);
  const rightPinGroup = sidebarNodePinGroup(right);
  if (leftPinGroup !== rightPinGroup) {
    if (leftPinGroup === null) return 1;
    if (rightPinGroup === null) return -1;
    return leftPinGroup === 'front' ? -1 : 1;
  }

  const groupCmp = SORT_GROUP_PRIORITY[right.sortGroup] - SORT_GROUP_PRIORITY[left.sortGroup];
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
 * allows inside a group). Group nodes sort above every top-level thread
 * and among themselves by the normal comparator, so a group with a running
 * member rises within the group section the way a discussion parent does.
 * Members sort by the same comparator, their own pins included.
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
    const resolvedPill = resolveThreadStatusPill(thread, ownLiveStatus);
    const unreachableMachine = input.unreachableMachineOf?.(thread) ?? null;
    const ownPill = unreachableMachine !== null ? lastKnownThreadStatusPill(resolvedPill, unreachableMachine) : resolvedPill;
    const display = resolveDisplay(ownLiveStatus, ownPill, children);
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
      sortGroup: getStatusSortGroup(display.displayLiveStatus, display.displayStatus),
      latestActivityAt,
    };
  };

  const buildGroupNode = (group: ThreadGroup, children: SidebarTreeNode[]): SidebarTreeNode => {
    // A group has no status of its own, so the display resolve always
    // yields the most important member status.
    const display = resolveDisplay('idle', null, children);
    return {
      kind: 'group',
      group,
      depth: 0,
      children,
      ownLiveStatus: 'idle',
      ownStatus: null,
      displayLiveStatus: display.displayLiveStatus,
      displayStatus: display.displayStatus,
      sortGroup: getStatusSortGroup(display.displayLiveStatus, display.displayStatus),
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
