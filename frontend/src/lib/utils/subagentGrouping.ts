// Pure projection utility for turning a flat timeline of Items into stable
// transcript nodes. The only structural grouping allowed here is Claude
// inline Agent/Task tool calls. Generic parentId nesting is deliberately not
// used because it can make an already-rendered row flip from leaf -> group.
//
// Contract:
//   - Input: a chronologically-ordered list of Items (preserves turnIndex
//     / itemIndex order — callers do not need to pre-sort).
//   - Output: an array of TimelineNode roots. A `group` node wraps a parent
//     item plus recursively-grouped children. A `leaf` node wraps a single
//     item with nothing under it. A `inline_subagent_group` node is a
//     structural Claude inline-agent wrapper whose children are real
//     subagent groups.
//
// Rules:
//   - Normal rows always stay leaves.
//   - Claude inline Agent/Task launch rows are groups from first render, even
//     before any child activity arrives.
//   - Adjacent inline agents from the same Claude assistant message share a
//     structural wrapper row keyed by that assistant message. The real agent
//     cards remain peers inside the wrapper.
//   - parentId children are nested only when their parent is one of those
//     inline-agent launch rows. Children of non-agent parents stay flat.
//   - Nesting is capped at MAX_DEPTH (3, matching forge). Descendants
//     beyond that depth collapse upward into their deepest allowed group
//     as leaf siblings.
//   - Each group surfaces the most recent (turnIndex, itemIndex) descendant
//     summary as `latestChildSummary` — the SubagentGroup card uses this
//     for its collapsed-header preview so the UI tracks "what the subagent
//     is doing right now" rather than concatenating completed history.
//     Running/streaming descendants win over terminal ones.
//
// The grouping function is pure — no mutation of inputs, no side effects.

import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

export const MAX_DEPTH = 3;
const PREVIEW_MAX_CHARS = 160;
const PREVIEW_SCAN_CHARS = 512;

/**
 * A node in the timeline tree returned by `groupItemsBySubagent`.
 * Consumers should dispatch on `.kind`.
 */
export type TimelineNode = TimelineLeaf | SubagentGroupNode | InlineSubagentGroupNode;

export interface TimelineLeaf {
  kind: 'leaf';
  item: Item;
  /**
   * True when the item declared a parentId that didn't match any
   * visible parent. Rendered with a warning indicator instead of dropped.
   */
  orphan?: boolean;
}

export interface SubagentGroupNode {
  kind: 'group';
  /** The parent tool-call item that anchors the group. */
  parent: Item;
  /** Stable structural key for virtualization and expansion state. */
  groupKey: string;
  /** Recursively grouped children, preserving chronological order. */
  children: TimelineNode[];
  /** Total child count (counts *all* descendants, not just immediate children). */
  descendantCount: number;
  /**
   * Most recent descendant summary — drives the collapsed-header
   * preview on `SubagentGroup`. Selection rule:
   *   1. Highest-(turnIndex, itemIndex) descendant whose status is
   *      `running` or `streaming`. This keeps the preview locked to
   *      whatever the subagent is actively working on.
   *   2. Highest-(turnIndex, itemIndex) terminal descendant (any
   *      status) when nothing is currently running.
   *   3. Empty string when no descendant carries any summary.
   *
   * Empty when the group has no children with usable summaries yet.
   */
  latestChildSummary: string;
}

export interface InlineSubagentGroupNode {
  kind: 'inline_subagent_group';
  /** Stable structural key for the non-collapsible Claude inline wrapper. */
  groupKey: string;
  /** Thread id is carried directly because this node has no synthetic Item. */
  threadId: string;
  /** Real subagent cards represented by this wrapper, in timeline order. */
  members: SubagentGroupNode[];
  /** Number of inline Agent/Task launches represented by this wrapper. */
  memberCount: number;
  /** Total entries inside every represented subagent. */
  descendantCount: number;
}

/**
 * Compare two items by their (turnIndex, itemIndex) coordinate.
 * Callers usually feed the store's listing in order, but this keeps the
 * grouping deterministic when consumers concatenate from multiple sources.
 * Equal coordinates intentionally return 0 so stable sort preserves the
 * backend/store arrival order used by the transcript.
 */
