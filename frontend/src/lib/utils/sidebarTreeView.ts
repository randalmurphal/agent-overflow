// Sidebar thread tree: the VIEW half. Everything between a built tree and the
// rows the sidebar renders — the depth-first flatten, the preview cut and its
// reveal step, the status rollup, the render-content identity cutoffs, and the
// active-thread expand sync. The BUILD half (node shapes, the comparator, the
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
  type SidebarTreeNode,
} from './sidebarTree';
import { THREAD_PREVIEW_LIMIT } from './sidebarThreadLimits';
import { reportFrontendDiagnostic } from './frontendErrorCapture';

const EMPTY_ID_SET: ReadonlySet<string> = new Set<string>();

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
}

/**
 * flattenSidebarThreadTree — depth-first walk, descending into a node's
 * children only when it is expanded. Returned nodes carry their depth so
 * the renderer can indent without a second pass.
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
    if (!isExpanded) return;
    const childOwner = node.kind === 'group' ? node.group.id : ownerGroupId;
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

/**
 * Does the cut have to keep this row? A thread node answers for ITSELF
 * only — that is the existing rule and a discussion child keeps its own
 * top-level parent's row either way. A group answers for its whole
 * subtree, because its members render nowhere else.
 */
function nodeHoldsOpenThread(
  node: SidebarTreeNode,
  openThreadIds: ReadonlySet<string>,
): boolean {
  if (node.kind === 'thread') return openThreadIds.has(node.thread.id);
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
 * float back into view after the head, in tail order (t3-code's "6 +
 * active" generalised to every pane, since the focused pane's thread is
 * one of them). Pinned rows always stay visible — they don't consume
 * preview slots; the limit only truncates the unpinned tail.
 *
 * A group is ONE slot and its members are none, the same way a discussion
 * parent is. A group floats when any thread in it is open, because a
 * member has no row of its own outside its group — hiding the group would
 * hide the open thread entirely.
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

  const head = rest.slice(0, limit);
  const tail = rest.slice(limit);

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

export interface SyncExpandedTreeResult {
  expandedThreadIds: Set<string>;
  /** The input set itself when nothing had to change — callers compare by
   *  content, so the common path allocates nothing. */
  collapsedGroupIds: ReadonlySet<string>;
}

/**
 * Auto-expand the chain of ancestors leading to the active thread so
 * the active row is always visible in the rendered tree. Drops any
 * expanded ids that no longer correspond to expandable nodes (a child
 * was deleted, parent is now leaf), and un-collapses the group holding
 * the active thread — a group auto-expands for its member the way a
 * discussion auto-expands for its participant.
 *
 * Both persisted sets are global across projects while this runs per
 * project, so neither may drop an id this tree does not contain: the
 * collapsed-group set is not pruned at all, and the expanded set drops
 * only ids that name a thread of THIS tree which is no longer expandable.
 */
export function syncExpandedTreeForActiveThread(input: {
  nodes: readonly SidebarTreeNode[];
  expandedThreadIds: ReadonlySet<string>;
  collapsedGroupIds?: ReadonlySet<string>;
  activeThreadId: string | null;
}): SyncExpandedTreeResult {
  const expandableIds = new Set<string>();
  // Every thread id this tree renders. `expandedThreadIds` is one set across
  // every project while this runs per project, so an id this tree has never
  // heard of belongs to another project and must survive the prune — without
  // that, two expanded projects prune each other to empty on every pass.
  const knownThreadIds = new Set<string>();
  const parentByThreadId = new Map<string, string | null>();
  // The group each top-level member belongs to, so the active thread can
  // name the one group that must open. Members are the group node's own
  // children — a discussion child of a member reaches its group through
  // the parent walk below.
  const groupIdByThreadId = new Map<string, string>();

  const visit = (node: SidebarTreeNode, groupId: string | null) => {
    if (node.kind === 'group') {
      for (const child of node.children) visit(child, node.group.id);
      return;
    }
    knownThreadIds.add(node.thread.id);
    parentByThreadId.set(node.thread.id, node.thread.parentThreadId ?? null);
    if (groupId !== null) groupIdByThreadId.set(node.thread.id, groupId);
    if (node.children.length > 0) expandableIds.add(node.thread.id);
    for (const child of node.children) visit(child, groupId);
  };
  for (const node of input.nodes) visit(node, null);

  const next = new Set(
    [...input.expandedThreadIds].filter((id) => expandableIds.has(id) || !knownThreadIds.has(id)),
  );

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
      // LocalOnly), so there the console line is the only evidence.
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

  const collapsedGroupIds = input.collapsedGroupIds ?? EMPTY_ID_SET;
  const activeGroupId = input.activeThreadId === null
    ? undefined
    : groupIdByThreadId.get(input.activeThreadId);
  if (activeGroupId === undefined || !collapsedGroupIds.has(activeGroupId)) {
    // Nothing to un-collapse, which is every streaming beat in an expanded
    // project: hand back the input set rather than a copy of it.
    return { expandedThreadIds: next, collapsedGroupIds };
  }
  const nextCollapsed = new Set(collapsedGroupIds);
  nextCollapsed.delete(activeGroupId);
  return { expandedThreadIds: next, collapsedGroupIds: nextCollapsed };
}
