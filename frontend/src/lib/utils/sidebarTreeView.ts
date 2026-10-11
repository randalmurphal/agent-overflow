// Sidebar thread tree: the VIEW half. Everything between a built tree and the
// rows the sidebar renders — the flatten, the preview cut and its reveal step,
// the status rollup and the render-content identity cutoffs. The BUILD half
// (node shapes, the comparator, the builder) is `sidebarTree.ts`; this file
// imports from it and never the other way round.
//
// Pure logic, no Svelte / DOM imports — table-drivable from unit tests.

import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { ThreadStatusPill } from './threadStatusPill';
import {
  sidebarNodeSection,
  statusPriority,
  type SidebarTreeNode,
} from './sidebarTree';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';

export type SidebarTreeVisibleNode = SidebarTreeNode & {
  /**
   * True on the first top-level row of a section (groups, front burner,
   * back burner, unpinned) when an earlier section has rows: the renderer
   * draws the section divider above it.
   */
  startsSection: boolean;
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
  /**
   * Groups default to EXPANDED, so this holds the ids the user explicitly
   * collapsed: a group the user just made must show what is in it.
   */
  collapsedGroupIds?: ReadonlySet<string>;
  /** A collapsed group shows only this member directly beneath it. */
  activeThreadId?: string | null;
}

/**
 * flattenSidebarThreadTree — emit each top-level row, followed by a group's
 * members when it is expanded, or only its focused member when collapsed.
 */
export function flattenSidebarThreadTree(
  input: FlattenSidebarThreadTreeInput,
): SidebarTreeVisibleNode[] {
  const visibleNodes: SidebarTreeVisibleNode[] = [];
  const collapsedGroupIds = input.collapsedGroupIds;

  // The top level arrives sorted, so each section is one contiguous run.
  // Drafts sit above every section and never open one.
  let previousSection: ReturnType<typeof sidebarNodeSection> = null;
  for (const node of input.nodes) {
    const section = sidebarNodeSection(node);
    const startsSection = section !== null && previousSection !== null && section !== previousSection;
    if (section !== null) previousSection = section;
    visibleNodes.push({ ...node, startsSection, ownerGroupId: null });
    if (node.kind === 'thread') continue;
    const groupId = node.group.id;
    const collapsed = collapsedGroupIds !== undefined && collapsedGroupIds.has(groupId);
    const members = collapsed
      ? node.children.filter((member) => member.thread.id === input.activeThreadId)
      : node.children;
    for (const member of members) {
      visibleNodes.push({ ...member, startsSection: false, ownerGroupId: groupId });
    }
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
    if (x.startsSection !== y.startsSection) return false;
    if (x.ownerGroupId !== y.ownerGroupId) return false;
    if (x.ownLiveStatus !== y.ownLiveStatus || x.displayLiveStatus !== y.displayLiveStatus) return false;
    if (!sameThreadStatusPill(x.ownStatus, y.ownStatus)) return false;
    if (!sameThreadStatusPill(x.displayStatus, y.displayStatus)) return false;
  }
  return true;
}

/** An open thread, or a group holding one, stays above the preview cut. */
function nodeHoldsOpenThread(
  node: SidebarTreeNode,
  openThreadIds: ReadonlySet<string>,
): boolean {
  if (node.kind === 'thread') return openThreadIds.has(node.thread.id);
  return node.children.some((member) => openThreadIds.has(member.thread.id));
}

export interface PreviewThreadsResult {
  visibleNodes: SidebarTreeNode[];
  hiddenNodes: SidebarTreeNode[];
}

/**
 * Slice a sorted top-level node list into a preview window. A thread that
 * is open in a pane never hides behind the cut: any that land in the tail
 * float back into view after the head, in tail order. Groups and pinned
 * rows from both burners sort above the unpinned section, count toward
 * the limit and always stay visible, even when their count exceeds it.
 * Drafts stay visible outside the limit.
 *
 * A group takes one slot. Its members take none.
 */
export function previewSidebarThreads(input: {
  nodes: readonly SidebarTreeNode[];
  /** Threads mounted in any pane; the cut never hides these. */
  openThreadIds: ReadonlySet<string>;
  limit?: number;
}): PreviewThreadsResult {
  const limit = input.limit ?? THREAD_PREVIEW_LIMIT;

  // Drafts, groups and pins all render outside the truncated unpinned
  // tail, in the order compareTreeNodes gives them.
  const drafts: SidebarTreeNode[] = [];
  const pinned: SidebarTreeNode[] = [];
  const rest: SidebarTreeNode[] = [];
  for (const node of input.nodes) {
    const section = sidebarNodeSection(node);
    if (section === null) drafts.push(node);
    else if (section === 'unpinned') rest.push(node);
    else pinned.push(node);
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