function compareItems(a: Item, b: Item): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
  return 0;
}

/**
 * Extract a short preview string from an item that contributes user-visible
 * text (assistant messages, thinking, tool summaries). Empty for items whose
 * summary is non-text noise.
 */
function itemPreviewText(item: Item): string {
  const summary = item.summary ?? '';
  if (summary.length === 0) return '';
  const source = summary.length > PREVIEW_SCAN_CHARS
    ? summary.slice(0, PREVIEW_SCAN_CHARS)
    : summary;
  const normalized = source.replace(/\s+/g, ' ').trim();
  if (normalized.length <= PREVIEW_MAX_CHARS) return normalized;
  return `${normalized.slice(0, PREVIEW_MAX_CHARS).trimEnd()}...`;
}

/**
 * True when an item is in the middle of doing work — running tool
 * calls and actively-streaming text/thinking blocks both qualify.
 * Used by `pickLatestChildSummary` to bias the preview toward the
 * subagent's current activity.
 */
function isItemActive(item: Item): boolean {
  return item.status === 'running' || item.status === 'streaming';
}

interface InlineAgentInfo {
  groupKey: string;
}

function metaString(meta: Record<string, unknown> | null, key: string): string {
  const value = meta?.[key];
  return typeof value === 'string' ? value.trim() : '';
}

function isInlineAgentToolName(toolName: string): boolean {
  return toolName === 'Agent' || toolName === 'Task';
}

function inlineAgentInfo(item: Item): InlineAgentInfo | null {
  if (item.kind !== 'tool_call') return null;
  if (item.isBackground === true) return null;

  const directToolName = (item.toolName ?? '').trim();
  if (directToolName && !isInlineAgentToolName(directToolName)) return null;

  const meta = parseJsonObject(item.meta);
  if (meta?.is_inline_subagent !== true) return null;

  const toolName = directToolName || metaString(meta, 'toolName');
  if (!isInlineAgentToolName(toolName)) return null;

  const stampedGroupID = metaString(meta, 'inline_subagent_group_id');
  return {
    groupKey: stampedGroupID ? `inline:${stampedGroupID}` : `item:${item.id}`,
  };
}

function isClaudeInlineAgentLaunch(item: Item): boolean {
  return inlineAgentInfo(item) !== null;
}

function subagentNodeGroupKey(item: Item, inlineGroupKey: string): string {
  return `${inlineGroupKey}:${item.id}`;
}

function timelineNodeRootItem(node: TimelineNode): Item {
  if (node.kind === 'leaf') return node.item;
  if (node.kind === 'group') return node.parent;
  return node.members[0].parent;
}

/**
 * Walk every descendant Item under `nodes` (depth-first, preserving
 * arrival order) and yield it. Only the leaves' Items and group
 * parents' Items are surfaced — group nodes themselves are
 * structural, not items.
 */
function* descendantItems(nodes: TimelineNode[]): Generator<Item> {
  for (const node of nodes) {
    if (node.kind === 'leaf') {
      yield node.item;
      continue;
    }
    if (node.kind === 'group') {
      yield node.parent;
      yield* descendantItems(node.children);
      continue;
    }
    for (const member of node.members) {
      yield member.parent;
      yield* descendantItems(member.children);
    }
  }
}

/**
 * Pick the descendant whose summary should appear in the collapsed
 * SubagentGroup header. Prefers active (running/streaming) descendants
 * so the preview tracks what the subagent is doing now; falls back to
 * the most recent terminal descendant only when nothing is active.
 *
 * Comparison key is `(turnIndex, itemIndex)` — the same canonical
 * ordering the timeline uses everywhere else.
 */
