// Sidebar thread tree + multi-key sort.
//
// Ported from forge's SidebarTree.logic.ts, adapted for agent-overflow:
//   - Unix-ms `number` timestamps instead of ISO strings.
//   - A `pinned` tier above forge's groups (driven by Thread.pinnedAt).
//   - Null pills (idle + read) treated as the lowest priority instead of
//     forge's PAUSED/COMPLETED placeholder pills.
//
// Pure logic, no Svelte / DOM imports — table-drivable from unit tests.
//
// Sort order (highest first):
//   1. Pinned tier (Thread.pinnedAt set) — within tier: pinnedAt desc.
//   2. needs-attention (error / pending-approval / awaiting-input / plan-ready / interrupted).
//   3. running (any mode — Working / Planning / Designing / Discussing).
//   4. paused (reserved for future; no current source emits it).
//   5. completed (idle + unread).
//   6. idle + read (no pill).
// Within each tier (after pinned): latestActivityAt desc, then thread.id
// localeCompare for stability.
//
// Discussion children bubble:
//   - displayStatus: a child's higher-priority status is shown on the
//     parent row UNLESS the parent itself sits in a more important tier
//     than the child (mirrors forge's resolveDisplayStatus).
//   - latestActivityAt: parent's effective activity is the max of its own
//     `updatedAt` and any descendant's latestActivityAt, so a parent
//     with a freshly-active child surfaces in the sort even if its own
//     row is stale.

import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread } from '../types/models';
import {
  resolveEffectiveThreadStatus,
  resolveThreadStatusPill,
  type ThreadStatusPill,
} from '../components/sidebar/threadStatusPill';

export const DEFAULT_SIDEBAR_TREE_MAX_DEPTH = 2;
export const THREAD_PREVIEW_LIMIT = 8;

export type ThreadStatusSortGroup =
  | 'pinned'
  | 'needs-attention'
  | 'running'
  | 'paused'
  | 'completed';

// Mirrors forge's flat priority for non-blocking tiers — running, paused,
// and completed share priority so within-tier sort is by activity time
// alone. Splitting them would force a hard hierarchy where a running
// thread always outranks an idle one regardless of recency, which isn't
// the design intent (a freshly-touched idle thread should still surface
// over an old running one).
export const SORT_GROUP_PRIORITY: Record<ThreadStatusSortGroup, number> = {
  pinned: 3,
  'needs-attention': 2,
  running: 1,
  paused: 1,
  completed: 1,
};

// Within-group ordering of live statuses. Used by displayStatus bubbling
// to pick the most important child status, NOT by the tree compare
// (which collapses all needs-attention rows into one tier and sorts by
// activity timestamp). Higher number = higher priority.
const STATUS_PRIORITY: Record<ThreadLiveStatus, number> = {
  error: 100,
  'pending-approval': 90,
  'awaiting-input': 80,
  'plan-ready': 60,
  running: 50,
  interrupted: 40,
  idle: 0,
};

export interface SidebarTreeNode {
  thread: Thread;
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
  /** Max(thread.updatedAt, max(child.latestActivityAt)). Unix ms. */
  latestActivityAt: number;
}

export interface SidebarTreeVisibleNode extends SidebarTreeNode {
  isExpanded: boolean;
  isExpandable: boolean;
}

export interface BuildSidebarThreadTreeInput {
  threads: readonly Thread[];
  liveStatusOf: (threadId: string) => ThreadLiveStatus;
  maxDepth?: number;
}

export interface FlattenSidebarThreadTreeInput {
  nodes: readonly SidebarTreeNode[];
  expandedThreadIds: ReadonlySet<string>;
}

/**
 * statusPriority — null collapses to 0 (lowest) so an idle+read thread
 * loses the bubble check against any child that has any pill.
 */
function statusPriority(status: ThreadLiveStatus): number {
  return STATUS_PRIORITY[status] ?? 0;
}

function getStatusSortGroup(thread: Thread, liveStatus: ThreadLiveStatus): ThreadStatusSortGroup {
  if (thread.pinnedAt != null) return 'pinned';
  switch (liveStatus) {
    case 'error':
    case 'pending-approval':
    case 'awaiting-input':
    case 'plan-ready':
    case 'interrupted':
      return 'needs-attention';
    case 'running':
      return 'running';
    case 'idle':
      return 'completed';
    default:
      // Defensive fallback — a future ThreadLiveStatus enum member
      // should not silently land in `undefined`. Tiering as completed
      // keeps it visible without claiming attention it hasn't earned.
      return 'completed';
  }
}

function resolveLatestActivityAt(thread: Thread, children: readonly SidebarTreeNode[]): number {
  let latest = thread.updatedAt ?? 0;
  for (const child of children) {
    if (child.latestActivityAt > latest) latest = child.latestActivityAt;
  }
  return latest;
}

