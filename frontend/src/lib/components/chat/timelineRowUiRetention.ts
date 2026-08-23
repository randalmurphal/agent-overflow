import type { Item } from '../../types/models';
import { compositeKey } from '../../utils/compositeKey';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { isRowUiRetentionActive } from '../../utils/rowUiRetention';
import type {
  PayloadExpansionRetentionKey,
  RowUiStateRetention,
} from '../../stores/threadRowUiState.svelte';

interface RetentionAccumulator {
  itemIds: Set<string>;
  payloads: Map<string, PayloadExpansionRetentionKey>;
  groupKeys: Set<string>;
}

export interface TimelineRowUiRetentionRange {
  first: number;
  last: number;
}

export interface TimelineRowUiRetentionOptions {
  nodeBuffer: number;
  tailNodeCount: number;
  isGroupExpanded(groupKey: string): boolean;
  /**
   * `pane.getItemById`. Timeline nodes carry the item refs they were
   * built from, and those are frozen at the last STRUCTURAL rebuild —
   * a row whose `payloadId` or `status` arrived after it would otherwise
   * be retained under a key nothing holds, and the key it actually holds
   * would be disposed out from under a mounted reader (the row-remount
   * contract in `components/chat/AGENTS.md`). Resolving by id at pass
   * cadence is the same "row boundary resolves current content" rule the
   * row components follow.
   */
  resolveItem(itemId: string): Item | undefined;
}

export interface TimelineRowUiPruneSignatureInputs {
  threadId: string | null;
  timelineRevision: number;
  revealTurnIndex: number | string;
  revealItemIndex: number | string;
  nodesLength: number;
  /**
   * `pane.activityRuns.revision` — a run's mount window changes which of its
   * children are retained, and it moves without changing structure, node
   * count, or the visible range.
   */
  activityRunRevision: number;
  range: TimelineRowUiRetentionRange;
  /**
   * `pane.rowUiRetentionRevision` — bumped by the store, at write time,
   * whenever an item write changed which rows the active-item pass
   * retains or what it retains for one (see
   * `utils/rowUiRetention.ts#rowUiRetentionChanged`).
   */
  rowUiRetentionRevision: number;
}

// Dedupe signature for a prune run. Captures every input the retention
// collection depends on (window position, structure revision, reveal
// gate, active-row membership) as SCALARS, so a no-op prune bails
// before allocating retention sets and without touching the node tree
// or the item list. The active-row leg is a store-maintained revision
// rather than a walk of `pane.items`: the walk cost O(loaded items) per
// prune callback (and, while the array was a deep `$state` proxy, a
// re-created source per index after every upsert batch), which was a
// main-thread wedge at the window cap mid-turn. The store proves the
// no-op instead, per changed row.
export function timelineRowUiPruneSignature(inputs: TimelineRowUiPruneSignatureInputs): string {
  return [
    inputs.threadId,
    inputs.timelineRevision,
    inputs.revealTurnIndex,
    inputs.revealItemIndex,
    inputs.nodesLength,
    inputs.activityRunRevision,
    inputs.range.first,
    inputs.range.last,
    inputs.rowUiRetentionRevision,
  ].join('|');
}

export function collectTimelineRowUiRetention(
  nodes: readonly TimelineNode[],
  items: readonly Item[],
  range: TimelineRowUiRetentionRange,
  options: TimelineRowUiRetentionOptions,
): RowUiStateRetention {
  const retained: RetentionAccumulator = {
    itemIds: new Set<string>(),
    payloads: new Map<string, PayloadExpansionRetentionKey>(),
    groupKeys: new Set<string>(),
  };
  const retainStart = Math.max(0, range.first - options.nodeBuffer);
  const retainEnd = Math.min(nodes.length - 1, range.last + options.nodeBuffer);
  const tailStart = Math.max(0, nodes.length - options.tailNodeCount);

  for (let index = retainStart; index <= retainEnd; index += 1) {
    retainNode(retained, nodes[index], options);
  }
  for (let index = tailStart; index < nodes.length; index += 1) {
    retainNode(retained, nodes[index], options);
  }

  const activeItemIds = new Set<string>();
  for (const item of items) {
    if (!isRowUiRetentionActive(item)) continue;
    activeItemIds.add(item.id);
    // No resolve: this leg walks the live item list, not the nodes.
    retainItem(retained, item);
  }
  if (activeItemIds.size > 0) {
    for (const node of nodes) {
      retainActiveGroupKeys(retained, node, activeItemIds);
    }
  }

  return {
    itemIds: retained.itemIds,
    payloads: [...retained.payloads.values()],
    groupKeys: retained.groupKeys,
  };
}

