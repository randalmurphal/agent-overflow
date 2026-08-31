// Sidebar thread tree + multi-key sort.
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
import type { Thread } from '../types/models';
import {
  resolveEffectiveThreadStatus,
  resolveThreadStatusPill,
  type ThreadStatusPill,
} from './threadStatusPill';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';
import { isHiddenThreadMode } from './threadModes';
import { reportFrontendDiagnostic } from './frontendErrorCapture';

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
  /** Status tier without the pin override; orders rows inside a pin block. */
  normalSortGroup: NormalThreadStatusSortGroup;
  /** Max(thread.updatedAt, max(child.latestActivityAt)). Unix ms. */
  latestActivityAt: number;
}

export interface SidebarTreeVisibleNode extends SidebarTreeNode {
  isExpanded: boolean;
  isExpandable: boolean;
  /** True only on the first top-level back-burner row when both blocks exist. */
  startsBackBurnerBlock: boolean;
}

export interface BuildSidebarThreadTreeInput {
  threads: readonly Thread[];
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

export function sidebarPinGroup(thread: Thread): SidebarPinGroup {
  if (thread.pinnedAt == null) return null;
  return thread.pinGroup === 1 ? 'back' : 'front';
}

function resolveLatestActivityAt(
  thread: Thread,
  children: readonly SidebarTreeNode[],
  activityOf: ((thread: Thread) => number) | undefined,
): number {
  let latest = activityOf ? activityOf(thread) : (thread.updatedAt ?? 0);
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
  const leftDraft = left.thread.isDraft === true;
  const rightDraft = right.thread.isDraft === true;
  if (leftDraft !== rightDraft) return leftDraft ? -1 : 1;
  if (leftDraft && rightDraft) {
    if (right.thread.createdAt !== left.thread.createdAt) {
      return right.thread.createdAt > left.thread.createdAt ? 1 : -1;
    }
    if (left.thread.id === right.thread.id) return 0;
    return left.thread.id < right.thread.id ? 1 : -1;
  }

  const leftPinGroup = sidebarPinGroup(left.thread);
  const rightPinGroup = sidebarPinGroup(right.thread);
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
    const latestActivityAt = resolveLatestActivityAt(thread, children, input.activityOf);

    return {
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

  const topLevel = visibleThreads.filter((thread) => {
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

  const visit = (node: SidebarTreeNode, startsBackBurnerBlock = false) => {
    const isExpandable = node.children.length > 0;
    const isExpanded = isExpandable && input.expandedThreadIds.has(node.thread.id);
    visibleNodes.push({ ...node, isExpanded, isExpandable, startsBackBurnerBlock });
    if (!isExpanded) return;
    for (const child of node.children) visit(child);
  };

  const hasFrontBurner = input.nodes.some((node) => sidebarPinGroup(node.thread) === 'front');
  const hasBackBurner = input.nodes.some((node) => sidebarPinGroup(node.thread) === 'back');
  let markedBackBurner = false;
  for (const node of input.nodes) {
    const startsBackBurnerBlock = hasFrontBurner
      && hasBackBurner
      && !markedBackBurner
      && sidebarPinGroup(node.thread) === 'back';
    if (startsBackBurnerBlock) markedBackBurner = true;
    visit(node, startsBackBurnerBlock);
  }
  return visibleNodes;
}

/**
 * Content equality for status pills. Pills are minted fresh on every
 * tree build, so the identity cutoffs below must compare fields, not
 * references.
 */
export function sameThreadStatusPill(
  a: ThreadStatusPill | null,
  b: ThreadStatusPill | null,
): boolean {
  if (a === b) return true;
  if (a === null || b === null) return false;
  return a.label === b.label
    && a.dotClass === b.dotClass
    && a.labelClass === b.labelClass
    && a.pulse === b.pulse
    && a.glowClass === b.glowClass;
}

/**
 * Render-content equality for the flattened sidebar list. The
 * ProjectThreadList derived returns its PREVIOUS array when this holds,
 * so svelte's derived cutoff stops the animated each-block from
 * reconciling — and the FLIP measure pass (getBoundingClientRect per
 * visible row, a forced layout mid-flush) only runs when membership,
 * order, or a row's rendered fields actually changed. latestActivityAt
 * is deliberately NOT compared: it moves on every streaming beat, it is
 * sort input rather than render input, and comparing it would defeat
 * the cutoff.
 */
export function sameSidebarVisibleNodes(
  a: readonly SidebarTreeVisibleNode[],
  b: readonly SidebarTreeVisibleNode[],
): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    const x = a[i];
    const y = b[i];
    if (x.thread !== y.thread) return false;
    if (x.depth !== y.depth) return false;
    if (x.isExpanded !== y.isExpanded || x.isExpandable !== y.isExpandable) return false;
    if (x.startsBackBurnerBlock !== y.startsBackBurnerBlock) return false;
    if (x.ownLiveStatus !== y.ownLiveStatus || x.displayLiveStatus !== y.displayLiveStatus) return false;
    if (!sameThreadStatusPill(x.ownStatus, y.ownStatus)) return false;
    if (!sameThreadStatusPill(x.displayStatus, y.displayStatus)) return false;
  }
  return true;
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
 * Slice a sorted top-level node list into a preview window. If
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

  // Drafts and pinned both render outside the truncated unpinned tail.
  // Drafts come first to match compareTreeNodes (the user is actively
  // composing them; pin state is a slower-moving curation choice).
  const drafts: SidebarTreeNode[] = [];
  const pinned: SidebarTreeNode[] = [];
  const rest: SidebarTreeNode[] = [];
  for (const node of input.nodes) {
    if (node.thread.isDraft === true) drafts.push(node);
    else if (node.thread.pinnedAt != null) pinned.push(node);
    else rest.push(node);
  }

  const head = rest.slice(0, limit);
  const tail = rest.slice(limit);

  if (tail.length === 0) {
    return { visibleNodes: [...drafts, ...pinned, ...head], hiddenNodes: [] };
  }

  // Active thread might already be in drafts/pinned/head; if so, hide the tail.
  const activeId = input.activeThreadId;
  const isActive = (n: SidebarTreeNode) => n.thread.id === activeId;
  const activeAboveTail = !activeId
    ? false
    : drafts.some(isActive) || pinned.some(isActive) || head.some(isActive);
  if (!activeId || activeAboveTail) {
    return { visibleNodes: [...drafts, ...pinned, ...head], hiddenNodes: tail };
  }

  // Active thread sits in the tail — float it back to visibility but
  // keep the rest of the tail hidden.
  const activeNode = tail.find(isActive);
  if (!activeNode) {
    return { visibleNodes: [...drafts, ...pinned, ...head], hiddenNodes: tail };
  }
  return {
    visibleNodes: [...drafts, ...pinned, ...head, activeNode],
    hiddenNodes: tail.filter((n) => n.thread.id !== activeId),
  };
}

export function nextSidebarThreadRevealLimit(input: {
  nodes: readonly SidebarTreeNode[];
  activeThreadId: string | null;
  currentLimit: number;
  revealCount: number;
}): number {
  const currentPreview = previewSidebarThreads({
    nodes: input.nodes,
    activeThreadId: input.activeThreadId,
    limit: input.currentLimit,
  });
  const targetHiddenCount = Math.max(0, currentPreview.hiddenNodes.length - input.revealCount);
  let nextLimit = input.currentLimit;
  let nextPreview = currentPreview;

  while (nextPreview.hiddenNodes.length > targetHiddenCount) {
    nextLimit += 1;
    nextPreview = previewSidebarThreads({
      nodes: input.nodes,
      activeThreadId: input.activeThreadId,
      limit: nextLimit,
    });
  }

  return nextLimit;
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

  // `parentThreadId` is backend data, and a cycle in it (A's parent is B,
  // B's parent is A) would spin this walk forever inside one macrotask —
  // a wedged renderer with nothing reported. Bound it by the ancestors
  // already seen, and report, because a cycle here means the thread tree is
  // corrupt and the sidebar is only the first place it shows up.
  //
  // Defence in depth, deliberately: a cycle cannot reach this walk TODAY
  // through `buildSidebarThreadTree`, which excludes cycle members from its
  // roots and bounds nesting by `maxDepth` — so `parentByThreadId` above
  // never contains a cyclic chain that starts at a rendered node. That is a
  // property of the builder, not of this function's inputs, and this function
  // is exported and callable with any node array. The Set is therefore
  // allocated only if the loop is entered at all, which is the common case's
  // cost (zero) versus a corrupt tree's (one Set).
  let cursor = input.activeThreadId ? parentByThreadId.get(input.activeThreadId) ?? null : null;
  let walked: Set<string> | null = null;
  while (cursor !== null) {
    if (walked === null) {
      walked = new Set<string>(input.activeThreadId ? [input.activeThreadId] : []);
    }
    if (walked.has(cursor)) {
      // Constant message, variables in `detail`: ids in the message would mint
      // a signature per corrupt thread and bypass the per-signature cap.
      // Console too — a remote session cannot persist (the reporter is
      // host-scoped), so there the console line is the only evidence.
      const detail = `revisited ${cursor} expanding ancestors of ${input.activeThreadId}`;
      console.warn(`[sidebarTree] parentThreadId cycle; walk stopped (${detail})`);
      reportFrontendDiagnostic(
        'sidebarTree: parentThreadId cycle — an ancestor was reached twice while expanding ' +
          'the active thread; walk stopped',
        detail,
      );
      break;
    }
    walked.add(cursor);
    if (expandableIds.has(cursor)) next.add(cursor);
    cursor = parentByThreadId.get(cursor) ?? null;
  }

  return next;
}
