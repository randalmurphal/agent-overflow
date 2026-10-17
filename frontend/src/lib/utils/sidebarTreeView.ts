// Sidebar thread tree: the VIEW half. Everything between a built tree and the
// rows the sidebar renders — the flatten, the preview cut and its reveal step,
// the status rollup and the render-content identity cutoffs. The BUILD half
// (node shapes, the comparator, the builder) is `sidebarTree.ts`; this file
// imports from it and never the other way round.
//
// Pure logic, no Svelte / DOM imports — table-drivable from unit tests.

import type { ThreadLiveStatus } from '../stores/threadStatuses.svelte';
import type { Thread } from '../types/models';
import type { ThreadStatusPill } from './threadStatusPill';
import {
  assistantKey,
  sidebarNodeSection,
  displayRank,
  type SidebarAgentTreeNode,
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
  /** The standing agent this row renders under, or null. */
  ownerAgentId: string | null;
}

/**
 * The `N done` footer under an agent's children: its settled idle children
 * (sidebarChildFolds) and its archived spawned threads, counted together
 * and listed inline when expanded, settled idle first.
 */
export interface SidebarAgentDoneNode extends SidebarVisibleNodePlacement {
  kind: 'agent-done';
  /** The agent whose finished children this footer counts. */
  agent: Thread;
  /** One deeper than the agent row, like its children. */
  depth: number;
  /** Settled idle children plus `archivedChildCount` of the agent's row. */
  count: number;
  expanded: boolean;
  /** True while the archived rows are expanded but not loaded yet. */
  loading: boolean;
  /** Done rows past the footer's own preview limit while expanded. */
  hiddenCount: number;
}

/**
 * The show-more footer under a group's members or an agent's live
 * children once they pass the container bound (boundContainerMembers).
 */
export interface SidebarContainerMoreNode extends SidebarVisibleNodePlacement {
  kind: 'container-more';
  /** The group id or the agent's assistant key: the paging's key. */
  containerKey: string;
  /** One deeper than the container row, like its members. */
  depth: number;
  /** Members the bound hides. */
  hiddenCount: number;
  /** The hidden members' rolled-up status, as the project footer shows it. */
  hiddenStatus: { liveStatus: ThreadLiveStatus; pill: ThreadStatusPill } | null;
}

export type SidebarTreeVisibleNode =
  | (SidebarTreeNode & SidebarVisibleNodePlacement & {
    /**
     * True for an archived spawned thread listed under an agent's done
     * footer: the row renders muted, and it is not in the live list.
     */
    archived?: boolean;
    /**
     * True for a settled idle child listed under an agent's open done
     * footer: a live row, shown in the done list rather than the live rows.
     */
    folded?: boolean;
  })
  | SidebarAgentDoneNode
  | SidebarContainerMoreNode;

export interface FlattenSidebarThreadTreeInput {
  nodes: readonly SidebarTreeNode[];
  /**
   * Groups default to EXPANDED, so this holds the ids the user explicitly
   * collapsed: a group the user just made must show what is in it.
   */
  collapsedGroupIds?: ReadonlySet<string>;
  /** Agents default to EXPANDED too; the thread ids the user folded. */
  collapsedAgentIds?: ReadonlySet<string>;
  /** A collapsed group or agent shows only this member directly beneath it. */
  activeThreadId?: string | null;
  /** Agents whose done footer the user opened. */
  expandedDoneAgentIds?: ReadonlySet<string>;
  /**
   * The archived spawned threads of an agent, newest first, or null while
   * they are still loading. Read only for an expanded done footer.
   */
  archivedChildrenOf?: (agent: Thread) => readonly Thread[] | null;
  /** How many done rows an expanded footer shows; THREAD_PREVIEW_LIMIT by default. */
  doneLimitOf?: (agentId: string) => number;
  /**
   * Threads mounted in any pane. A child open in a pane never folds into
   * the done footer, and no member open in a pane hides behind a bound.
   */
  openThreadIds?: ReadonlySet<string>;
  /**
   * How many unpinned members a group (by id) or an agent (by assistant
   * key) shows before its show-more footer; THREAD_PREVIEW_LIMIT by default.
   */
  memberLimitOf?: (containerKey: string) => number;
}