function pickLatestChildSummary(children: TimelineNode[]): string {
  let bestActive: { item: Item; preview: string } | null = null;
  let bestTerminal: { item: Item; preview: string } | null = null;
  for (const item of descendantItems(children)) {
    const preview = itemPreviewText(item);
    if (!preview) continue;
    if (isItemActive(item)) {
      if (!bestActive || compareItems(item, bestActive.item) > 0) bestActive = { item, preview };
    } else if (!bestActive) {
      // Only track terminals while no active descendant is in the
      // running. Once an active candidate appears we stop bothering
      // with terminals — they can't beat an active winner regardless
      // of order.
      if (!bestTerminal || compareItems(item, bestTerminal.item) > 0) bestTerminal = { item, preview };
    }
  }
  const winner = bestActive ?? bestTerminal;
  return winner?.preview ?? '';
}

/**
 * Count every descendant (recursive) under a group node.
 */
function countDescendants(children: TimelineNode[]): number {
  let n = 0;
  for (const child of children) {
    n += 1;
    if (child.kind === 'group') n += countDescendants(child.children);
    if (child.kind === 'inline_subagent_group') n += countDescendants(child.members);
  }
  return n;
}

function countSubagentEntries(members: SubagentGroupNode[]): number {
  let n = 0;
  for (const member of members) {
    n += countDescendants(member.children);
  }
  return n;
}

/**
 * Stable key for a timeline node, used by virtualization (VList getKey)
 * and by anything else that needs to identify a node across renders. The
 * key is unique within a thread; including the threadId guards against
 * stale-thread collisions when a snapshot is restored after a switch.
 */
export function timelineNodeKey(node: TimelineNode): string {
  if (node.kind === 'leaf') return `l:${node.item.threadId}:${node.item.id}`;
  if (node.kind === 'group') return `g:${node.parent.threadId}:${node.groupKey}`;
  return `ig:${node.threadId}:${node.groupKey}`;
}

/** Item id of the leaf or group root. */
export function timelineNodeItemId(node: TimelineNode): string {
  if (node.kind === 'leaf') return node.item.id;
  if (node.kind === 'group') return node.parent.id;
  return node.members[0].parent.id;
}

/** Turn index of the leaf or structural node's first represented item. */
export function timelineNodeTurnIndex(node: TimelineNode): number {
  return timelineNodeRootItem(node).turnIndex;
}

/**
 * Visual-cadence bucket for the timeline. Tool rows pack tight; prose
 * rows carry their own bottom margin; everything else (notifications,
 * compaction, errors, thinking) is treated as transparent for boundary
 * purposes so it neither triggers nor breaks a tool↔text transition.
 */
export type NodeRole = 'tool' | 'text' | 'other';

export function nodeRole(node: TimelineNode): NodeRole {
  if (node.kind === 'group' || node.kind === 'inline_subagent_group') return 'tool';
  const k = node.item.kind;
  if (k === 'tool_call' || k === 'tool_completion' || k === 'terminal_interaction') return 'tool';
  if (k === 'assistant_text' || k === 'user_text') return 'text';
  return 'other';
}

/**
 * True iff `nodes[index]` sits at a tool↔text boundary — the predecessor
 * and current node are both `tool` or `text` and disagree. Drives the
 * conditional `mt-4` on the per-row wrapper so prose visibly clears the
 * adjacent tool block in either direction; tool↔tool stays tight.
 */
export function isToolTextBoundary(nodes: TimelineNode[], index: number): boolean {
  if (index <= 0) return false;
  const prev = nodeRole(nodes[index - 1]);
  const curr = nodeRole(nodes[index]);
  if (prev === 'other' || curr === 'other') return false;
  return prev !== curr;
}

/**
 * Item ids that are the LAST top-level `assistant_text` in their turn,
 * excluding any turn currently in flight. The "Response" pill divider
 * uses this to label only the closing assistant message of a settled
 * turn; intermediate text-after-tool boundaries inside the same turn
 * render the divider as a plain line. Excluding the in-flight turn is
 * load-bearing because providers emit per-wire-round (see
 * internal/triage/CLAUDE.md § Wire-round vs logical-turn): more rounds
 * may still arrive and promote a different message to "final."
 *
 * Subagent-group descendants are intentionally invisible to this walk —
 * the divider can only sit before a top-level leaf (chat row contract),
 * so a turn whose only trailing assistant_text lives inside a group
 * gets no pill.
 */