type RetainableItem = Pick<Item, 'id' | 'threadId' | 'payloadId'>;

/**
 * The row this node describes AS IT IS NOW, or the node's own snapshot
 * when the store no longer holds it.
 *
 * The fallback is the right answer for an id that does not resolve, not a
 * concession: a row leaves the window by being pruned or by being folded
 * into a subagent aggregate, and in both cases the last thing known about
 * it is what the node carries. Retaining that costs one entry in a set
 * already bounded by the node band, and it keeps the reader's expansion
 * alive across the fold/unfold round trip; dropping it would dispose
 * state a card re-expansion immediately asks for. A row that is genuinely
 * gone for good stops appearing in the node band on the next structural
 * pass, and its state is disposed then.
 */
function currentItem(
  options: TimelineRowUiRetentionOptions,
  embedded: RetainableItem,
): RetainableItem {
  return options.resolveItem(embedded.id) ?? embedded;
}

function retainItem(
  retained: RetentionAccumulator,
  item: RetainableItem,
): void {
  retained.itemIds.add(item.id);
  if (!item.payloadId) return;
  // The same key the expansion registry files this payload under, built
  // by the same helper so the two cannot drift.
  const payloadKey = compositeKey(item.threadId, item.payloadId);
  retained.payloads.set(payloadKey, {
    threadId: item.threadId,
    payloadId: item.payloadId,
  });
}

function retainNode(
  retained: RetentionAccumulator,
  node: TimelineNode,
  options: TimelineRowUiRetentionOptions,
): void {
  if (node.kind === 'leaf') {
    retainItem(retained, currentItem(options, node.item));
    return;
  }

  if (node.kind === 'read_group') {
    for (const item of node.members) retainItem(retained, currentItem(options, item));
    return;
  }

  if (node.kind === 'activity_run') {
    // The run's mount window, not its whole child list. The buffer above is
    // counted in NODES and a run collapses many rows into one node, so
    // without a bound of its own 48 runs of 200 rows inside the band would
    // retain ~9600 items where the flat timeline retained 96. The window is
    // exactly what the run can have in the DOM, so this agrees with the
    // mounted rows by construction. Rows outside it cannot hold an expansion
    // handle: mounted rows keep theirs alive through their own lease
    // (disposal is deferred while leased), and every running/streaming item
    // is retained unconditionally by the active-item pass in
    // `collectTimelineRowUiRetention`.
    const end = Math.min(node.children.length, node.mountedFrom + node.mountedRows);
    for (let i = node.mountedFrom; i < end; i += 1) {
      retainNode(retained, node.children[i], options);
    }
    return;
  }

  retained.groupKeys.add(node.groupKey);
  retainItem(retained, currentItem(options, node.parent));

  if (node.kind === 'group' && !options.isGroupExpanded(node.groupKey)) return;
  for (const child of node.children) {
    retainNode(retained, child, options);
  }
}

function retainActiveGroupKeys(
  retained: RetentionAccumulator,
  node: TimelineNode,
  activeItemIds: ReadonlySet<string>,
): boolean {
  if (node.kind === 'leaf') {
    return activeItemIds.has(node.item.id);
  }
  if (node.kind === 'read_group') {
    return node.members.some((item) => activeItemIds.has(item.id));
  }

  if (node.kind === 'activity_run') {
    // A run is a presentation wrapper with no groupKey of its own; walk
    // through it so a subagent card inside a run still registers its key.
    let containsActive = false;
    for (const child of node.children) {
      if (retainActiveGroupKeys(retained, child, activeItemIds)) containsActive = true;
    }
    return containsActive;
  }

  let containsActiveItem = activeItemIds.has(node.parent.id);
  for (const child of node.children) {
    if (retainActiveGroupKeys(retained, child, activeItemIds)) {
      containsActiveItem = true;
    }
  }
  if (containsActiveItem) {
    retained.groupKeys.add(node.groupKey);
  }
  return containsActiveItem;
}