/**
 * Whether an agent's child folds into the done footer
 * (docs/specs/standing-agents.md ruling 5): effective status idle with no
 * pill (so read, and no Failed, Interrupted, Plan Ready or Setup Failed),
 * not pinned, and not open in a pane or focused.
 */
export function sidebarChildFolds(
  child: SidebarThreadTreeNode,
  openThreadIds: ReadonlySet<string> | undefined,
  activeThreadId: string | null | undefined,
): boolean {
  if (child.ownLiveStatus !== 'idle' || child.ownStatus !== null) return false;
  if (child.thread.pinnedAt != null) return false;
  if (child.thread.id === activeThreadId) return false;
  return openThreadIds?.has(child.thread.id) !== true;
}

function foldedChildCount(
  node: SidebarAgentTreeNode,
  openThreadIds: ReadonlySet<string> | undefined,
  activeThreadId: string | null | undefined,
): number {
  let folded = 0;
  for (const child of node.children) {
    if (sidebarChildFolds(child, openThreadIds, activeThreadId)) folded += 1;
  }
  return folded;
}

/** An agent's done footer count: its settled idle children plus its archived ones. */
export function agentDoneCount(
  node: SidebarAgentTreeNode,
  openThreadIds: ReadonlySet<string> | undefined,
  activeThreadId: string | null | undefined,
): number {
  return foldedChildCount(node, openThreadIds, activeThreadId) + (node.thread.archivedChildCount ?? 0);
}

/** The count a collapsed agent row shows: its live children, not the done ones. */
export function agentLiveChildCount(
  node: SidebarAgentTreeNode,
  openThreadIds: ReadonlySet<string> | undefined,
  activeThreadId: string | null | undefined,
): number {
  return node.children.length - foldedChildCount(node, openThreadIds, activeThreadId);
}

export interface BoundContainerResult {
  /** The members shown, in their sorted order. */
  shown: SidebarThreadTreeNode[];
  /** The members the bound hides, in their sorted order. */
  hidden: SidebarThreadTreeNode[];
}

/**
 * The one container bound, for a group's members and an agent's live
 * children (ruling 32): pinned members always show and take no slot, the
 * first `limit` unpinned members show, and a member open in a pane past
 * the cut shows too. Members arrive sorted, so pinned ones lead.
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

/** A row for an archived spawned thread: no live status, never a sort input. */
function archivedChildNode(thread: Thread): SidebarThreadTreeNode {
  return {
    kind: 'thread',
    thread,
    depth: 1,
    ownLiveStatus: 'idle',
    ownStatus: null,
    displayLiveStatus: 'idle',
    displayStatus: null,
    sortGroup: 'idle',
    normalSortGroup: 'idle',
    latestActivityAt: thread.updatedAt ?? 0,
  };
}

/**
 * flattenSidebarThreadTree — emit each top-level row, followed by a group's
 * or agent's members when it is expanded, or only its focused member when
 * collapsed. An expanded container shows its members up to its bound, then
 * a show-more footer for the rest. An expanded agent's settled idle
 * children leave its live rows for its done footer, which ends the agent's
 * rows with its archived children; while the footer is open the done rows,
 * settled idle first, precede it.
 */
