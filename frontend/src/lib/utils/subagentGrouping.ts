// Pure projection utility for turning a flat timeline of Items into stable
// transcript nodes. Structural grouping is deliberately limited to provider
// subagent launches and wait carriers. Generic parentId nesting is
// deliberately not used because it can make an already-rendered row flip
// from leaf -> group.
//
// Contract:
//   - Input: a chronologically-ordered list of Items (preserves turnIndex
//     / itemIndex order — callers do not need to pre-sort).
//   - Output: an array of TimelineNode roots. A `group` node wraps a parent
//     item plus recursively-grouped children. A `wait_group` node wraps a
//     terminal/subagent wait carrier plus target completion leaves observed by
//     that wait. A `leaf` node wraps a single item with nothing under it.
//
// Rules:
//   - Normal rows always stay leaves.
//   - Claude foreground Agent/Task launch rows are groups from first render,
//     even before any child activity arrives.
//   - Claude backgrounded Agent/Task launch rows stay leaves; their inner
//     transcript (rows whose parentId is the launch id) is withheld from the
//     main timeline. The launch's live status shows in the background tray,
//     and the subagent's result surfaces on the completion sibling row
//     (expandable) when it finishes — not in the tray, which only carries
//     launch/completion status. This is the same child-suppression Codex
//     spawn rows get (next rule).
//   - Codex spawn_agent launch rows stay leaves. Their child transcript is
//     provider-internal detail represented by the background tray while live
//     and explicit completion rows after wait/notification signals.
//   - Wait carriers use a stable structural wrapper from first render; Codex
//     subagent target completions render beneath them when linked by
//     `wait_carrier_id` or shared wait payload correlation.
//   - parentId children are nested only when their parent is one of those
//     subagent launch rows. Children of non-agent parents stay flat.
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
import { isCodexSubagentLaunchItem } from './subagentLaunch';

// Shared core for the two launch predicates below. The Agent/Task tool-name
// set is the one thing that must stay identical between foreground grouping
// and background suppression — factor it so a new subagent tool name can't
// drift the two apart.
function isAgentOrTaskLaunch(item: Item): boolean {
  return item.kind === 'tool_call'
    && (item.toolName === 'Agent' || item.toolName === 'Task');
}

// Foreground Agent/Task launches anchor a group from first render.
// `isBackground` is optional on the wire; undefined and false both mean
// foreground, so this is the negation — deliberately NOT a strict `=== false`.
function isSubagentLaunch(item: Item): boolean {
  return isAgentOrTaskLaunch(item) && !item.isBackground;
}

// Backgrounded Claude Agent/Task launches are deliberately NOT inline group
// anchors — an expanding card would crowd the main thread with live subagent
// churn (the exact "tool calls jumping around" symptom). Their inner
// transcript (rows whose parentId is the launch id) is suppressed from the
// main timeline the same way Codex spawn-agent children are; the outcome —
// success or failure alike — surfaces on the completion sibling row (whose
// parentId is empty, so this suppression never swallows it), and live status
// shows in the background tray. The launch row stays a leaf so the thread
// records where it began.
function isBackgroundSubagentLaunch(item: Item): boolean {
  return isAgentOrTaskLaunch(item) && item.isBackground === true;
}

export const MAX_DEPTH = 3;
const PREVIEW_MAX_CHARS = 160;
const PREVIEW_SCAN_CHARS = 512;

/**
 * A node in the timeline tree returned by `groupItemsBySubagent`,
 * plus the `read_group` variant produced as a top-level post-pass by
 * `groupConsecutiveReads` (`readGrouping.ts`). Consumers should
 * dispatch on `.kind`.
 */
export type TimelineNode =
  | TimelineLeaf
  | SubagentGroupNode
  | WaitGroupNode
  | ReadGroupNode;

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

export interface WaitGroupNode {
  kind: 'wait_group';
  /** The wait/poll row that anchors the group. */
  parent: Item;
  /** Stable structural key for virtualization. */
  groupKey: string;
  /**
   * The standalone `wait_agent` tool_completion (id like `complete:<carrierId>`,
   * `completionOf === parent.id`) once it has loaded. Rendered AS the group
   * header by `WaitGroup.svelte` in place of the carrier, so a finished wait
   * reads "Finished waiting" + per-agent statuses instead of the carrier
   * tool_call's permanent "Waiting for N agents". Undefined while the wait is
   * still running, when the carrier is a terminal wait carrier (those have no
   * split completion item), or before the completion itself has loaded (it can
   * arrive after the carrier, or sit outside the loaded window).
   */
  completion?: Item;
  /**
   * Codex subagent target completion rows observed by this wait. Terminal
   * command completions intentionally render as sibling rows.
   */
  children: TimelineLeaf[];
  /** Total child count. */
  descendantCount: number;
}

