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
  sidebarNodeSection,
  sidebarTreeNodeId,
  displayStatusOutranks,
  type SidebarThreadTreeNode,
  type SidebarTreeNode,
} from './sidebarTree';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';

export type SidebarTreeVisibleNode = SidebarTreeNode & {
  isExpanded: boolean;
  isExpandable: boolean;
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
  /**
   * The row carries a pin affordance: a top-level thread or a group's
   * direct member. A discussion child pins through its parent. Decided from
   * the node's tree depth, which a collapsed container's preview row no
   * longer shows in `depth`.
   */
  isPinTarget: boolean;
};

/**
 * The footer under an expanded group whose members were cut: "Show N More"
 * and "Show Less", the same controls a project's list ends with.
 */
export interface SidebarGroupMoreRow {
  kind: 'group-more';
  groupId: string;
  /** Members behind the cut. */
  hiddenCount: number;
  /** The most important status among the hidden members. */
  hiddenStatus: SidebarStatusRollup | null;
  canShowLess: boolean;
}

/** A row the sidebar renders: a tree node, or a group's show-more footer. */
export type SidebarTreeVisibleRow = SidebarTreeVisibleNode | SidebarGroupMoreRow;

/** Each-block key; a footer's key cannot collide with a node id. */
export function sidebarVisibleRowKey(row: SidebarTreeVisibleRow): string {
  return row.kind === 'group-more' ? `${row.groupId}:more` : sidebarTreeNodeId(row);
}

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
  /** Threads mounted in any pane; a group's preview cut never hides these. */
  openThreadIds: ReadonlySet<string>;
  /** An expanded group's preview limit, by group id. */
  groupVisibleLimit: (groupId: string) => number;
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
 *
 * An expanded group's members take the project's preview cut
 * (`previewSidebarThreads`) against the group's own limit, and a
 * `group-more` footer follows the last member when the cut hid any or the
 * group can be shown less.
 */
export function flattenSidebarThreadTree(
  input: FlattenSidebarThreadTreeInput,
): SidebarTreeVisibleRow[] {
  const visibleNodes: SidebarTreeVisibleRow[] = [];
  const collapsedGroupIds = input.collapsedGroupIds;

  const visit = (
    node: SidebarTreeNode,
    startsSection = false,
    ownerGroupId: string | null = null,
  ) => {
    const isExpandable = node.children.length > 0;
    const isExpanded = isExpandable && (
      node.kind === 'group'
        ? collapsedGroupIds === undefined || !collapsedGroupIds.has(node.group.id)
        : input.expandedThreadIds.has(node.thread.id)
    );
    visibleNodes.push({
      ...node,
      isExpanded,
      isExpandable,
      startsSection,
      ownerGroupId,
      isPinTarget: isPinTarget(node, ownerGroupId),
    });
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
          startsSection: false,
          ownerGroupId: childOwner,
          isPinTarget: isPinTarget(active, childOwner),
          displayLiveStatus: active.ownLiveStatus,
          displayStatus: active.ownStatus,
        });
      }
      return;
    }
    if (node.kind === 'thread') {
      for (const child of node.children) visit(child, false, childOwner);
      return;
    }
    const cut = previewSidebarThreads({
      nodes: node.children,
      openThreadIds: input.openThreadIds,
      limit: input.groupVisibleLimit(node.group.id),
    });
    for (const child of cut.visibleNodes) visit(child, false, childOwner);
    if (cut.hiddenNodes.length > 0 || cut.canShowLess) {
      visibleNodes.push({
        kind: 'group-more',
        groupId: node.group.id,
        hiddenCount: cut.hiddenNodes.length,
        hiddenStatus: rollupDisplayStatus(cut.hiddenNodes),
        canShowLess: cut.canShowLess,
      });
    }
  };

  // The top level arrives sorted, so each section is one contiguous run.
  // Drafts sit above every section and never open one.
  let previousSection: ReturnType<typeof sidebarNodeSection> = null;
  for (const node of input.nodes) {
    const section = sidebarNodeSection(node);
    const startsSection = section !== null && previousSection !== null && section !== previousSection;
    if (section !== null) previousSection = section;
    visit(node, startsSection);
  }
  return visibleNodes;
}

function isPinTarget(node: SidebarTreeNode, ownerGroupId: string | null): boolean {
  if (node.kind !== 'thread') return false;
  return node.depth === 0 || (node.depth === 1 && ownerGroupId !== null);
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

function sameStatusRollup(
  a: SidebarStatusRollup | null,
  b: SidebarStatusRollup | null,
): boolean {
  if (a === b) return true;
  if (a === null || b === null) return false;
  return a.liveStatus === b.liveStatus && sameThreadStatusPill(a.pill, b.pill);
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
export function sameSidebarVisibleRows(
  a: readonly SidebarTreeVisibleRow[],
  b: readonly SidebarTreeVisibleRow[],
): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    const x = a[i];
    const y = b[i];
    if (x.kind === 'group-more' || y.kind === 'group-more') {
      if (x.kind !== 'group-more' || y.kind !== 'group-more') return false;
      if (x.groupId !== y.groupId || x.hiddenCount !== y.hiddenCount) return false;
      if (x.canShowLess !== y.canShowLess) return false;
      if (!sameStatusRollup(x.hiddenStatus, y.hiddenStatus)) return false;
      continue;
    }
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
    if (x.startsSection !== y.startsSection) return false;
    if (x.ownerGroupId !== y.ownerGroupId) return false;
    if (x.isPinTarget !== y.isPinTarget) return false;
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
  /** The list was revealed past the default preview and is longer than it. */
  canShowLess: boolean;
}

/**
 * Slice a sorted top-level node list into a preview window. A thread that
 * is open in a pane never hides behind the cut: any that land in the tail
 * float back into view after the head, in tail order. Groups and pinned
 * rows from both burners sort above the unpinned section, count toward
 * the limit and always stay visible, even when their count exceeds it.
 * Drafts stay visible outside the limit.
 *
 * A group or discussion takes one slot. Its descendants take none.
 * Discussions with an open descendant stay visible above the cut.
 *
 * The same cut applies to a project's top level and to an expanded
 * group's members, each against its own limit.
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

  const canShowLess = limit > THREAD_PREVIEW_LIMIT && input.nodes.length > THREAD_PREVIEW_LIMIT;
  const unpinnedLimit = Math.max(0, limit - pinned.length);
  const head = rest.slice(0, unpinnedLimit);
  const tail = rest.slice(unpinnedLimit);

  if (tail.length === 0) {
    return { visibleNodes: [...drafts, ...pinned, ...head], hiddenNodes: [], canShowLess };
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
    canShowLess,
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

export interface SidebarStatusRollup {
  liveStatus: ThreadLiveStatus;
  pill: ThreadStatusPill;
}

/**
 * Roll up the most-important display status across a list of nodes —
 * used both for the "Show more" hidden-status pill and the per-project
 * status dot when the project is collapsed.
 */
export function rollupDisplayStatus(
  nodes: readonly SidebarTreeNode[],
): SidebarStatusRollup | null {
  let best: SidebarTreeNode | null = null;
  let bestPill: ThreadStatusPill | null = null;
  for (const node of nodes) {
    if (node.displayStatus === null) continue;
    if (best === null || displayStatusOutranks(node, best)) {
      best = node;
      bestPill = node.displayStatus;
    }
  }
  if (best === null || bestPill === null) return null;
  return { liveStatus: best.displayLiveStatus, pill: bestPill };
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
