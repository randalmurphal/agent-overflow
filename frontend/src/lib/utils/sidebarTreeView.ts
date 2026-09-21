// Sidebar thread tree: the VIEW half. Everything between a built tree and the
// rows the sidebar renders — the depth-first flatten, the preview cut and its
// reveal step, the status rollup, the render-content identity cutoffs, and the
// discussion expansion cleanup. The BUILD half (node shapes, the comparator, the
// builder) is `sidebarTree.ts`; this file imports from it and never the other
// way round.
//
// Pure logic, no Svelte / DOM imports — table-drivable from unit tests.

import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { ThreadStatusPill } from './threadStatusPill';
import {
  isDraftNode,
  sidebarNodePinGroup,
  sidebarNodeRow,
  statusPriority,
  type SidebarThreadTreeNode,
  type SidebarTreeNode,
} from './sidebarTree';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';

export type SidebarTreeVisibleNode = SidebarTreeNode & {
  isExpanded: boolean;
  isExpandable: boolean;
  /** True only on the first top-level back-burner row when both blocks exist. */
  startsBackBurnerBlock: boolean;
  /**
   * The group this row RENDERS INSIDE, or null at the top level (a group's
   * own row included). It is the drop-target identity for the member-row
   * wrapper: the tree knows which group it walked through, where a row's
   * `groupId` field is unverified against what is actually on screen.
   */
  ownerGroupId: string | null;
};

export interface FlattenSidebarThreadTreeInput {
  nodes: readonly SidebarTreeNode[];
  expandedThreadIds: ReadonlySet<string>;
  /**
   * Inverted, unlike `expandedThreadIds`: groups default to EXPANDED, so
   * this holds the ids the user explicitly collapsed.
   */
  collapsedGroupIds?: ReadonlySet<string>;
  /** A closed container shows only this descendant directly beneath it. */
  activeThreadId?: string | null;
}

/**
 * flattenSidebarThreadTree — depth-first walk, descending into a node's
 * children when expanded, or only the focused descendant when collapsed.
 * Returned nodes carry their display depth so the renderer needs no second pass.
 *
 * The two kinds use OPPOSITE defaults, deliberately: a discussion is
 * closed until the user opens it (`expandedThreadIds` lists the open
 * ones), a group is open until the user closes it (`collapsedGroupIds`
 * lists the closed ones), because a group the user just made must show
 * what is in it.
 */
export function flattenSidebarThreadTree(
  input: FlattenSidebarThreadTreeInput,
): SidebarTreeVisibleNode[] {
  const visibleNodes: SidebarTreeVisibleNode[] = [];
  const collapsedGroupIds = input.collapsedGroupIds;

  const visit = (
    node: SidebarTreeNode,
    startsBackBurnerBlock = false,
    ownerGroupId: string | null = null,
  ) => {
    const isExpandable = node.children.length > 0;
    const isExpanded = isExpandable && (
      node.kind === 'group'
        ? collapsedGroupIds === undefined || !collapsedGroupIds.has(node.group.id)
        : input.expandedThreadIds.has(node.thread.id)
    );
    visibleNodes.push({ ...node, isExpanded, isExpandable, startsBackBurnerBlock, ownerGroupId });
    const childOwner = node.kind === 'group' ? node.group.id : ownerGroupId;
    if (!isExpanded) {
      const active = input.activeThreadId
        ? findThreadNode(node.children, input.activeThreadId)
        : null;
      if (active) {
        visibleNodes.push({
          ...active,
          depth: node.depth + 1,
          isExpanded: false,
          isExpandable: false,
          startsBackBurnerBlock: false,
          ownerGroupId: childOwner,
          displayLiveStatus: active.ownLiveStatus,
          displayStatus: active.ownStatus,
        });
      }
      return;
    }
    for (const child of node.children) visit(child, false, childOwner);
  };

  const hasFrontBurner = input.nodes.some((node) => sidebarNodePinGroup(node) === 'front');
  const hasBackBurner = input.nodes.some((node) => sidebarNodePinGroup(node) === 'back');
  let markedBackBurner = false;
  for (const node of input.nodes) {
    const startsBackBurnerBlock = hasFrontBurner
      && hasBackBurner
      && !markedBackBurner
      && sidebarNodePinGroup(node) === 'back';
    if (startsBackBurnerBlock) markedBackBurner = true;
    visit(node, startsBackBurnerBlock);
  }
  return visibleNodes;
}

