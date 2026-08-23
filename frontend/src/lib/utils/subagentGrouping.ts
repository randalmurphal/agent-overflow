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
//   - Every subagent launch gets ONE card (a `group` node) — Claude
//     `Agent`/`Task` (awaited or async), a forked `Skill`, a §E6
//     `SendMessage` resume carrier, and a Codex `spawn_agent`. One
//     predicate decides (`utils/subagentLaunch.ts#subagentLaunchInfo`);
//     nothing here knows a tool name. A launch nested inside another
//     launch becomes a nested group, recursively.
//   - WHERE the card sits depends on whether the launch runs detached
//     (`launchRunsDetached`: a background / async Claude launch, one
//     backgrounded mid-flight, every Codex spawn):
//       - an AWAITED launch is the card from first render, anchored on
//         the launch row itself (`anchor === parent`);
//       - a DETACHED launch keeps its pre-card launch row as a plain leaf
//         (ruling 2026-08-23: that row is immutable — the only addition is
//         the open-pane door) and its card renders at its COMPLETION
//         sibling (the Go `complete:<id>` row, `completionOf` = the
//         launch), with `anchor` = that sibling and the launch's whole
//         subtree as children. No card exists while the agent runs; the
//         tray and the pane are its live surfaces. A completion observed
//         by a Codex `wait_agent` renders as that card under the wait
//         group, so `WaitGroupNode.children` are nodes, not leaves.
//   - Wait carriers use a stable structural wrapper from first render; Codex
//     subagent target completions render beneath them when linked by
//     `wait_carrier_id` or shared wait payload correlation.
//   - Every row inside a launch's subtree renders inside that launch's card,
//     but only a LAUNCH ever becomes a group: a row whose parent is an
//     ordinary tool call attaches to its nearest launch ANCESTOR as a flat
//     sibling, rather than turning that tool call into a container. Generic
//     parentId nesting would let an already-rendered row flip from leaf to
//     group mid-turn, which is exactly what this projection exists to avoid.
//     A row with no launch anywhere above it stays a top-level leaf.
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
import {
  isPotentialSubagentLaunch,
  launchRunsDetached,
  subagentDescendantCountFromMeta,
  subagentLaunchContextFrom,
  subagentLaunchInfo,
} from './subagentLaunch';
import { reportFrontendDiagnostic } from './frontendErrorCapture';

/**
 * Accessor into the pane's live-eviction fold registry. Group nodes add
 * the evicted-terminal count and compete the folded terminal preview
 * against loaded children, so a collapsed card renders identically
 * whether its settled transcript rows are in memory or evicted.
 */
export type SubagentLiveAggregates = (
  anchorId: string,
) => SubagentFoldAggregate | undefined;

export const MAX_DEPTH = 3;
const PREVIEW_MAX_CHARS = 160;
const PREVIEW_SCAN_CHARS = 512;
// Not global: `test` on a `/g` regex advances `lastIndex` and would
// answer differently on alternate calls.
const NON_WHITESPACE = /\S/;

