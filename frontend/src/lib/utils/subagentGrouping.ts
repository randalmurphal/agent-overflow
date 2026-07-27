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
//   - Claude backgrounded Agent/Task launch rows stay leaves; their entire
//     descendant subtree (every row transitively parented under the launch —
//     not just direct children, so a non-background launch nested inside a
//     backgrounded one is withheld along with its own children in turn) is
//     withheld from the main timeline. The launch's live status shows in the
//     background tray, and the subagent's result surfaces on the completion
//     sibling row (expandable) when it finishes — not in the tray, which
//     only carries launch/completion status. This is the same transitive
//     child-suppression Codex spawn rows get (next rule).
//   - Codex spawn_agent launch rows stay leaves. Their descendant transcript
//     (transitively, same as above) is provider-internal detail represented
//     by the background tray while live and explicit completion rows after
//     wait/notification signals.
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
//   - Loaded child rows are not the only descendants: the pane evicts
//     settled subagent children from memory (utils/subagentFold.ts) and
//     passes their per-anchor aggregates in as `SubagentLiveAggregates`.
//     Group counts compose loaded + evicted (ratcheted against the
//     backend-decorated count), and the evicted terminal preview competes
//     with loaded terminals by position. Active loaded children always win
//     the preview — evicted rows are terminal by definition.
//
// The grouping function is pure — no mutation of inputs, no side effects.

import type { Item } from '../types/models';
import type { SubagentFoldAggregate } from './subagentFold';
import { parseJsonObject } from './parseJsonObject';
import { isCodexSubagentLaunchItem } from './subagentLaunch';

/**
 * Accessor into the pane's live-eviction fold registry. Group nodes add
 * the evicted-terminal count and compete the folded terminal preview
 * against loaded children, so a collapsed card renders identically
 * whether its settled transcript rows are in memory or evicted.
 */
export type SubagentLiveAggregates = (
  anchorId: string,
) => SubagentFoldAggregate | undefined;

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
// churn (the exact "tool calls jumping around" symptom). Their entire
// descendant subtree (every row transitively parented under the launch, not
// just direct children — see the suppressedDescendantIDs pass in
// groupItemsBySubagent) is suppressed from the main timeline the same way
// Codex spawn-agent descendants are; the outcome — success or failure alike —
// surfaces on the completion sibling row (whose parentId is empty, so this
// suppression never swallows it), and live status shows in the background
// tray. The launch row stays a leaf so the thread records where it began.
function isBackgroundSubagentLaunch(item: Item): boolean {
  return isAgentOrTaskLaunch(item) && item.isBackground === true;
}

/**
 * How a launch anchor presents its child transcript, for the pane's
 * live-eviction policy:
 *   - 'inline': foreground Claude Agent/Task — children render inside the
 *     card when expanded, so terminal children are evictable only while
 *     the card is collapsed.
 *   - 'suppressed': backgrounded Claude launches and Codex spawns — the
 *     grouping walk drops their entire descendant subtree from the timeline
 *     entirely (transitively, not just direct children), so terminal
 *     children are always evictable.
 *   - null: not a launch anchor. Items whose parentId points at a
 *     non-launch row render as flat top-level leaves and must never be
 *     evicted.
 */
export type SubagentLaunchKind = 'inline' | 'suppressed';