function findThreadNode(
  nodes: readonly SidebarTreeNode[],
  threadId: string,
): SidebarThreadTreeNode | null {
  for (const node of nodes) {
    if (node.kind === 'thread' && node.thread.id === threadId) return node;
    const found = findThreadNode(node.children, threadId);
    if (found) return found;
  }
  return null;
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
    && a.ringClass === b.ringClass
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
    if (x.kind !== y.kind) return false;
    if (x.kind === 'thread' && y.kind === 'thread' && x.thread !== y.thread) return false;
    if (x.kind === 'group' && y.kind === 'group') {
      if (x.group !== y.group) return false;
      // A collapsed group renders its MEMBER COUNT, and its members are
      // not in this array to be compared — so the count is render input
      // here even though a thread node's child count is not.
      if (x.children.length !== y.children.length) return false;
    }
    if (x.depth !== y.depth) return false;
    if (x.isExpanded !== y.isExpanded || x.isExpandable !== y.isExpandable) return false;
    if (x.startsBackBurnerBlock !== y.startsBackBurnerBlock) return false;
    if (x.ownerGroupId !== y.ownerGroupId) return false;
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

/** Keep the container of an open descendant above the preview cut. */
function nodeHoldsOpenThread(
  node: SidebarTreeNode,
  openThreadIds: ReadonlySet<string>,
): boolean {
  if (node.kind === 'thread' && openThreadIds.has(node.thread.id)) return true;
  return subtreeHoldsOpenThread(node.children, openThreadIds);
}

function subtreeHoldsOpenThread(
  nodes: readonly SidebarTreeNode[],
  openThreadIds: ReadonlySet<string>,
): boolean {
  for (const node of nodes) {
    if (node.kind === 'thread' && openThreadIds.has(node.thread.id)) return true;
    if (subtreeHoldsOpenThread(node.children, openThreadIds)) return true;
  }
  return false;
}

export interface PreviewThreadsResult {
  visibleNodes: SidebarTreeNode[];
  hiddenNodes: SidebarTreeNode[];
}

/**
 * Slice a sorted top-level node list into a preview window. A thread that
 * is open in a pane never hides behind the cut: any that land in the tail
 * float back into view after the head, in tail order. Pinned rows from
 * both burners count toward the limit and always stay visible, even
 * when their count exceeds it. Drafts stay visible outside the limit.
 *
 * A group or discussion takes one slot. Its descendants take none.
 * Containers with an open descendant stay visible above the cut.
 */
export function previewSidebarThreads(input: {
  nodes: readonly SidebarTreeNode[];
  /** Threads mounted in any pane; the cut never hides these. */
  openThreadIds: ReadonlySet<string>;
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
    if (isDraftNode(node)) drafts.push(node);
    else if (sidebarNodeRow(node).pinnedAt != null) pinned.push(node);
    else rest.push(node);
  }

  const unpinnedLimit = Math.max(0, limit - pinned.length);
  const head = rest.slice(0, unpinnedLimit);
  const tail = rest.slice(unpinnedLimit);

  if (tail.length === 0) {
    return { visibleNodes: [...drafts, ...pinned, ...head], hiddenNodes: [] };
  }

  // Open threads in the tail float back into view, in tail order; the rest
  // of the tail stays hidden. An open thread already in drafts / pinned /
  // head is visible as-is.
  const floated: SidebarTreeNode[] = [];
  const hidden: SidebarTreeNode[] = [];
  for (const node of tail) {
    if (nodeHoldsOpenThread(node, input.openThreadIds)) floated.push(node);
    else hidden.push(node);
  }
  return {
    visibleNodes: [...drafts, ...pinned, ...head, ...floated],
    hiddenNodes: hidden,
  };
}

export function nextSidebarThreadRevealLimit(input: {
  nodes: readonly SidebarTreeNode[];
  openThreadIds: ReadonlySet<string>;
  currentLimit: number;
  revealCount: number;
}): number {
  const currentPreview = previewSidebarThreads({
    nodes: input.nodes,
    openThreadIds: input.openThreadIds,
    limit: input.currentLimit,
  });
  const targetHiddenCount = Math.max(0, currentPreview.hiddenNodes.length - input.revealCount);
  let nextLimit = input.currentLimit;
  let nextPreview = currentPreview;

  while (nextPreview.hiddenNodes.length > targetHiddenCount) {
    nextLimit += 1;
    nextPreview = previewSidebarThreads({
      nodes: input.nodes,
      openThreadIds: input.openThreadIds,
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

/** Remove leaf IDs belonging to this tree without changing other projects. */
export function pruneSidebarDiscussionExpansion(input: {
  nodes: readonly SidebarTreeNode[];
  expandedThreadIds: ReadonlySet<string>;
}): ReadonlySet<string> {
  let next: Set<string> | undefined;
  const visit = (nodes: readonly SidebarTreeNode[]) => {
    for (const node of nodes) {
      if (node.kind === 'thread' && node.children.length === 0 && input.expandedThreadIds.has(node.thread.id)) {
        next ??= new Set(input.expandedThreadIds);
        next.delete(node.thread.id);
      }
      visit(node.children);
    }
  };
  visit(input.nodes);
  return next ?? input.expandedThreadIds;
}
