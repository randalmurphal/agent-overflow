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
  statusPriority,
  type SidebarThreadTreeNode,
  type SidebarTreeNode,
} from './sidebarTree';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';

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
 * The `N done` footer under an agent's children: the agent's archived
 * spawned threads, counted on its row and listed inline when expanded.
 */
export interface SidebarAgentDoneNode extends SidebarVisibleNodePlacement {
  kind: 'agent-done';
  /** The agent whose archived children this footer counts. */
  agent: Thread;
  /** One deeper than the agent row, like its children. */
  depth: number;
  /** `archivedChildCount` of the agent's row. */
  count: number;
  expanded: boolean;
  /** True while the archived rows are expanded but not loaded yet. */
  loading: boolean;
  /** Archived rows past the footer's own preview limit while expanded. */
  hiddenCount: number;
}

export type SidebarTreeVisibleNode =
  | (SidebarTreeNode & SidebarVisibleNodePlacement & {
    /**
     * True for an archived spawned thread listed under an agent's done
     * footer: the row renders muted, and it is not in the live list.
     */
    archived?: boolean;
  })
  | SidebarAgentDoneNode;

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
  /** How many archived rows an expanded footer shows; THREAD_PREVIEW_LIMIT by default. */
  doneLimitOf?: (agentId: string) => number;
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
 * collapsed. An expanded agent with archived children ends with its done
 * footer, preceded by the archived rows while the footer is open.
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
      const members = collapsed
        ? node.children.filter((member) => member.thread.id === input.activeThreadId)
        : node.children;
      for (const member of members) {
        visibleNodes.push({ ...member, startsSection: false, ownerGroupId: groupId, ownerAgentId: null });
      }
      continue;
    }
    const agentId = assistantKey(node.thread);
    const collapsed = collapsedAgentIds !== undefined && collapsedAgentIds.has(agentId);
    const children = collapsed
      ? node.children.filter((child) => child.thread.id === input.activeThreadId)
      : node.children;
    for (const child of children) {
      visibleNodes.push({ ...child, startsSection: false, ownerGroupId: null, ownerAgentId: agentId });
    }
    const count = node.thread.archivedChildCount ?? 0;
    if (collapsed || count === 0) continue;
    const expanded = input.expandedDoneAgentIds?.has(agentId) === true;
    let loading = false;
    let hiddenCount = 0;
    if (expanded) {
      const archived = input.archivedChildrenOf?.(node.thread) ?? null;
      if (archived === null) {
        loading = true;
      } else {
        const limit = input.doneLimitOf?.(agentId) ?? THREAD_PREVIEW_LIMIT;
        const shown = archived.slice(0, limit);
        hiddenCount = Math.max(0, archived.length - shown.length);
        for (const thread of shown) {
          visibleNodes.push({
            ...archivedChildNode(thread), startsSection: false, ownerGroupId: null, ownerAgentId: agentId, archived: true,
          });
        }
      }
    }
    visibleNodes.push({
      kind: 'agent-done', agent: node.thread, depth: node.depth + 1, count, expanded, loading, hiddenCount,
      startsSection: false, ownerGroupId: null, ownerAgentId: agentId,
    });
  }
  return visibleNodes;
}

/** The each-block key of a visible row; a done footer keys on its agent. */
export function sidebarVisibleNodeId(node: SidebarTreeVisibleNode): string {
  if (node.kind === 'agent-done') return `agent:${assistantKey(node.agent)}:done`;
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
    if (x.kind === 'agent-done' || y.kind === 'agent-done') {
      if (x.kind !== 'agent-done' || y.kind !== 'agent-done') return false;
      if (x.agent !== y.agent || x.count !== y.count || x.expanded !== y.expanded
        || x.loading !== y.loading || x.hiddenCount !== y.hiddenCount) return false;
      continue;
    }
    if (x.kind === 'thread' && y.kind === 'thread') {
      if (x.thread !== y.thread || x.archived !== y.archived) return false;
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