// The one window both `normalizePreviewText` and `hasPreviewText` look
// at — sharing it is what keeps "has a preview" structurally equal to
// "the preview is non-empty" on the window half of that contract (the
// other half, `\S` ⟺ collapse-to-empty, is asserted by test).
function previewScanWindow(summary: string): string {
  return summary.length > PREVIEW_SCAN_CHARS
    ? summary.slice(0, PREVIEW_SCAN_CHARS)
    : summary;
}

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
  /**
   * The launch tool-call this card IS: its identity (label, model,
   * description), its expansion key, and the bucket its children were
   * collected under. Always the launch, wherever the card sits.
   */
  parent: Item;
  /**
   * The item whose POSITION the card occupies, and the id the card
   * answers to for ordering, reveal gating, scroll anchors and
   * jump-to-item (`timelineNodeItemId`, `nodeContainsItem`):
   *
   *   - an AWAITED launch completes in place, so the card sits at the
   *     launch and `anchor === parent`;
   *   - a DETACHED Claude launch (`launchRunsDetached`: async,
   *     run_in_background, a resume carrier, or backgrounded mid-flight)
   *     keeps its pre-card launch row — a compact leaf that never changes
   *     — and the card sits at the launch's `complete:<id>` sibling,
   *     after everything the main thread wrote while the agent ran.
   *     There `anchor === completion`. Until that sibling loads there is
   *     no card at all: the agent's live transcript is the pane's and the
   *     tray's, never the launch row's (user ruling 2026-08-23).
   *   - a Codex spawn is detached by definition and gets the same shape:
   *     the collab `launched` leaf at the launch, the card at its
   *     completion sibling. When the parent `wait_agent`ed on the child,
   *     that sibling is a child of the wait group, and the card renders
   *     there (`WaitGroupNode.children` are nodes, not only leaves).
   */
  anchor: Item;
  /** Stable structural key for virtualization and expansion state. */
  groupKey: string;
  /**
   * The launch's background completion sibling (the Go `complete:<id>`
   * row: `kind:'tool_completion'`, `completionOf === parent.id`, empty
   * parentId) once it has loaded. It carries the outcome an async agent
   * reports at terminal — status, duration, the final report payload —
   * which the launch row itself never receives, because a backgrounded
   * launch stays `running` forever by design (the tray invariant). The
   * card renders it as its status/result source, and — for a detached
   * launch — sits AT it (`anchor`), so the sibling's position in the
   * transcript is exactly where the card renders. That is load-bearing:
   * the bell (`notification` row) is hidden whenever a completed sibling
   * exists (utils/notificationFilter.ts), so the card at the completion
   * point is the only in-sequence evidence that the agent finished. A
   * version that folded the sibling onto a card sitting at the LAUNCH and
   * dropped the sibling's row erased every trace of the completion from
   * the point where it happened (live regression 2026-08-22; tripwire
   * `backgroundCompletionVisibility.test.ts`).
   *
   * Undefined while the agent is still running and for an awaited launch
   * (which completes in place and has no sibling at all). At a page
   * boundary where the sibling loaded but the launch did not, the sibling
   * renders as a compact leaf of its own and no card exists.
   *
   * NOT counted in `descendantCount` and not a preview candidate of ITS
   * OWN card: it is that card's header source, not its transcript.
   */
  completion?: Item;
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
   * Codex subagent target completion rows observed by this wait. A
   * completion whose launch is a detached spawn renders as that spawn's
   * CARD (`SubagentGroupNode.anchor === completion`); any other observed
   * completion is a leaf. Terminal command completions intentionally
   * render as sibling rows.
   */
  children: TimelineNode[];
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
   * This run is the newest node the reader can SEE — the last node in the
   * revealed array. Wider than `live`, which additionally requires that
   * nothing foreign waits behind the reveal gate: closing prose that has
   * arrived on the wire ends `live` while the reader is still watching this
   * run stream. Collapse resolution keys on this
   * (`ActivityRunIdentity.collapsedFor`), and so does the row's scroll
   * controller lifetime, and the auto-collapse gate's skip — tearing the
   * controller down at `live`'s edge cancelled a mid-stream glide and
   * snapped the remainder in one frame (2026-08-19). Under monotonic reveal
   * it flips at most twice per run, same as `live`; truncation
   * (edit-and-resend revert) and late rail membership can hand the tail
   * back, which the controller's snapshot/pin restore covers.
   */
  atTail: boolean;
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
   * Registry-assigned membership stamp — see `ActivityRunResolution`.
   * The header pairs it with the pane's per-run content revision and the
   * registry's wholesale-replace generation to key its summary, so no
   * change any of the three covers needs a walk over `summaryItemIds`.
   */
  membershipEpoch: number;
  /**
   * Positional identity items for the run, group members included, in
   * timeline order. A detached card names its completion anchor here, never
   * the launch leaf that already belongs to an earlier run.
   */
  memberItemIds: readonly string[];
  /**
   * Current rows the header summarizes. Counts, failure, and the running
   * label come from the current items behind these ids rather than a snapshot
   * baked into the node. Separate from identity membership
   * because an immutable detached launch depends on its later completion,
   * which is also the positional anchor of another run.
   */
  summaryItemIds: readonly string[];
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
  const normalized = previewScanWindow(summary).replace(/\s+/g, ' ').trim();
  if (normalized.length <= PREVIEW_MAX_CHARS) return normalized;
  return `${normalized.slice(0, PREVIEW_MAX_CHARS).trimEnd()}...`;
}