/**
 * Pick the displayed status for a parent row given its own status and
 * its children. Mirrors forge:resolveDisplayStatus — the parent keeps
 * its own status when it has a strictly higher group priority AND it's
 * not in paused/completed (so a "completed" parent surfaces a "running"
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

  // Single-pass max — equivalent to sorting by statusPriority and
  // taking [0] but allocates nothing.
  let topChild: SidebarTreeNode | null = null;
  let topPriority = -1;
  for (const child of children) {
    const priority = statusPriority(child.displayLiveStatus);
    if (priority > topPriority) {
      topPriority = priority;
      topChild = child;
    }
  }
  if (!topChild) {
    return { displayLiveStatus: ownLiveStatus, displayStatus: ownPill };
  }

  const childGroup = topChild.sortGroup;
  const ownIsPassive = ownGroup === 'paused' || ownGroup === 'completed';
  if (SORT_GROUP_PRIORITY[ownGroup] > SORT_GROUP_PRIORITY[childGroup] && !ownIsPassive) {
    return { displayLiveStatus: ownLiveStatus, displayStatus: ownPill };
  }
  return { displayLiveStatus: topChild.displayLiveStatus, displayStatus: topChild.displayStatus };
}

/**
 * compareTreeNodes — pinned-tier first (with within-tier pinnedAt-desc
 * tiebreak), then sort group, then activity desc, then id for stability.
 */