export function finalAssistantTextIdsByTurn(
  nodes: readonly TimelineNode[],
  activeTurnIndex: number | null,
): Set<string> {
  const lastByTurn = new Map<number, string>();
  for (const node of nodes) {
    if (node.kind !== 'leaf') continue;
    if (node.item.kind !== 'assistant_text') continue;
    lastByTurn.set(node.item.turnIndex, node.item.id);
  }
  if (activeTurnIndex !== null) {
    lastByTurn.delete(activeTurnIndex);
  }
  return new Set(lastByTurn.values());
}

/**
 * Recursive containment check: does `node` (or any descendant of a group
 * node) carry an item with this id?
 */
export function nodeContainsItem(node: TimelineNode, itemId: string): boolean {
  if (node.kind === 'leaf') return node.item.id === itemId;
  if (node.kind === 'group') {
    return node.parent.id === itemId
      || node.children.some((child) => nodeContainsItem(child, itemId));
  }
  return node.members.some((member) => nodeContainsItem(member, itemId));
}

/**
 * Find the index in a flat node list of the root that carries (or
 * contains) `itemId`. Returns -1 when no node matches. The caller is
 * responsible for paging back via pane.loadUntilItem if the item lives
 * outside the loaded window.
 */
export function findTimelineNodeIndex(nodes: TimelineNode[], itemId: string): number {
  return nodes.findIndex((node) => nodeContainsItem(node, itemId));
}

/**
 * Group items by subagent parentage. Pure function — does not mutate the
 * input and returns a fresh tree each call.
 */