/**
 * Whether an item contributes user-visible text (assistant messages,
 * thinking, tool summaries) — false for items whose summary is non-text
 * noise, which are not preview candidates.
 *
 * Exactly `normalizePreviewText(summary) !== ''` without building the
 * preview: that function returns empty precisely when its scan window
 * holds no non-whitespace character, and `\S` finds the first one and
 * stops. The candidate walk asks this of every descendant on every
 * streaming tick of any of them, so it must not allocate — normalizing
 * is deferred to the one item that wins.
 */
function hasPreviewText(item: Item): boolean {
  return NON_WHITESPACE.test(previewScanWindow(item.summary ?? ''));
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
  const count = subagentDescendantCountFromMeta(meta);
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
  // A card's position is its anchor's: the launch for an awaited card, the
  // completion sibling for a detached launch's card (see
  // `SubagentGroupNode.anchor`).
  if (node.kind === 'group') return node.anchor;
  if (node.kind === 'wait_group') return node.parent;
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
    if (node.kind === 'group' || node.kind === 'wait_group') {
      // A folded `completion` is deliberately NOT yielded by either group
      // kind: this walk feeds the collapsed-header preview, and the
      // completion is the header's own status row. Yielding it would make
      // every finished background agent's preview read "…-> done" instead
      // of the last thing the agent actually did.
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
 * Every item id rendered somewhere inside `nodes` — the reach of the
 * auto-collapse gate's engagement peek (`hasUserExpansionWithin`), which
 * must see an expansion on ANY row a reader can touch, however deeply the
 * grouping nested it. Unlike `descendantItems` (the collapsed-header
 * preview scope) this includes a group's folded completion: that row
 * renders AS the group header, and its payload expands like any other.
 */
export function* renderedItemIdsWithin(
  nodes: readonly TimelineNode[],
): Generator<string> {
  for (const node of nodes) {
    switch (node.kind) {
      case 'leaf':
        yield node.item.id;
        break;
      case 'read_group':
        for (const member of node.members) yield member.id;
        break;
      case 'group':
      case 'wait_group':
        yield node.parent.id;
        // Both group kinds can fold a completion in AS the header; its
        // payload expands like any other row, so the engagement peek has
        // to be able to see an expansion on it.
        if (node.completion) yield node.completion.id;
        yield* renderedItemIdsWithin(node.children);
        break;
      case 'activity_run':
        yield* renderedItemIdsWithin(node.children);
        break;
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
 *
 * `getItem` re-resolves each descendant against the store, which is how
 * the mounted card tracks a streaming child: the node tree is a
 * structural snapshot, so without it the preview would only move when
 * the timeline's shape changed. Omitting it reads the snapshot as-is,
 * which is what the pure grouping pass wants.
 */
export function pickLatestChildSummary(
  children: TimelineNode[],
  fold?: SubagentFoldAggregate,
  getItem?: (id: string) => Item | undefined,
): string {
  // Candidates are carried as ITEMS, not as items paired with their
  // rendered preview: only the winner's text is ever used, and building
  // a preview per candidate meant a whitespace-collapse pass and two
  // string allocations for every descendant on every re-run. The walk
  // is O(descendants) and re-runs whenever any descendant's row is
  // rewritten (this reads each one through `getItem`, which is the
  // point — that is how a streaming child moves the preview), so the
  // per-candidate cost is what matters, not the loop.
  let bestActive: Item | null = null;
  let bestTerminal: Item | null = null;
  for (const snapshot of descendantItems(children)) {
    const item = getItem?.(snapshot.id) ?? snapshot;
    if (!hasPreviewText(item)) continue;
    if (isItemActive(item)) {
      if (!bestActive || compareItems(item, bestActive) > 0) bestActive = item;
    } else if (!bestActive) {
      // Only track terminals while no active descendant is in the
      // running. Once an active candidate appears we stop bothering
      // with terminals — they can't beat an active winner regardless
      // of order.
      if (!bestTerminal || compareItems(item, bestTerminal) > 0) bestTerminal = item;
    }
  }
  if (bestActive) return normalizePreviewText(bestActive.summary ?? '');
  if (fold && fold.terminalPreview) {
    if (
      !bestTerminal
      || fold.terminalTurnIndex > bestTerminal.turnIndex
      || (fold.terminalTurnIndex === bestTerminal.turnIndex
        && fold.terminalItemIndex >= bestTerminal.itemIndex)
    ) {
      return fold.terminalPreview;
    }
  }
  return bestTerminal ? normalizePreviewText(bestTerminal.summary ?? '') : '';
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
  /** Where the card sits: the launch, or its completion sibling (see `SubagentGroupNode.anchor`). */
  anchor: Item,
  children: TimelineNode[],
  loadedDescendantCount: number,
  aggregates: SubagentLiveAggregates | undefined,
  /**
   * The launch's background completion sibling, when one has loaded. Folded
   * onto the node as its status source; the FOLD adds nothing to the
   * counts or the preview (a finished agent with an empty transcript must
   * not wedge its body on "Loading 1 entries…").
   */
  completion?: Item,
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
    anchor,
    groupKey: parent.id,
    ...(completion ? { completion } : {}),
    children,
    descendantCount: Math.max(liveTotal, decorated.count),
    loadedDescendantCount,
    latestChildSummary: pickLatestChildSummary(children, fold) || decorated.summary,
  };
}

/**
 * The derived `subagentGroupExpanded` registry keys an anchor item can own
 * next to its bare id (a subagent group's `groupKey` IS its parent id):
 * `wait:` minted for wait groups below, `reads:` by `groupConsecutiveReads`
 * (`readGrouping.ts`). Minting, clearing (`disposeItems`), and the
 * engagement peek (`hasUserExpansionWithin`) all build the keys here, so a
 * new derived key cannot join one side and silently miss the others.
 */
export function waitGroupKey(anchorItemId: string): string {
  return `wait:${anchorItemId}`;
}

export function readGroupKey(anchorItemId: string): string {
  return `reads:${anchorItemId}`;
}

/** Every `subagentGroupExpanded` key `itemId` can own. */
export function subagentGroupKeysFor(itemId: string): [string, string, string] {
  return [itemId, waitGroupKey(itemId), readGroupKey(itemId)];
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

/**
 * Item id of the leaf or structural node's root — the row the node
 * RENDERS at its position. For a subagent card that is its anchor: a
 * detached launch's own id belongs to its spawn leaf, so a scroll anchor
 * or jump naming that id lands on the spawn row, never on the card
 * sitting at the completion point turns later.
 */
export function timelineNodeItemId(node: TimelineNode): string {
  if (node.kind === 'leaf') return node.item.id;
  if (node.kind === 'group') return node.anchor.id;
  if (node.kind === 'wait_group') return node.parent.id;
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
  activeTurnKey: number | null,
  turnKeyOf: (item: Item) => number = itemTurnIndexKey,
): Set<string> {
  const lastByTurn = new Map<number, string>();
  for (const node of nodes) {
    if (node.kind !== 'leaf') continue;
    if (node.item.kind !== 'assistant_text') continue;
    lastByTurn.set(turnKeyOf(node.item), node.item.id);
  }
  if (activeTurnKey !== null) {
    lastByTurn.delete(activeTurnKey);
  }
  return new Set(lastByTurn.values());
}

/**
 * The default turn key: the provider turn the row was written in. The
 * agent pane's scoped facade substitutes one key for its whole window
 * (`ThreadPane.timelineTurns`), because a subagent's rows straddle the
 * main thread's turns while belonging to ONE run.
 */
export function itemTurnIndexKey(item: Item): number {
  return item.turnIndex;
}

/** The item whose position represents `node` (leaf, anchor, or first member). */
export function timelineNodeRepresentativeItem(node: TimelineNode): Item {
  return timelineNodeRootItem(node);
}

/**
 * Recursive containment check: does `node` (or any descendant of a group
 * node) carry an item with this id?
 */
export function nodeContainsItem(node: TimelineNode, itemId: string): boolean {
  if (node.kind === 'leaf') return node.item.id === itemId;
  if (node.kind === 'group') {
    // A card claims the row it renders AT (its anchor) and its children.
    // For an awaited launch the anchor IS the launch; for a detached one
    // it is the completion sibling, and the launch id is deliberately
    // NOT claimed: that row is the spawn leaf, rendered on its own
    // turns earlier, and a scroll anchor or jump naming it must land
    // there (see `timelineNodeItemId`).
    return node.anchor.id === itemId
      || node.children.some((child) => nodeContainsItem(child, itemId));
  }
  if (node.kind === 'wait_group') {
    // A wait group's folded `completion` is rendered AS its header and
    // exists nowhere else in the node array, so a search hit on that
    // completion's own id (Go indexes its summary) must resolve to this
    // row or fail to scroll.
    return node.parent.id === itemId
      || node.completion?.id === itemId
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

/**
 * The id of the top-level timeline row that REPRESENTS `itemId`.
 *
 * Fallback for jump-to-item when `findTimelineNodeIndex` misses outright:
 * every loaded row is reachable in the tree today (children of a launch
 * nest inside its group; a depth-capped descendant flattens into the
 * deepest allowed group as a leaf), but a caller may hold an id whose row
 * is folded, evicted, or otherwise absent from the node array it is
 * searching. Walking `parentId` — and, for a launch's background
 * completion sibling, `completionOf` — up to the outermost enclosing
 * launch names the row the reader can actually be scrolled to.
 *
 * Provider-neutral: the ancestor only has to BE a launch, whichever kind.
 * Returns `itemId` unchanged when nothing above it is one (a row parented
 * under a non-launch renders flat, so it is already its own row).
 */
export function visibleTimelineItemIdForItem(items: readonly Item[], itemId: string): string {
  const byID = new Map<string, Item>();
  for (const item of items) byID.set(item.id, item);
  const item = byID.get(itemId);
  if (!item) return itemId;
  const ctx = subagentLaunchContextFrom(items);

  let current = item;
  let visible = itemId;
  // `byID.size` bounds the walk: a parentId cycle in provider data must not
  // spin here (the same guard the depth-cap flatten carries).
  for (let hops = 0; hops < byID.size; hops++) {
    const nextID = current.parentId || (current.completionOf ?? '');
    if (!nextID) break;
    const next = byID.get(nextID);
    if (!next) break;
    // A completionOf hop only counts when it lands on a launch — an
    // ordinary tool's completion is its own row, not a folded header.
    const isLaunch = subagentLaunchInfo(next, ctx) !== null;
    if (!current.parentId && !isLaunch) break;
    if (isLaunch) visible = next.id;
    current = next;
  }
  return visible;
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

  // Fast path: if no item declares a parentId, no item could be a subagent
  // launch, AND the input is already in canonical order, there is nothing
  // to group. Skip the sort, id-set build, and grouping walk entirely —
  // just wrap each item as a leaf. ThreadPane.upsertItem maintains
  // canonical order on insertion, so MessageTimeline (the hot caller) hits
  // this path for the common no-subagent thread.
  //
  // The launch test here is the cheap tool-name PREFILTER, not the real
  // predicate: it parses no meta, and being a superset it can only cost an
  // unnecessary grouping walk, never miss a launch.
  //
  // The monotonic check preserves the documented contract that callers
  // need not pre-sort: if items arrive out of order we fall through to
  // the slow path which sorts defensively.
  //
  // Measured (N=500 items, common no-subagent case): 25us -> 3us per call,
  // a ~9x win. Threads with subagents are the minority; the grouping logic
  // below is still exercised for them.
  let hasGroupingSignals = false;
  let alreadySorted = true;
  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    const pid = item.parentId;
    if ((pid && pid.length > 0) || item.completionOf || item.kind === 'terminal_interaction' || item.toolName === 'wait_agent' || isPotentialSubagentLaunch(item)) {
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
  const waitCompletionPayloadCarrierByPayloadID = new Map<string, string>();
  const waitChildrenByCarrierID = new Map<string, Item[]>();
  const waitChildIDs = new Set<string>();
  const waitCompletionByCarrierID = new Map<string, Item>();

  // Every launch in the window, provider-neutral. The context is built over
  // the same list, so forked-Skill detection sees exactly the rows this pass
  // is about to place.
  const launchContext = subagentLaunchContextFrom(sortedWithCarriers);
  const subagentLaunchIDs = new Set<string>();
  // Launches that run detached from the main turn (`launchRunsDetached`),
  // every provider. Their launch row is the pre-card leaf and never a
  // group; their card sits at their completion sibling wherever that
  // sibling renders — top level, inside a parent card, or under the
  // `wait_agent` group that claimed it (user ruling 2026-08-23). See
  // `SubagentGroupNode.anchor`.
  const detachedLaunchIDs = new Set<string>();

  /** Returns whether the child was linked under the carrier. */
  function addWaitChild(carrierID: string, child: Item): boolean {
    const carrier = itemByID.get(carrierID);
    if (!carrier || !isWaitCarrier(carrier)) return false;
    if (isTerminalWaitCarrier(carrier) && child.toolName === 'command_execution') return false;
    if (waitChildIDs.has(child.id)) return false;
    const bucket = waitChildrenByCarrierID.get(carrierID);
    if (bucket) {
      bucket.push(child);
    } else {
      waitChildrenByCarrierID.set(carrierID, [child]);
    }
    waitChildIDs.add(child.id);
    return true;
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
    const launchInfo = subagentLaunchInfo(item, launchContext);
    if (launchInfo !== null) {
      subagentLaunchIDs.add(item.id);
      if (launchRunsDetached(launchInfo, parseJsonObject(item.meta))) {
        detachedLaunchIDs.add(item.id);
      }
    }
  }

  // A LAUNCH's background completion sibling folds into its launch's group
  // node as the status source. For a detached launch the sibling is ALSO
  // where that card renders (`cardLaunchByCompletionID` below): the
  // sibling's position becomes the card's, so nothing is dropped and
  // nothing renders twice.
  const launchCompletionByLaunchID = new Map<string, Item>();

  for (const item of sortedWithCarriers) {
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
    // Wait correlation is tried FIRST and wins: a Codex spawn's completion
    // carries `wait_carrier_id` (or shares the wait's payload), and its home
    // is the wait group that observed it — that is what "Finished waiting"
    // renders under. Only a completion no wait claimed falls through to its
    // own launch's card, which is the Claude `complete:<launchID>` shape.
    let linkedToWait = false;
    const explicitCarrierID = itemWaitCarrierID(item);
    if (explicitCarrierID) {
      linkedToWait = addWaitChild(explicitCarrierID, item);
    } else if (item.payloadId) {
      const carrierID = waitCompletionPayloadCarrierByPayloadID.get(item.payloadId);
      if (carrierID) linkedToWait = addWaitChild(carrierID, item);
    }
    if (!linkedToWait && subagentLaunchIDs.has(item.completionOf)) {
      launchCompletionByLaunchID.set(item.completionOf, item);
    }
  }
  // A detached launch's completion sibling: the position at which that
  // launch's card renders (see `SubagentGroupNode.anchor`). Keyed by the
  // sibling's id so `buildNode`, visiting the sibling at its own place —
  // its bucket, or the wait group that claimed it — builds the card
  // there. Every completion of a detached launch qualifies, wait-claimed
  // or not; an awaited launch that somehow carries a sibling keeps the
  // sibling as a leaf of its own.
  const cardLaunchByCompletionID = new Map<string, Item>();
  for (const item of sortedWithCarriers) {
    if (item.kind !== 'tool_completion' || !item.completionOf) continue;
    if (item.toolName === 'wait_agent') continue;
    const launch = itemByID.get(item.completionOf);
    if (launch && detachedLaunchIDs.has(launch.id)) {
      cardLaunchByCompletionID.set(item.id, launch);
    }
  }

  // A launch's background completion sibling is NOT filtered here: it is
  // the row the card is built AT — see `SubagentGroupNode.completion` for
  // why dropping it erased the completion from the timeline.
  const sorted = sortedWithCarriers.filter((item) => {
    if (waitChildIDs.has(item.id)) return false;
    if (item.kind === 'tool_completion' && item.completionOf && item.toolName === 'wait_agent') {
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

  // Bucket every row under the LAUNCH it belongs to. The bucket key is the
  // nearest launch ANCESTOR, not the immediate parent: a row parented under
  // an ordinary tool call that is itself inside an agent still belongs in
  // that agent's card, and letting it fall through to the top level is the
  // orphan-leak this projection is built to prevent. Only launches ever get
  // a bucket, so no ordinary row can flip from leaf to container.
  //
  // One forward pass suffices: a parent always precedes its children after
  // the canonical sort (invariants #10/#11), so `anchorByID` already holds
  // the answer for `pid` by the time a child is visited. A parentId cycle
  // therefore resolves to no anchor rather than looping.
  const childrenByParent = new Map<string, Item[]>();
  const anchorByID = new Map<string, string>();
  const orphanIds = new Set<string>();

  for (const item of sorted) {
    const pid = item.parentId ?? '';
    if (!pid) continue;
    if (!idSet.has(pid)) {
      orphanIds.add(item.id);
      continue;
    }
    const anchorID = subagentLaunchIDs.has(pid) ? pid : anchorByID.get(pid);
    if (anchorID === undefined) continue;
    anchorByID.set(item.id, anchorID);
    const bucket = childrenByParent.get(anchorID);
    if (bucket) {
      bucket.push(item);
    } else {
      childrenByParent.set(anchorID, [item]);
    }
  }

  // Stable chronological order within each bucket.
  for (const bucket of childrenByParent.values()) {
    bucket.sort(compareItems);
  }

  function buildNode(item: Item, depth: number): TimelineNode {
    if (isWaitCarrier(item)) {
      // An observed completion is a leaf — unless it is a detached
      // spawn's completion sibling, in which case it is where that
      // spawn's card renders (`cardLaunchByCompletionID`, via buildNode).
      const children = (waitChildrenByCarrierID.get(item.id) ?? [])
        .map((child) => buildNode(child, depth + 1));
      return {
        kind: 'wait_group',
        parent: item,
        groupKey: waitGroupKey(item.id),
        completion: waitCompletionByCarrierID.get(item.id),
        children,
        descendantCount: children.length,
      };
    }

    // A detached launch's completion sibling is where that launch's card
    // renders (`SubagentGroupNode.anchor`): the sibling's slot, the
    // launch's identity and subtree.
    const cardLaunch = cardLaunchByCompletionID.get(item.id);
    if (cardLaunch) return buildLaunchGroup(cardLaunch, item, depth);

    const isLaunch = subagentLaunchIDs.has(item.id);
    // A detached launch keeps its pre-card launch row: a leaf, whatever
    // sits under it. Its subtree renders under its card once the
    // completion sibling loads; until then the agent's live transcript is
    // the pane's and the tray's, never the launch row's.
    if (isLaunch && detachedLaunchIDs.has(item.id)) return { kind: 'leaf', item };

    const childItems = childrenByParent.get(item.id);
    if ((!childItems || childItems.length === 0) && !isLaunch) {
      return { kind: 'leaf', item };
    }
    return buildLaunchGroup(item, item, depth);
  }

  /**
   * The card for launch `item`, positioned at `anchor` (the launch itself
   * for an awaited launch, its completion sibling for a detached one).
   */
  function buildLaunchGroup(item: Item, anchor: Item, depth: number): SubagentGroupNode {
    const childItems = childrenByParent.get(item.id);
    if (depth >= MAX_DEPTH) {
      // Cap depth: render the deeper descendants as flat leaf siblings of
      // this node's parent instead of nesting further. The group still
      // reports the full descendant count so the collapsed card is honest.
      // Flattened nested launches contribute their evicted-fold counts
      // here because they render as leaves, not fold-aware group nodes.
      const flatChildren: TimelineNode[] = [];
      let flattenedFoldCount = 0;
      // `childrenByParent` is built from provider-supplied `parentId`s and
      // keyed by item id, neither of which this pass owns. A cycle in those
      // links is a synchronous allocate-until-dead loop (the queue grows
      // forever, one core pegged, nothing reported); a duplicate item id —
      // which a transport-gap replay can produce — re-walks a whole subtree
      // and renders it twice. A visited set closes both, and is the same
      // guard `threadTimelineWindow.svelte.ts`'s parent walk carries.
      //
      // EVERY enqueue goes through `enqueue`, the initial bucket included.
      // Seeding `visited` from that bucket instead would leave its own
      // duplicates un-deduped and unreported — and two leaves with the same
      // id are a duplicate key in the row `{#each}` downstream, which throws.
      // The append is iterative for a second reason: `push(...children)` is a
      // spread-apply, so a wide enough bucket throws RangeError instead of
      // flattening.
      const queue: Item[] = [];
      const visited = new Set<string>();
      let revisits = 0;
      const enqueue = (children: readonly Item[] | undefined): void => {
        if (!children) return;
        for (const child of children) {
          if (visited.has(child.id)) {
            revisits += 1;
            continue;
          }
          visited.add(child.id);
          queue.push(child);
        }
      };

      enqueue(childItems);
      for (let head = 0; head < queue.length; head++) {
        const next = queue[head];
        flatChildren.push({ kind: 'leaf', item: next });
        if (subagentLaunchIDs.has(next.id)) {
          flattenedFoldCount += aggregates?.(next.id)?.evictedCount ?? 0;
          // A flattened launch renders as a LEAF with no card to build at
          // its completion sibling; the sibling is a leaf of its own in
          // this same bucket (it carries the launch's parentId), so it
          // arrives through `enqueue` like any other descendant.
        }
        enqueue(childrenByParent.get(next.id));
      }
      if (revisits > 0) {
        // Surviving the corruption silently would leave it in place and the
        // transcript quietly short; only the report says why. Constant
        // message, variables in `detail` — an id in the message would mint a
        // fresh signature per launch and walk straight past the per-signature
        // cap. Console too: a remote session cannot persist (the reporter is
        // LocalOnly), and there the console line is the only evidence.
        const detail = `launch ${item.id}, ${revisits} skipped`;
        console.warn(`[subagentGrouping] corrupt parent links while flattening (${detail})`);
        reportFrontendDiagnostic(
          'subagentGrouping: corrupt parent links under a subagent launch — already-visited ' +
            'descendant(s) skipped while flattening at the depth cap (a parentId cycle, or a ' +
            'duplicate item id)',
          detail,
        );
      }
      return subagentGroupNode(
        item,
        anchor,
        flatChildren,
        flatChildren.length,
        aggregates,
        cardCompletion(item, anchor),
        flattenedFoldCount,
      );
    }

    // `childItems` is this launch's whole subtree in timeline order, so a
    // child that is itself a launch recurses into its own nested card and
    // everything else stays a sibling leaf right here.
    const children = (childItems ?? []).map((child) => buildNode(child, depth + 1));
    return subagentGroupNode(
      item,
      anchor,
      children,
      countDescendants(children),
      aggregates,
      cardCompletion(item, anchor),
    );
  }

  /**
   * The completion sibling a card folds in as its status source. For a
   * card at its completion point the anchor IS that sibling — including a
   * wait-claimed Codex completion, which the launch fold never recorded.
   */
  function cardCompletion(item: Item, anchor: Item): Item | undefined {
    return anchor.id !== item.id ? anchor : launchCompletionByLaunchID.get(item.id);
  }

  const roots: TimelineNode[] = [];
  for (const item of sorted) {
    // Anything a launch claimed renders inside that launch's card.
    if (anchorByID.has(item.id)) continue;
    const pid = item.parentId ?? '';
    if (!pid) {
      roots.push(buildNode(item, 0));
      continue;
    }
    if (orphanIds.has(item.id)) {
      roots.push({ kind: 'leaf', item, orphan: true });
      continue;
    }
    // A declared parent that exists but sits outside every launch: the row
    // stays a flat top-level leaf, unflagged.
    roots.push({ kind: 'leaf', item });
  }

  return roots;
}