/**
 * Top-level wrapper that folds adjacent Read tool_calls into a single
 * compact row. Produced by `groupConsecutiveReads` (`readGrouping.ts`)
 * as a post-pass over the output of `groupItemsBySubagent`, so Reads
 * nested inside a subagent stay inside their parent group and only
 * the top-level transcript collapses. Has no expansion or body — the
 * row renders a wrapped list of file links keyed by `members[i].id`.
 */
export interface ReadGroupNode {
  kind: 'read_group';
  /** Stable structural key derived from the first member's coordinates. */
  groupKey: string;
  /** Carried directly because this node has no synthetic anchor item. */
  threadId: string;
  /** Grouped Read tool_call items in timeline order. Always >= 2. */
  members: Item[];
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

function itemWaitCarrierID(item: Item): string {
  const meta = parseJsonObject(item.meta);
  const carrierID = meta?.wait_carrier_id ?? meta?.waitCarrierID;
  return typeof carrierID === 'string' ? carrierID.trim() : '';
}

function isTerminalWaitCarrier(item: Item): boolean {
  if (item.kind !== 'terminal_interaction') return false;
  const meta = parseJsonObject(item.meta);
  return meta?.has_stdin !== true;
}

function nestedMetaString(meta: Record<string, unknown> | null, parentKey: string, key: string): string {
  const parent = meta?.[parentKey];
  if (!parent || typeof parent !== 'object' || Array.isArray(parent)) return '';
  const value = (parent as Record<string, unknown>)[key];
  return typeof value === 'string' ? value.trim() : '';
}

function isCodexWaitAgentCarrier(item: Item): boolean {
  if (item.kind !== 'tool_call' || item.toolName !== 'wait_agent') return false;
  const meta = parseJsonObject(item.meta);
  return nestedMetaString(meta, 'input', 'tool') === 'wait_agent';
}

function isWaitCarrier(item: Item): boolean {
  return isTerminalWaitCarrier(item)
    || isCodexWaitAgentCarrier(item);
}

function timelineNodeRootItem(node: TimelineNode): Item {
  if (node.kind === 'leaf') return node.item;
  if (node.kind === 'group' || node.kind === 'wait_group') return node.parent;
  if (node.kind === 'read_group') return node.members[0];
  const _exhaustive: never = node;
  return _exhaustive;
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
    if (node.kind === 'wait_group') {
      yield node.parent;
      yield* descendantItems(node.children);
      continue;
    }
    if (node.kind === 'read_group') {
      for (const member of node.members) yield member;
      continue;
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
export function pickLatestChildSummary(children: TimelineNode[]): string {
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
    if (child.kind === 'group' || child.kind === 'wait_group') n += countDescendants(child.children);
    if (child.kind === 'read_group') n += child.members.length - 1;
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
  if (node.kind === 'wait_group') return `wg:${node.parent.threadId}:${node.groupKey}`;
  if (node.kind === 'read_group') return `rg:${node.threadId}:${node.groupKey}`;
  const _exhaustive: never = node;
  return _exhaustive;
}

/** Item id of the leaf or group root. */
export function timelineNodeItemId(node: TimelineNode): string {
  if (node.kind === 'leaf') return node.item.id;
  if (node.kind === 'group' || node.kind === 'wait_group') return node.parent.id;
  if (node.kind === 'read_group') return node.members[0].id;
  const _exhaustive: never = node;
  return _exhaustive;
}

/** Turn index of the leaf or structural node's first represented item. */
export function timelineNodeTurnIndex(node: TimelineNode): number {
  return timelineNodeRootItem(node).turnIndex;
}

/** Item index of the leaf or structural node's first represented item. */
export function timelineNodeItemIndex(node: TimelineNode): number {
  return timelineNodeRootItem(node).itemIndex;
}

/**
 * The last top-level item position the timeline may render while a turn is
 * streaming. Items strictly after it are withheld by `sliceRevealedNodes`
 * until the reveal sequencer advances the boundary. `null` means "no gate —
 * render everything", which is the steady state outside live streaming.
 */
export interface RevealBoundary {
  turnIndex: number;
  itemIndex: number;
}

/**
 * Returns the prefix of `nodes` whose representative (turnIndex, itemIndex)
 * is at or before `boundary`. `nodes` is already in (turnIndex, itemIndex)
 * order, so the withheld set is always a trailing run. A `null` boundary
 * returns the same array reference (no gate) so downstream `$derived`s don't
 * re-run. Pure — never mutates the input.
 *
 * The boundary is always a top-level item (the sequencer ignores subagent
 * children when choosing it), so a subagent group whose parent sits at or
 * before the boundary is returned whole — its still-streaming children
 * animate inside the card and are never individually gated here.
 */
export function sliceRevealedNodes(
  nodes: TimelineNode[],
  boundary: RevealBoundary | null,
): TimelineNode[] {
  if (!boundary) return nodes;
  for (let i = 0; i < nodes.length; i++) {
    const turnIndex = timelineNodeTurnIndex(nodes[i]);
    const itemIndex = timelineNodeItemIndex(nodes[i]);
    const afterBoundary =
      turnIndex > boundary.turnIndex ||
      (turnIndex === boundary.turnIndex && itemIndex > boundary.itemIndex);
    if (afterBoundary) return nodes.slice(0, i);
  }
  return nodes;
}

/**
 * Visual-cadence bucket for the timeline. The tool↔text boundary (the
 * `mt-4` driven by `isToolTextBoundary` below) is the only inter-row
 * spacer: tool rows pack tight, and prose runs tight against adjacent
 * prose and thinking (assistant messages no longer carry a bottom
 * margin). The single deliberate gap is the transition between a tool
 * block and prose. Everything else (notifications, compaction, errors,
 * thinking) is treated as transparent for boundary purposes so it
 * neither triggers nor breaks a tool↔text transition.
 */
export type NodeRole = 'tool' | 'text' | 'other';

export function nodeRole(node: TimelineNode): NodeRole {
  if (
    node.kind === 'group'
    || node.kind === 'wait_group'
    || node.kind === 'read_group'
  ) return 'tool';
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
export function isToolTextBoundary(nodes: readonly TimelineNode[], index: number): boolean {
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
  if (node.kind === 'group' || node.kind === 'wait_group') {
    // A wait_group also carries its folded `completion` — the standalone
    // wait_agent tool_completion rendered AS the header. Counting it as
    // contained lets a search hit on that completion's own id (Go indexes its
    // summary) resolve to this row instead of silently failing to scroll. The
    // anchor id (timelineNodeItemId) stays the carrier; containment is the
    // separate "does this row carry that item?" question, answered yes here.
    return node.parent.id === itemId
      || (node.kind === 'wait_group' && node.completion?.id === itemId)
      || node.children.some((child) => nodeContainsItem(child, itemId));
  }
  if (node.kind === 'read_group') return node.members.some((m) => m.id === itemId);
  const _exhaustive: never = node;
  return _exhaustive;
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

export function visibleTimelineItemIdForItem(items: readonly Item[], itemId: string): string {
  let parentID = '';
  for (const item of items) {
    if (item.id !== itemId) continue;
    parentID = item.parentId ?? '';
    break;
  }
  if (!parentID) return itemId;
  for (const item of items) {
    if (item.id === parentID && isCodexSubagentLaunchItem(item)) return item.id;
  }
  return itemId;
}

/**
 * Group items by subagent parentage. Pure function — does not mutate the
 * input and returns a fresh tree each call.
 */
export function groupItemsBySubagent(items: readonly Item[]): TimelineNode[] {
  if (items.length === 0) return [];

  // Fast path: if no item declares a parentId, no item is a subagent launch,
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
    if ((pid && pid.length > 0) || item.completionOf || item.kind === 'terminal_interaction' || item.toolName === 'wait_agent' || isSubagentLaunch(item)) {
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
  const sortedWithCarriers = alreadySorted ? items : [...items].sort(compareItems);
  const itemByID = new Map<string, Item>();
  const codexSpawnIDs = new Set<string>();
  const backgroundSubagentIDs = new Set<string>();
  const waitCompletionPayloadCarrierByPayloadID = new Map<string, string>();
  const waitChildrenByCarrierID = new Map<string, Item[]>();
  const waitChildIDs = new Set<string>();
  const waitCompletionByCarrierID = new Map<string, Item>();

  function addWaitChild(carrierID: string, child: Item): void {
    const carrier = itemByID.get(carrierID);
    if (!carrier || !isWaitCarrier(carrier)) return;
    if (isTerminalWaitCarrier(carrier) && child.toolName === 'command_execution') return;
    if (waitChildIDs.has(child.id)) return;
    const bucket = waitChildrenByCarrierID.get(carrierID);
    if (bucket) {
      bucket.push(child);
    } else {
      waitChildrenByCarrierID.set(carrierID, [child]);
    }
    waitChildIDs.add(child.id);
  }

  // A wait_agent tool_completion folds into its wait_group (rendered AS the
  // header) exactly when its carrier is a loaded Codex wait_agent carrier. The
  // capture into waitCompletionByCarrierID and the drop from the top-level leaf
  // list MUST agree on this predicate: folding without dropping double-renders
  // the completion, dropping without folding makes it vanish. One named check
  // keeps the two sites in lockstep. Terminal wait carriers and foreign
  // wait_agent-named tools fail the gate and are left untouched. itemByID is
  // fully populated by the pass below before either caller runs.
  function carrierIsLoadedCodexWait(completionOf: string): boolean {
    const carrier = itemByID.get(completionOf);
    return carrier !== undefined && isCodexWaitAgentCarrier(carrier);
  }

  for (const item of sortedWithCarriers) {
    itemByID.set(item.id, item);
    if (isCodexSubagentLaunchItem(item)) {
      codexSpawnIDs.add(item.id);
    }
    if (isBackgroundSubagentLaunch(item)) {
      backgroundSubagentIDs.add(item.id);
    }
  }

  for (const item of sortedWithCarriers) {
    if (isCodexSubagentLaunchItem(item)) {
      continue;
    }
    if (isTerminalWaitCarrier(item)) {
      continue;
    }
    if (item.kind === 'tool_completion' && item.toolName === 'wait_agent' && item.completionOf) {
      if (item.payloadId) {
        waitCompletionPayloadCarrierByPayloadID.set(item.payloadId, item.completionOf);
      }
      // Fold this standalone completion into its wait_group as the header
      // (see buildNode and carrierIsLoadedCodexWait).
      if (carrierIsLoadedCodexWait(item.completionOf)) {
        waitCompletionByCarrierID.set(item.completionOf, item);
      }
      continue;
    }
    if (item.kind !== 'tool_completion' || !item.completionOf || item.toolName === 'wait_agent') {
      continue;
    }
    const explicitCarrierID = itemWaitCarrierID(item);
    if (explicitCarrierID) {
      addWaitChild(explicitCarrierID, item);
      continue;
    }
    if (item.payloadId) {
      const carrierID = waitCompletionPayloadCarrierByPayloadID.get(item.payloadId);
      if (carrierID) addWaitChild(carrierID, item);
    }
  }
  const sorted = sortedWithCarriers.filter((item) => {
    const parentID = item.parentId ?? '';
    if (parentID && (codexSpawnIDs.has(parentID) || backgroundSubagentIDs.has(parentID))) {
      return false;
    }
    if (waitChildIDs.has(item.id)) return false;
    if (item.kind === 'tool_completion' && item.toolName === 'wait_agent' && item.completionOf) {
      // Drop the standalone wait_agent completion when its Codex carrier is
      // loaded — the wait_group folds it in as the header (it used to render as
      // a top-level "Finished waiting" leaf that flashed before children linked,
      // then vanished when they did). At a page boundary where the carrier is
      // outside the loaded window, keep it as a leaf so a finished wait still
      // renders something rather than disappearing.
      if (carrierIsLoadedCodexWait(item.completionOf)) return false;
    }
    return true;
  });

  const idSet = new Set(sorted.map((it) => it.id));
  const subagentLaunchIDs = new Set<string>();

  for (const item of sorted) {
    if (isSubagentLaunch(item)) subagentLaunchIDs.add(item.id);
  }

  // Index children by parentId, but only for stable subagent launch
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
    if (!subagentLaunchIDs.has(pid)) continue;
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
    if (isWaitCarrier(item)) {
      const children = (waitChildrenByCarrierID.get(item.id) ?? [])
        .map((child): TimelineLeaf => ({ kind: 'leaf', item: child }));
      return {
        kind: 'wait_group',
        parent: item,
        groupKey: `wait:${item.id}`,
        completion: waitCompletionByCarrierID.get(item.id),
        children,
        descendantCount: children.length,
      };
    }

    const childItems = childrenByParent.get(item.id);
    if ((!childItems || childItems.length === 0) && !subagentLaunchIDs.has(item.id)) {
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
        groupKey: item.id,
        children: flatChildren,
        descendantCount: flatChildren.length,
        latestChildSummary: pickLatestChildSummary(flatChildren),
      };
    }

    const children = (childItems ?? []).map((child) => buildNode(child, depth + 1));
    return {
      kind: 'group',
      parent: item,
      groupKey: item.id,
      children,
      descendantCount: countDescendants(children),
      latestChildSummary: pickLatestChildSummary(children),
    };
  }

  const roots: TimelineNode[] = [];
  for (const item of sorted) {
    const pid = item.parentId ?? '';
    if (pid && subagentLaunchIDs.has(pid)) {
      continue;
    }
    if (!pid) {
      roots.push(buildNode(item, 0));
      continue;
    }
    if (orphanIds.has(item.id)) {
      roots.push({ kind: 'leaf', item, orphan: true });
      continue;
    }
    if (!subagentLaunchIDs.has(pid)) {
      roots.push({ kind: 'leaf', item });
    }
  }

  return roots;
}