function compareTreeNodes(left: SidebarTreeNode, right: SidebarTreeNode): number {
  const leftPinned = left.thread.pinnedAt ?? 0;
  const rightPinned = right.thread.pinnedAt ?? 0;
  if (leftPinned !== rightPinned) {
    if (leftPinned > 0 && rightPinned > 0) return rightPinned - leftPinned;
    return rightPinned > 0 ? 1 : -1;
  }

  const groupCmp = SORT_GROUP_PRIORITY[right.sortGroup] - SORT_GROUP_PRIORITY[left.sortGroup];
  if (groupCmp !== 0) return groupCmp;

  if (right.latestActivityAt !== left.latestActivityAt) {
    return right.latestActivityAt > left.latestActivityAt ? 1 : -1;
  }

  // Lexicographic tie-break for stability. Plain `<` / `>` is faster
  // than localeCompare for ASCII UUIDs (the only id source) and the
  // ordering only needs to be stable, not locale-correct.
  if (left.thread.id === right.thread.id) return 0;
  return left.thread.id < right.thread.id ? 1 : -1;
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
 */
export function buildSidebarThreadTree(input: BuildSidebarThreadTreeInput): SidebarTreeNode[] {
  const maxDepth = input.maxDepth ?? DEFAULT_SIDEBAR_TREE_MAX_DEPTH;
  const threadsById = new Map(input.threads.map((t) => [t.id, t] as const));
  const childIdsByParent = new Map<string, string[]>();

  for (const thread of input.threads) {
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

    const ownLiveStatus = resolveEffectiveThreadStatus(thread, input.liveStatusOf(thread.id));
    const ownPill = resolveThreadStatusPill(thread, ownLiveStatus);
    const ownGroup = getStatusSortGroup(thread, ownLiveStatus);
    const display = resolveDisplay(ownLiveStatus, ownPill, ownGroup, children);
    const latestActivityAt = resolveLatestActivityAt(thread, children);

    return {
      thread,
      depth,
      children,
      ownLiveStatus,
      ownStatus: ownPill,
      displayLiveStatus: display.displayLiveStatus,
      displayStatus: display.displayStatus,
      sortGroup: getStatusSortGroup(thread, display.displayLiveStatus),
      latestActivityAt,
    };
  };

  const topLevel = input.threads.filter((thread) => {
    const parentId = thread.parentThreadId;
    if (!parentId) return true;
    return !threadsById.has(parentId);
  });

  return topLevel.map((t) => buildNode(t, 0)).sort(compareTreeNodes);
}

/**
 * flattenSidebarThreadTree — depth-first walk, descending into a node's
 * children only when its id is in `expandedThreadIds`. Returned nodes
 * carry their depth so the renderer can indent without a second pass.
 */
export function flattenSidebarThreadTree(
  input: FlattenSidebarThreadTreeInput,
): SidebarTreeVisibleNode[] {
  const visibleNodes: SidebarTreeVisibleNode[] = [];

  const visit = (node: SidebarTreeNode) => {
    const isExpandable = node.children.length > 0;
    const isExpanded = isExpandable && input.expandedThreadIds.has(node.thread.id);
    visibleNodes.push({ ...node, isExpanded, isExpandable });
    if (!isExpanded) return;
    for (const child of node.children) visit(child);
  };

  for (const node of input.nodes) visit(node);
  return visibleNodes;
}

/**
 * toggleSidebarTreeThreadExpansion — pure helper for the discussion
 * expand/collapse store. Returns a new Set so callers can swap state
 * without mutating shared references.
 */
export function toggleSidebarTreeThreadExpansion(
  expandedThreadIds: ReadonlySet<string>,
  threadId: string,
): Set<string> {
  const next = new Set(expandedThreadIds);
  if (next.has(threadId)) next.delete(threadId);
  else next.add(threadId);
  return next;
}

/**
 * Slice a sorted top-level node list into a 6-row preview window. If
 * the active thread sits outside that window we surface it alongside
 * (mirrors t3-code: 6 + active = up to 7 rows). Pinned threads always
 * stay visible — they don't consume preview slots; the limit only
 * truncates the unpinned tail.
 */
export interface PreviewThreadsResult {
  visibleNodes: SidebarTreeNode[];
  hiddenNodes: SidebarTreeNode[];
}

export function previewSidebarThreads(input: {
  nodes: readonly SidebarTreeNode[];
  activeThreadId: string | null;
  limit?: number;
}): PreviewThreadsResult {
  const limit = input.limit ?? THREAD_PREVIEW_LIMIT;

  // Pinned tier always renders. Slice only operates on the unpinned tail.
  const pinned: SidebarTreeNode[] = [];
  const rest: SidebarTreeNode[] = [];
  for (const node of input.nodes) {
    if (node.thread.pinnedAt != null) pinned.push(node);
    else rest.push(node);
  }

  const head = rest.slice(0, limit);
  const tail = rest.slice(limit);

  if (tail.length === 0) {
    return { visibleNodes: [...pinned, ...head], hiddenNodes: [] };
  }

  // Active thread might already be in head; if it is, we just hide the tail.
  const activeId = input.activeThreadId;
  const activeInHead = !activeId ? false : head.some((n) => n.thread.id === activeId);
  const pinnedHasActive = !activeId ? false : pinned.some((n) => n.thread.id === activeId);
  if (!activeId || activeInHead || pinnedHasActive) {
    return { visibleNodes: [...pinned, ...head], hiddenNodes: tail };
  }

  // Active thread sits in the tail — float it back to visibility but
  // keep the rest of the tail hidden.
  const activeNode = tail.find((n) => n.thread.id === activeId);
  if (!activeNode) {
    return { visibleNodes: [...pinned, ...head], hiddenNodes: tail };
  }
  return {
    visibleNodes: [...pinned, ...head, activeNode],
    hiddenNodes: tail.filter((n) => n.thread.id !== activeId),
  };
}

/**
 * Roll up the most-important display status across a list of nodes —
 * used both for the "Show more" hidden-status pill and the per-project
 * status dot when the project is collapsed.
 */
export function rollupDisplayStatus(
  nodes: readonly SidebarTreeNode[],
): { liveStatus: ThreadLiveStatus; pill: ThreadStatusPill } | null {
  let best: { liveStatus: ThreadLiveStatus; pill: ThreadStatusPill } | null = null;
  let bestPriority = 0;
  for (const node of nodes) {
    if (node.displayStatus == null) continue;
    const priority = statusPriority(node.displayLiveStatus);
    if (priority > bestPriority) {
      best = { liveStatus: node.displayLiveStatus, pill: node.displayStatus };
      bestPriority = priority;
    }
  }
  return best;
}

/**
 * Auto-expand the chain of ancestors leading to the active thread so
 * the active row is always visible in the rendered tree. Drops any
 * expanded ids that no longer correspond to expandable nodes (a child
 * was deleted, parent is now leaf).
 */
export function syncExpandedTreeForActiveThread(input: {
  nodes: readonly SidebarTreeNode[];
  expandedThreadIds: ReadonlySet<string>;
  activeThreadId: string | null;
}): Set<string> {
  const expandableIds = new Set<string>();
  const parentByThreadId = new Map<string, string | null>();

  const visit = (node: SidebarTreeNode) => {
    parentByThreadId.set(node.thread.id, node.thread.parentThreadId ?? null);
    if (node.children.length > 0) expandableIds.add(node.thread.id);
    for (const child of node.children) visit(child);
  };
  for (const node of input.nodes) visit(node);

  const next = new Set([...input.expandedThreadIds].filter((id) => expandableIds.has(id)));

  let cursor = input.activeThreadId ? parentByThreadId.get(input.activeThreadId) ?? null : null;
  while (cursor !== null) {
    if (expandableIds.has(cursor)) next.add(cursor);
    cursor = parentByThreadId.get(cursor) ?? null;
  }

  return next;
}
