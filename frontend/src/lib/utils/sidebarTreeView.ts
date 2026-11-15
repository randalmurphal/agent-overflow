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
  displayRank,
  type SidebarThreadTreeNode,
  type SidebarTreeNode,
} from './sidebarTree';
import { THREAD_PREVIEW_LIMIT, THREAD_REVEAL_INCREMENT } from './sidebarThreadLimits';

interface SidebarVisibleNodePlacement {
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
}

/**
 * The show-more footer under a group's members once they pass the
 * container bound (boundContainerMembers).
 */
export interface SidebarContainerMoreNode extends SidebarVisibleNodePlacement {
  kind: 'container-more';
  /** The group id: the paging's key. */
  containerKey: string;
  /** One deeper than the container row, like its members. */
  depth: number;
  /** Members the bound hides. */
  hiddenCount: number;
  /** The hidden members' rolled-up status, as the project footer shows it. */
  hiddenStatus: { liveStatus: ThreadLiveStatus; pill: ThreadStatusPill } | null;
}

export type SidebarTreeVisibleNode =
  | (SidebarTreeNode & SidebarVisibleNodePlacement)
  | SidebarContainerMoreNode;

export interface FlattenSidebarThreadTreeInput {
  nodes: readonly SidebarTreeNode[];
  /**
   * Groups default to EXPANDED, so this holds the ids the user explicitly
   * collapsed: a group the user just made must show what is in it.
   */
  collapsedGroupIds?: ReadonlySet<string>;
  /** A collapsed group shows only this member directly beneath it. */
  activeThreadId?: string | null;
  /** Threads mounted in any pane: no member open in a pane hides behind a bound. */
  openThreadIds?: ReadonlySet<string>;
  /**
   * How many unpinned members a group (by id) shows before its show-more
   * footer; THREAD_PREVIEW_LIMIT by default.
   */
  memberLimitOf?: (containerKey: string) => number;
}

export interface BoundContainerResult {
  /** The members shown, in their sorted order. */
  shown: SidebarThreadTreeNode[];
  /** The members the bound hides, in their sorted order. */
  hidden: SidebarThreadTreeNode[];
}

/**
 * The container bound of a group's members (ruling 32): pinned members
 * always show and take no slot, the first `limit` unpinned members show,
 * and a member open in a pane past the cut shows too. Members arrive
 * sorted, so pinned ones lead.
 */
export function boundContainerMembers(
  members: readonly SidebarThreadTreeNode[],
  limit: number,
  openThreadIds: ReadonlySet<string> | undefined,
): BoundContainerResult {
  const shown: SidebarThreadTreeNode[] = [];
  const hidden: SidebarThreadTreeNode[] = [];
  let unpinned = 0;
  for (const member of members) {
    if (member.thread.pinnedAt != null) {
      shown.push(member);
      continue;
    }
    unpinned += 1;
    if (unpinned <= limit || openThreadIds?.has(member.thread.id) === true) shown.push(member);
    else hidden.push(member);
  }
  return { shown, hidden };
}

/**
 * The limit that reveals the next THREAD_REVEAL_INCREMENT hidden members,
 * or all of them when fewer remain: a member open in a pane already shows,
 * so the limit grows past it rather than by a fixed step.
 */
export function nextContainerRevealLimit(
  members: readonly SidebarThreadTreeNode[],
  openThreadIds: ReadonlySet<string> | undefined,
  currentLimit: number,
): number {
  const hidden = boundContainerMembers(members, currentLimit, openThreadIds).hidden.length;
  const target = Math.max(0, hidden - THREAD_REVEAL_INCREMENT);
  let next = currentLimit;
  while (boundContainerMembers(members, next, openThreadIds).hidden.length > target) next += 1;
  return next;
}