export function groupItemsBySubagent(items: readonly Item[]): TimelineNode[] {
  if (items.length === 0) return [];

  // Fast path: if no item declares a parentId, no item is an inline Agent,
  // AND the input is already in canonical order, there is nothing to group.
  // Skip the sort, id-set build, and grouping walk entirely — just wrap
  // each item as a leaf. ThreadPane.upsertItem maintains canonical order
  // on insertion, so MessageTimeline (the hot caller) hits this path for
  // the common no-subagent thread.
  //
  // The monotonic check preserves the documented contract that callers
  // need not pre-sort: if items arrive out of order we fall through to
  // the slow path which sorts defensively.
  //
  // Measured (N=500 items, common no-subagent case): 25µs -> 3µs per call,
  // a ~9x win. Threads with subagents are the minority; the grouping logic
  // below is still exercised for them.
  let hasGroupingSignals = false;
  let alreadySorted = true;
  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    const pid = item.parentId;
    if ((pid && pid.length > 0) || isClaudeInlineAgentLaunch(item)) {
      hasGroupingSignals = true;
    }
    if (i > 0) {
      const prev = items[i - 1];
      if (compareItems(prev, item) > 0) {
        alreadySorted = false;
      }
    }
  }
  if (!hasGroupingSignals && alreadySorted) {
    const leaves: TimelineNode[] = new Array(items.length);
    for (let i = 0; i < items.length; i++) {
      leaves[i] = { kind: 'leaf', item: items[i] };
    }
    return leaves;
  }

  // Work on a shallow copy sorted in canonical order so callers may pass
  // any collection (e.g., a subset) without needing to pre-sort.
  const sorted = alreadySorted ? items : [...items].sort(compareItems);

  const idSet = new Set(sorted.map((it) => it.id));
  const inlineAgentIDs = new Set<string>();
  const inlineGroupKeyByItemID = new Map<string, string>();

  for (const item of sorted) {
    const info = inlineAgentInfo(item);
    if (!info) continue;
    const key = info.groupKey;
    inlineAgentIDs.add(item.id);
    inlineGroupKeyByItemID.set(item.id, key);
  }

  // Index children by parentId, but only for stable inline-agent launch
  // parents. Generic parentId nesting stays flat by design.
  const childrenByParent = new Map<string, Item[]>();
  const orphanIds = new Set<string>();

  for (const item of sorted) {
    const pid = item.parentId ?? '';
    if (!pid) continue;
    if (!idSet.has(pid)) {
      orphanIds.add(item.id);
      continue;
    }
    if (!inlineAgentIDs.has(pid)) continue;
    const bucket = childrenByParent.get(pid);
    if (bucket) {
      bucket.push(item);
    } else {
      childrenByParent.set(pid, [item]);
    }
  }

  // Stable chronological order within each bucket.
  for (const bucket of childrenByParent.values()) {
    bucket.sort(compareItems);
  }

  function buildNode(item: Item, depth: number): TimelineNode {
    const childItems = childrenByParent.get(item.id);
    if ((!childItems || childItems.length === 0) && !inlineAgentIDs.has(item.id)) {
      return { kind: 'leaf', item };
    }

    if (depth >= MAX_DEPTH) {
      // Cap depth: render the deeper descendants as flat leaf siblings of
      // this node's parent instead of nesting further. The group still
      // reports the full descendant count so the collapsed card is honest.
      const flatChildren: TimelineNode[] = [];
      const stack: Item[] = [...(childItems ?? [])];
      while (stack.length > 0) {
        const next = stack.shift()!;
        flatChildren.push({ kind: 'leaf', item: next });
        const grand = childrenByParent.get(next.id);
        if (grand) stack.push(...grand);
      }
      return {
        kind: 'group',
        parent: item,
        groupKey: subagentNodeGroupKey(item, inlineGroupKeyByItemID.get(item.id) ?? `item:${item.id}`),
        children: flatChildren,
        descendantCount: flatChildren.length,
        latestChildSummary: pickLatestChildSummary(flatChildren),
      };
    }

    const children = (childItems ?? []).map((child) => buildNode(child, depth + 1));
    return {
      kind: 'group',
      parent: item,
      groupKey: subagentNodeGroupKey(item, inlineGroupKeyByItemID.get(item.id) ?? `item:${item.id}`),
      children,
      descendantCount: countDescendants(children),
      latestChildSummary: pickLatestChildSummary(children),
    };
  }

  function buildInlineAgentWrapper(members: Item[]): InlineSubagentGroupNode {
    const [first] = members;
    const baseGroupKey = inlineGroupKeyByItemID.get(first.id) ?? `item:${first.id}`;
    const groupKey = `${baseGroupKey}:${first.id}`;
    const memberNodes = members
      .map((member) => buildNode(member, 1))
      .filter((node): node is SubagentGroupNode => node.kind === 'group')
      .sort((a, b) => compareItems(a.parent, b.parent));
    return {
      kind: 'inline_subagent_group',
      groupKey,
      threadId: first.threadId,
      memberCount: members.length,
      members: memberNodes,
      descendantCount: countSubagentEntries(memberNodes),
    };
  }

  const roots: TimelineNode[] = [];
  for (let index = 0; index < sorted.length; index += 1) {
    const item = sorted[index];
    const inlineKey = inlineGroupKeyByItemID.get(item.id);
    if (inlineKey) {
      const pid = item.parentId ?? '';
      if (pid && inlineAgentIDs.has(pid)) {
        continue;
      }
      const members = [item];
      let cursor = index + 1;
      while (cursor < sorted.length) {
        const candidate = sorted[cursor];
        const candidateInlineKey = inlineGroupKeyByItemID.get(candidate.id);
        const candidateParentID = candidate.parentId ?? '';
        if (candidateInlineKey !== inlineKey || (candidateParentID && inlineAgentIDs.has(candidateParentID))) {
          break;
        }
        members.push(candidate);
        cursor += 1;
      }
      roots.push(buildInlineAgentWrapper(members));
      index = cursor - 1;
      continue;
    }

    const pid = item.parentId ?? '';
    if (!pid) {
      roots.push({ kind: 'leaf', item });
      continue;
    }
    if (orphanIds.has(item.id)) {
      // Orphans surface at the top level with a warning indicator. Rendering
      // silently would lose data; this keeps the timeline honest.
      roots.push({ kind: 'leaf', item, orphan: true });
      continue;
    }
    if (!inlineAgentIDs.has(pid)) {
      roots.push({ kind: 'leaf', item });
    }
    // Otherwise child is consumed by its inline-agent group above.
  }

  return roots;
}