export function subagentLaunchKind(item: Item): SubagentLaunchKind | null {
  if (isSubagentLaunch(item)) return 'inline';
  if (isBackgroundSubagentLaunch(item) || isCodexSubagentLaunchItem(item)) {
    return 'suppressed';
  }
  return null;
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
  | ReadGroupNode
  | ActivityRunNode;

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
  /**
   * Total child count (counts *all* descendants, not just immediate
   * children). Composed from three sources: descendants loaded in
   * memory, terminal descendants evicted into the pane's live fold
   * (`SubagentLiveAggregates`), and — as a ratchet floor — the
   * backend-decorated aggregate stamped on history-loaded anchors.
   */
  descendantCount: number;
  /**
   * Descendants actually present in memory under this group. When this
   * trails `descendantCount` the child transcript is paged out and
   * loads on demand via ListSubagentDescendants when the card expands.
   */
  loadedDescendantCount: number;
  /**
   * Most recent descendant summary — drives the collapsed-header
   * preview on `SubagentGroup`. Selection rule:
   *   1. Highest-(turnIndex, itemIndex) loaded descendant whose status
   *      is `running` or `streaming`. This keeps the preview locked to
   *      whatever the subagent is actively working on.
   *   2. With nothing active: the live fold's evicted-terminal preview
   *      (`SubagentLiveAggregates`) when its position is at or after
   *      the best loaded terminal; otherwise that loaded terminal.
   *   3. The backend-decorated summary from the anchor's meta when no
   *      loaded or folded descendant carries text (history loads keep
   *      child rows paged out).
   *   4. Empty string otherwise.
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
   * reads "Finished waiting" + the waited agent list instead of the carrier
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
 * Top-level wrapper that renders adjacent Read tool_calls through one
 * stable row. Produced by `groupConsecutiveReads` (`readGrouping.ts`)
 * as a post-pass over the output of `groupItemsBySubagent`, so Reads
 * nested inside a subagent stay inside their parent group and only
 * the top-level transcript collapses. Has no expansion or body — the
 * row renders a wrapped list of file links keyed by `members[i].id`.
 */
export interface ReadGroupNode {
  kind: 'read_group';
  /** Stable structural key derived from the first member id. */
  groupKey: string;
  /** Carried directly because this node has no synthetic anchor item. */
  threadId: string;
  /** Grouped Read tool_call items in timeline order. Always >= 1. */
  members: Item[];
}

/**
 * A maximal run of consecutive rail-kind rows (tool calls, completions,
 * thinking, and the subagent / wait / read group containers) wrapped into
 * one row so long stretches of activity can be bounded, scrolled in place,
 * or collapsed to a count chip. Produced by `groupActivityRuns`
 * (`activityRunGrouping.ts`) as the LAST projection pass, after the
 * streaming reveal gate.
 *
 * Not a stored entity: runs are recomputed every projection pass from
 * whatever items are loaded. See `runId` for how identity survives that.
 */
export interface ActivityRunNode {
  kind: 'activity_run';
  /**
   * Registry-assigned identity, stable across the window edges. Deriving
   * a key from the first member id instead would remount the row (and
   * recreate its scroll controller) whenever lazy older-paging extended
   * the run backward or a live-window prune trimmed its head — the second
   * of which happens mid-stream on exactly the long runs this node exists
   * to bound.
   */
  runId: string;
  /** Carried directly because this node has no anchor item of its own. */
  threadId: string;
  /** The wrapped rows, in timeline order. Always >= 1. */
  children: TimelineNode[];
  /**
   * Resolved at projection time from the per-pane registry, falling back to
   * the `activityRunDefault` setting. Carried on the node so the row
   * signature can price a chip differently from a clip — folding it into
   * the entry-level `expansionSig` instead would drop every row's prior in
   * the thread each time one run was toggled.
   */
  collapsed: boolean;
  /**
   * This run holds the timeline's tail, so new activity lands in it.
   *
   * On the node rather than re-derived per consumer because two of them are
   * not components: the row signature has to price a collapsed LIVE run as a
   * chip over an open clip (it renders both, so a chip-shaped prior would
   * under-estimate it by the height of the clip), and the size estimate has
   * to agree. It flips at most twice in a run's life — once when it takes the
   * tail and once when a later node displaces it — so it costs nothing that
   * the per-delta fields deliberately kept off this node would have cost.
   */
  live: boolean;
  /**
   * The mounted row window: `mountedRows` children starting at
   * `mountedFrom`. Defaults to the run's tail and relocates when a jump
   * resolves into the run (`utils/activityRunWindow.ts`).
   *
   * Resolved onto the node rather than read from the registry at render time
   * so it reaches the row signature: moving or growing the window changes a
   * sub-cap run's height, and a signature blind to it would replay the
   * pre-change measured height.
   */
  mountedFrom: number;
  mountedRows: number;
  /**
   * Every item the run represents, group members included, in timeline
   * order. The chip aggregates its counts from the CURRENT items behind
   * these ids rather than from a snapshot baked in here: counts, failure,
   * and the running label all change on ordinary streaming deltas, and a
   * node that carried them would rebuild the virtualizer's data array on
   * every chunk. Same rule leaf rows already follow.
   */
  memberItemIds: readonly string[];
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
 * Normalize raw summary text into the collapsed-header preview shape:
 * whitespace collapsed, capped at PREVIEW_MAX_CHARS with an ellipsis.
 * Shared by loaded-children previews, the backend-decorated summary
 * fallback, and the pane's live-eviction fold (which captures the
 * preview at evict time) so all three render identically.
 */
export function normalizePreviewText(summary: string): string {
  if (summary.length === 0) return '';
  const source = summary.length > PREVIEW_SCAN_CHARS
    ? summary.slice(0, PREVIEW_SCAN_CHARS)
    : summary;
  const normalized = source.replace(/\s+/g, ' ').trim();
  if (normalized.length <= PREVIEW_MAX_CHARS) return normalized;
  return `${normalized.slice(0, PREVIEW_MAX_CHARS).trimEnd()}...`;
}

/**
 * Extract a short preview string from an item that contributes user-visible
 * text (assistant messages, thinking, tool summaries). Empty for items whose
 * summary is non-text noise.
 */
function itemPreviewText(item: Item): string {
  return normalizePreviewText(item.summary ?? '');
}

/**
 * Read the backend's subagent aggregates off a launch anchor's meta.
 * History windows load only top-level rows; the store decorates each
 * launch anchor with its transitive descendant count and the same
 * latest-child summary pickLatestChildSummary would compute (see
 * internal/store/subagent_items.go). Live anchors created by streaming
 * events carry no decoration — their active children are in memory and
 * their settled children are tracked by the pane's live-eviction fold
 * (utils/subagentFold.ts), surfaced through `SubagentLiveAggregates`.
 */
export function decoratedSubagentAggregates(item: Item): { count: number; summary: string } {
  const meta = parseJsonObject(item.meta);
  const rawCount = meta?.subagentDescendantCount;
  const count = typeof rawCount === 'number' && Number.isFinite(rawCount) && rawCount > 0
    ? Math.floor(rawCount)
    : 0;
  const rawSummary = meta?.subagentLatestChildSummary;
  const summary = typeof rawSummary === 'string' ? normalizePreviewText(rawSummary) : '';
  return { count, summary };
}

/**
 * True when an item is in the middle of doing work — running tool
 * calls and actively-streaming text/thinking blocks both qualify.
 * Biases `pickLatestChildSummary` toward the subagent's current
 * activity, and defines "settled" for the pane's live-eviction policy
 * (thread.svelte.ts) — the two must share one status set or eviction
 * could fold a row the preview still treats as active.
 */
export function isItemActive(item: Item): boolean {
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
  // A run's position is its first member's position — that is what orders
  // it against prose rows and what the reveal gate compares against.
  if (node.kind === 'activity_run') return timelineNodeRootItem(node.children[0]);
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
    if (node.kind === 'activity_run') {
      yield* descendantItems(node.children);
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
 * The pane's live-eviction `fold` competes on the terminal side:
 * active loaded descendants always win (evicted rows are terminal by
 * definition, so they can never outrank live work); with nothing
 * active, the folded terminal preview wins when its position is at or
 * after the best loaded terminal.
 *
 * Comparison key is `(turnIndex, itemIndex)` — the same canonical
 * ordering the timeline uses everywhere else.
 */
export function pickLatestChildSummary(
  children: TimelineNode[],
  fold?: SubagentFoldAggregate,
): string {
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
  if (bestActive) return bestActive.preview;
  if (fold && fold.terminalPreview) {
    if (
      !bestTerminal
      || fold.terminalTurnIndex > bestTerminal.item.turnIndex
      || (fold.terminalTurnIndex === bestTerminal.item.turnIndex
        && fold.terminalItemIndex >= bestTerminal.item.itemIndex)
    ) {
      return fold.terminalPreview;
    }
  }
  return bestTerminal?.preview ?? '';
}

/**
 * Count every descendant (recursive) under a group node. Nested group
 * children contribute their own `descendantCount` — which already folds
 * in their evicted-terminal rows — so an outer card's entry counter
 * stays honest when an inner agent's settled transcript is paged out.
 */
function countDescendants(children: TimelineNode[]): number {
  let n = 0;
  for (const child of children) {
    if (child.kind === 'activity_run') {
      // A run is a presentation wrapper, not a descendant: count through it
      // so a card's entry counter is unchanged by how its rows are grouped.
      n += countDescendants(child.children);
      continue;
    }
    n += 1;
    if (child.kind === 'group' || child.kind === 'wait_group') n += child.descendantCount;
    if (child.kind === 'read_group') n += child.members.length - 1;
  }
  return n;
}

/**
 * Assemble a SubagentGroupNode, reconciling three sources: children
 * loaded in memory (they track live status and win the preview), the
 * pane's live-eviction fold (terminal rows dropped from memory while
 * the card is collapsed), and the backend-decorated aggregates on the
 * anchor (history loads deliver anchors without child rows). The count
 * is loaded + folded, ratcheted against the decoration.
 */
function subagentGroupNode(
  parent: Item,
  children: TimelineNode[],
  loadedDescendantCount: number,
  aggregates: SubagentLiveAggregates | undefined,
  // Evicted-fold counts of launches rendered as flattened leaves inside
  // this group (depth-cap path). They are part of the true total but are
  // NOT loaded rows — keeping them out of `loadedDescendantCount` keeps
  // the card's hydrate-on-expand trigger (loaded < descendant) honest.
  flattenedFoldCount = 0,
): SubagentGroupNode {
  const decorated = decoratedSubagentAggregates(parent);
  const fold = aggregates?.(parent.id);
  const liveTotal = loadedDescendantCount + (fold?.evictedCount ?? 0) + flattenedFoldCount;
  return {
    kind: 'group',
    parent,
    groupKey: parent.id,
    children,
    descendantCount: Math.max(liveTotal, decorated.count),
    loadedDescendantCount,
    latestChildSummary: pickLatestChildSummary(children, fold) || decorated.summary,
  };
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
  if (node.kind === 'activity_run') return `ar:${node.threadId}:${node.runId}`;
  const _exhaustive: never = node;
  return _exhaustive;
}

/** Item id of the leaf or group root. */
export function timelineNodeItemId(node: TimelineNode): string {
  if (node.kind === 'leaf') return node.item.id;
  if (node.kind === 'group' || node.kind === 'wait_group') return node.parent.id;
  if (node.kind === 'read_group') return node.members[0].id;
  if (node.kind === 'activity_run') return timelineNodeItemId(node.children[0]);
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
    // A run is the activity block, so it takes the block's role wholesale.
    // This deliberately gaps junctions that a leading or trailing `thinking`
    // row used to swallow (thinking is 'other', which short-circuits the
    // comparison): whether a block opens with thinking or with a Bash call
    // is arbitrary to a reader who sees "prose, activity, prose" either way.
    || node.kind === 'activity_run'
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
  // Load-bearing for jump-to-item: a search hit, review jump, target flash,
  // or restore anchor inside a run must resolve to the run's row so the run
  // can reveal the item, not fail to scroll.
  if (node.kind === 'activity_run') {
    return node.children.some((child) => nodeContainsItem(child, itemId));
  }
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
 * input and returns a fresh tree each call. `aggregates` (optional) is
 * the pane's live-eviction fold accessor; group nodes fold its evicted
 * counts and terminal previews into their collapsed-card aggregates.
 */
export function groupItemsBySubagent(
  items: readonly Item[],
  aggregates?: SubagentLiveAggregates,
): TimelineNode[] {
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

  // Suppression is transitive over a suppressed anchor's ENTIRE descendant
  // subtree, not just its direct children. A backgrounded/spawned launch can
  // have a non-suppressed launch nested inside it (e.g. a foreground Agent
  // launched from within a backgrounded one — occurs in real Claude
  // sessions); if only direct children were dropped, that nested launch's
  // own children would keep a parentId the filter doesn't recognize, fall
  // through to the orphan branch, and render at the TOP LEVEL of the
  // timeline — briefly, until they settle and the eviction sweep in
  // thread.svelte.ts resolves the chain against raw pane.items and folds
  // them. Running rows can't be evicted, so that's the flash this set
  // exists to prevent.
  //
  // One forward pass over the sorted items suffices: a launch always
  // precedes its descendants after the canonical sort, because descendants
  // inherit the launch's turn_index and item_index is assigned in
  // intended-appearance order (docs/architecture/invariants.md #10/#11 —
  // the same ordering collectSettledSubtree in thread.svelte.ts relies on).
  // So by the time an item is visited, its parent's anchor/suppressed
  // status is already recorded. When no suppressed anchors exist this
  // costs only empty-set lookups on items that carry a parentId.
  const suppressedDescendantIDs = new Set<string>();
  for (const item of sortedWithCarriers) {
    itemByID.set(item.id, item);
    if (isCodexSubagentLaunchItem(item)) {
      codexSpawnIDs.add(item.id);
    }
    if (isBackgroundSubagentLaunch(item)) {
      backgroundSubagentIDs.add(item.id);
    }
    const parentID = item.parentId ?? '';
    if (
      parentID
      && (codexSpawnIDs.has(parentID)
        || backgroundSubagentIDs.has(parentID)
        || suppressedDescendantIDs.has(parentID))
    ) {
      suppressedDescendantIDs.add(item.id);
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
    if (suppressedDescendantIDs.has(item.id)) return false;
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
      // Flattened nested launches contribute their evicted-fold counts
      // here because they render as leaves, not fold-aware group nodes.
      const flatChildren: TimelineNode[] = [];
      let flattenedFoldCount = 0;
      const queue: Item[] = [...(childItems ?? [])];
      for (let head = 0; head < queue.length; head++) {
        const next = queue[head];
        flatChildren.push({ kind: 'leaf', item: next });
        if (subagentLaunchKind(next) !== null) {
          flattenedFoldCount += aggregates?.(next.id)?.evictedCount ?? 0;
        }
        const grand = childrenByParent.get(next.id);
        if (grand) queue.push(...grand);
      }
      return subagentGroupNode(
        item,
        flatChildren,
        flatChildren.length,
        aggregates,
        flattenedFoldCount,
      );
    }

    const children = (childItems ?? []).map((child) => buildNode(child, depth + 1));
    return subagentGroupNode(item, children, countDescendants(children), aggregates);
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