export function flattenSidebarThreadTree(
  input: FlattenSidebarThreadTreeInput,
): SidebarTreeVisibleNode[] {
  const visibleNodes: SidebarTreeVisibleNode[] = [];
  const collapsedGroupIds = input.collapsedGroupIds;
  const collapsedAgentIds = input.collapsedAgentIds;

  // The top level arrives sorted, so each section is one contiguous run.
  // Drafts sit above every section and never open one.
  let previousSection: ReturnType<typeof sidebarNodeSection> = null;
  for (const node of input.nodes) {
    const section = sidebarNodeSection(node);
    const startsSection = section !== null && previousSection !== null && section !== previousSection;
    if (section !== null) previousSection = section;
    visibleNodes.push({ ...node, startsSection, ownerGroupId: null, ownerAgentId: null });
    if (node.kind === 'thread') continue;
    if (node.kind === 'group') {
      const groupId = node.group.id;
      const collapsed = collapsedGroupIds !== undefined && collapsedGroupIds.has(groupId);
      if (collapsed) {
        for (const member of node.children) {
          if (member.thread.id !== input.activeThreadId) continue;
          visibleNodes.push({ ...member, startsSection: false, ownerGroupId: groupId, ownerAgentId: null });
        }
        continue;
      }
      pushBounded(visibleNodes, input, node.children, groupId, node.depth + 1, { ownerGroupId: groupId, ownerAgentId: null });
      continue;
    }
    const agentId = assistantKey(node.thread);
    const collapsed = collapsedAgentIds !== undefined && collapsedAgentIds.has(agentId);
    const placement = { ownerGroupId: null, ownerAgentId: agentId };
    if (collapsed) {
      for (const child of node.children) {
        if (child.thread.id !== input.activeThreadId) continue;
        visibleNodes.push({ ...child, startsSection: false, ...placement });
      }
      continue;
    }
    const live: SidebarThreadTreeNode[] = [];
    const folded: SidebarThreadTreeNode[] = [];
    for (const child of node.children) {
      if (sidebarChildFolds(child, input.openThreadIds, input.activeThreadId)) folded.push(child);
      else live.push(child);
    }
    pushBounded(visibleNodes, input, live, agentId, node.depth + 1, placement);
    const archivedCount = node.thread.archivedChildCount ?? 0;
    const count = folded.length + archivedCount;
    if (count === 0) continue;
    const expanded = input.expandedDoneAgentIds?.has(agentId) === true;
    let loading = false;
    let hiddenCount = 0;
    if (expanded) {
      // Settled idle first, newest first; then the archived ones, which
      // arrive newest first. A thread a handoff just superseded can be in
      // the archived list before its row leaves the live list; it shows
      // once, as the live row.
      const done: Array<{ row: SidebarThreadTreeNode; archived: boolean }> = [...folded]
        .sort((a, b) => b.latestActivityAt - a.latestActivityAt
          || (a.thread.id < b.thread.id ? 1 : a.thread.id > b.thread.id ? -1 : 0))
        .map((row) => ({ row, archived: false }));
      if (archivedCount > 0) {
        const loaded = input.archivedChildrenOf?.(node.thread) ?? null;
        if (loaded === null) {
          loading = true;
        } else {
          const children = new Set(node.children.map((child) => child.thread.id));
          for (const thread of loaded) {
            if (!children.has(thread.id)) done.push({ row: archivedChildNode(thread), archived: true });
          }
        }
      }
      const limit = input.doneLimitOf?.(agentId) ?? THREAD_PREVIEW_LIMIT;
      const shown = done.slice(0, limit);
      hiddenCount = Math.max(0, done.length - shown.length);
      for (const { row, archived } of shown) {
        visibleNodes.push({ ...row, startsSection: false, ...placement, ...(archived ? { archived: true } : { folded: true }) });
      }
    }
    visibleNodes.push({
      kind: 'agent-done', agent: node.thread, depth: node.depth + 1, count, expanded, loading, hiddenCount,
      startsSection: false, ...placement,
    });
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
  placement: { ownerGroupId: string | null; ownerAgentId: string | null },
): void {
  const limit = input.memberLimitOf?.(containerKey) ?? THREAD_PREVIEW_LIMIT;
  const { shown, hidden } = boundContainerMembers(members, limit, input.openThreadIds);
  for (const member of shown) visibleNodes.push({ ...member, startsSection: false, ...placement });
  if (hidden.length === 0) return;
  visibleNodes.push({
    kind: 'container-more', containerKey, depth, hiddenCount: hidden.length,
    hiddenStatus: rollupDisplayStatus(hidden), startsSection: false, ...placement,
  });
}

/** The each-block key of a visible row; a done footer keys on its agent. */
export function sidebarVisibleNodeId(node: SidebarTreeVisibleNode): string {
  if (node.kind === 'agent-done') return `agent:${assistantKey(node.agent)}:done`;
  if (node.kind === 'container-more') return `more:${node.containerKey}`;
  if (node.kind === 'agent') return `agent:${assistantKey(node.thread)}`;
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
    if (x.startsSection !== y.startsSection) return false;
    if (x.ownerGroupId !== y.ownerGroupId) return false;
    if (x.ownerAgentId !== y.ownerAgentId) return false;
    if (x.kind === 'container-more' || y.kind === 'container-more') {
      if (x.kind !== 'container-more' || y.kind !== 'container-more') return false;
      if (x.containerKey !== y.containerKey || x.hiddenCount !== y.hiddenCount || x.depth !== y.depth) return false;
      if ((x.hiddenStatus === null) !== (y.hiddenStatus === null)) return false;
      if (x.hiddenStatus !== null && y.hiddenStatus !== null && (x.hiddenStatus.liveStatus !== y.hiddenStatus.liveStatus
        || !sameThreadStatusPill(x.hiddenStatus.pill, y.hiddenStatus.pill))) return false;
      continue;
    }
    if (x.kind === 'agent-done' || y.kind === 'agent-done') {
      if (x.kind !== 'agent-done' || y.kind !== 'agent-done') return false;
      if (x.agent !== y.agent || x.count !== y.count || x.expanded !== y.expanded
        || x.loading !== y.loading || x.hiddenCount !== y.hiddenCount) return false;
      continue;
    }
    if (x.kind === 'thread' && y.kind === 'thread') {
      if (x.thread !== y.thread || x.archived !== y.archived || x.folded !== y.folded) return false;
    }
    if (x.kind === 'group' && y.kind === 'group') {
      if (x.group !== y.group) return false;
      // A collapsed group renders its MEMBER COUNT, and its members are
      // not in this array to be compared — so the count is render input
      // here even though a thread node's child count is not.
      if (x.children.length !== y.children.length) return false;
    }
    if (x.kind === 'agent' && y.kind === 'agent') {
      // The agent row renders its thread (name, state, done count) and,
      // collapsed, its child count.
      if (x.thread !== y.thread || x.children.length !== y.children.length) return false;
    }
    if (x.depth !== y.depth) return false;
    if (x.ownLiveStatus !== y.ownLiveStatus || x.displayLiveStatus !== y.displayLiveStatus) return false;
    if (!sameThreadStatusPill(x.ownStatus, y.ownStatus)) return false;
    if (!sameThreadStatusPill(x.displayStatus, y.displayStatus)) return false;
  }
  return true;
}

/** An open thread, or a group or agent holding one, stays above the preview cut. */
function nodeHoldsOpenThread(
  node: SidebarTreeNode,
  openThreadIds: ReadonlySet<string>,
): boolean {
  if (node.kind === 'thread') return openThreadIds.has(node.thread.id);
  if (node.kind === 'agent' && openThreadIds.has(node.thread.id)) return true;
  return node.children.some((member) => openThreadIds.has(member.thread.id));
}

export interface PreviewThreadsResult {
  visibleNodes: SidebarTreeNode[];
  hiddenNodes: SidebarTreeNode[];
}

/**
 * Slice a sorted top-level node list into a preview window. A thread that
 * is open in a pane never hides behind the cut: any that land in the tail
 * float back into view after the head, in tail order. Groups, agents and
 * pinned rows from both burners sort above the unpinned section, count
 * toward the limit and always stay visible, even when their count exceeds
 * it. Drafts stay visible outside the limit.
 *
 * A group or agent takes one slot. Its members take none.
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
 * row shows its dot: the "Show more" hidden-status dot of a project, group
 * or agent, and the per-project status dot when the project is collapsed.
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