/**
 * flattenSidebarThreadTree — emit each top-level row, followed by a group's
 * members when it is expanded, or only its focused member when collapsed.
 * An expanded group shows its members up to its bound, then a show-more
 * footer for the rest.
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
    if (collapsed) {
      for (const member of node.children) {
        if (member.thread.id !== input.activeThreadId) continue;
        visibleNodes.push({ ...member, startsSection: false, ownerGroupId: groupId });
      }
      continue;
    }
    pushBounded(visibleNodes, input, node.children, groupId, node.depth + 1);
  }
  return visibleNodes;
}

/** Emit a container's members up to its bound, then its show-more footer. */
function pushBounded(
  visibleNodes: SidebarTreeVisibleNode[],
  input: FlattenSidebarThreadTreeInput,
  members: readonly SidebarThreadTreeNode[],
  containerKey: string,
  depth: number,
): void {
  const limit = input.memberLimitOf?.(containerKey) ?? THREAD_PREVIEW_LIMIT;
  const { shown, hidden } = boundContainerMembers(members, limit, input.openThreadIds);
  for (const member of shown) visibleNodes.push({ ...member, startsSection: false, ownerGroupId: containerKey });
  if (hidden.length === 0) return;
  visibleNodes.push({
    kind: 'container-more', containerKey, depth, hiddenCount: hidden.length,
    hiddenStatus: rollupDisplayStatus(hidden), startsSection: false, ownerGroupId: containerKey,
  });
}

/** The each-block key of a visible row. */
export function sidebarVisibleNodeId(node: SidebarTreeVisibleNode): string {
  if (node.kind === 'container-more') return `more:${node.containerKey}`;
  return node.kind === 'group' ? node.group.id : node.thread.id;
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
 * SidebarTreeList derived returns its PREVIOUS array when this holds,
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
    if (x.startsSection !== y.startsSection) return false;
    if (x.ownerGroupId !== y.ownerGroupId) return false;
    if (x.kind === 'container-more' || y.kind === 'container-more') {
      if (x.kind !== 'container-more' || y.kind !== 'container-more') return false;
      if (x.containerKey !== y.containerKey || x.hiddenCount !== y.hiddenCount || x.depth !== y.depth) return false;
      if ((x.hiddenStatus === null) !== (y.hiddenStatus === null)) return false;
      if (x.hiddenStatus !== null && y.hiddenStatus !== null && (x.hiddenStatus.liveStatus !== y.hiddenStatus.liveStatus
        || !sameThreadStatusPill(x.hiddenStatus.pill, y.hiddenStatus.pill))) return false;
      continue;
    }
    if (x.kind === 'thread' && y.kind === 'thread') {
      if (x.thread !== y.thread) return false;
    }
    if (x.kind === 'group' && y.kind === 'group') {
      if (x.group !== y.group) return false;
      // A collapsed group renders its MEMBER COUNT, and its members are
      // not in this array to be compared — so the count is render input
      // here even though a thread node's child count is not.
      if (x.children.length !== y.children.length) return false;
    }
    if (x.depth !== y.depth) return false;
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
 * float back into view after the head, in tail order. Groups and
 * pinned rows from both burners sort above the unpinned section, count
 * toward the limit and always stay visible, even when their count exceeds
 * it. Drafts stay visible outside the limit.
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
 * Roll up the most important display status across a list of nodes, ranked
 * as a container row ranks its members (displayRank), so an unread hidden
 * row shows its dot: the "Show more" hidden-status dot of a project or
 * group, and the per-project status dot when the project is collapsed.
 */
export function rollupDisplayStatus(
  nodes: readonly SidebarTreeNode[],
): { liveStatus: ThreadLiveStatus; pill: ThreadStatusPill } | null {
  let best: { liveStatus: ThreadLiveStatus; pill: ThreadStatusPill } | null = null;
  let bestRank = -1;
  for (const node of nodes) {
    if (node.displayStatus == null) continue;
    const rank = displayRank(node);
    if (rank > bestRank) {
      best = { liveStatus: node.displayLiveStatus, pill: node.displayStatus };
      bestRank = rank;
    }
  }
  return best;
}
