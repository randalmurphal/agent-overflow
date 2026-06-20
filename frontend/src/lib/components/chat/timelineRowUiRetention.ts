import type { Item } from '../../types/models';
import {
  timelineNodeKey,
  type TimelineNode,
} from '../../utils/subagentGrouping';
import type {
  PayloadExpansionRetentionKey,
  RowUiStateRetention,
} from '../../stores/threadRowUiState.svelte';

interface RetentionAccumulator {
  itemIds: Set<string>;
  payloads: Map<string, PayloadExpansionRetentionKey>;
  groupKeys: Set<string>;
  rowGeometryKeys: Set<string>;
}

export interface TimelineRowUiRetentionRange {
  first: number;
  last: number;
}

export interface TimelineRowUiRetentionOptions {
  nodeBuffer: number;
  tailNodeCount: number;
  isGroupExpanded(groupKey: string): boolean;
}

export interface TimelineRowUiPruneSignatureInputs {
  threadId: string | null;
  timelineRevision: number;
  revealTurnIndex: number | string;
  revealItemIndex: number | string;
  nodesLength: number;
  range: TimelineRowUiRetentionRange;
  items: readonly Item[];
}

// Dedupe signature for a prune run. Captures every input the retention
// collection depends on (window position, structure revision, reveal
// gate, active-row membership) WITHOUT walking the node tree, so a
// no-op prune bails before allocating retention sets. Computed at
// prune cadence, NOT as a reactive derived — `pane.items` churns its
// array reference on every streaming delta, and a derived tracking it
// would walk the full item list per chunk just to compare equal.
export function timelineRowUiPruneSignature(inputs: TimelineRowUiPruneSignatureInputs): string {
  return [
    inputs.threadId,
    inputs.timelineRevision,
    inputs.revealTurnIndex,
    inputs.revealItemIndex,
    inputs.nodesLength,
    inputs.range.first,
    inputs.range.last,
    activeRowUiRetentionSignature(inputs.items),
  ].join('|');
}

export function activeRowUiRetentionSignature(items: readonly Item[]): string {
  const parts: string[] = [];
  for (const item of items) {
    if (!isActiveItem(item)) continue;
    parts.push([
      item.id,
      item.threadId,
      item.payloadId ?? '',
      item.status,
    ].join(':'));
  }
  return parts.join('|');
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
    rowGeometryKeys: new Set<string>(),
  };
  const retainStart = Math.max(0, range.first - options.nodeBuffer);
  const retainEnd = Math.min(nodes.length - 1, range.last + options.nodeBuffer);
  const tailStart = Math.max(0, nodes.length - options.tailNodeCount);

  for (let index = retainStart; index <= retainEnd; index += 1) {
    retainTopLevelNode(retained, nodes[index], options);
  }
  for (let index = tailStart; index < nodes.length; index += 1) {
    retainTopLevelNode(retained, nodes[index], options);
  }

  const activeItemIds = new Set<string>();
  for (const item of items) {
    if (!isActiveItem(item)) continue;
    activeItemIds.add(item.id);
    retainItem(retained, item);
  }
  if (activeItemIds.size > 0) {
    for (const node of nodes) {
      if (!retainActiveGroupKeys(retained, node, activeItemIds)) continue;
      retained.rowGeometryKeys.add(timelineNodeKey(node));
    }
  }

  return {
    itemIds: retained.itemIds,
    payloads: retained.payloads.values(),
    groupKeys: retained.groupKeys,
    rowGeometryKeys: retained.rowGeometryKeys,
  };
}

function isActiveItem(item: Item): boolean {
  return item.status === 'running' || item.status === 'streaming';
}

function retainItem(
  retained: RetentionAccumulator,
  item: Pick<Item, 'id' | 'threadId' | 'payloadId'>,
): void {
  retained.itemIds.add(item.id);
  if (!item.payloadId) return;
  const payloadKey = JSON.stringify([item.threadId, item.payloadId]);
  retained.payloads.set(payloadKey, {
    threadId: item.threadId,
    payloadId: item.payloadId,
  });
}

function retainTopLevelNode(
  retained: RetentionAccumulator,
  node: TimelineNode,
  options: TimelineRowUiRetentionOptions,
): void {
  retained.rowGeometryKeys.add(timelineNodeKey(node));
  retainNode(retained, node, options);
}

function retainNode(
  retained: RetentionAccumulator,
  node: TimelineNode,
  options: TimelineRowUiRetentionOptions,
): void {
  if (node.kind === 'leaf') {
    retainItem(retained, node.item);
    return;
  }

  if (node.kind === 'read_group') {
    for (const item of node.members) retainItem(retained, item);
    return;
  }

  retained.groupKeys.add(node.groupKey);
  retainItem(retained, node.parent);

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
