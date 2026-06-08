import type { Item } from '../../types/models';
import type { TimelineNode } from '../../utils/subagentGrouping';
import type {
  PayloadExpansionRetentionKey,
  RowUiStateRetention,
} from '../../stores/threadRowUiState.svelte';

export interface TimelineRowUiRetentionRange {
  first: number;
  last: number;
}

export interface TimelineRowUiRetentionOptions {
  nodeBuffer: number;
  tailNodeCount: number;
  isGroupExpanded(groupKey: string): boolean;
}

export interface TimelineRowUiRetentionResult {
  retention: RowUiStateRetention;
  signature: string;
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
): TimelineRowUiRetentionResult {
  const retainedItemIds = new Set<string>();
  const retainedPayloads = new Map<string, PayloadExpansionRetentionKey>();
  const retainedGroupKeys = new Set<string>();
  const retainStart = Math.max(0, range.first - options.nodeBuffer);
  const retainEnd = Math.min(nodes.length - 1, range.last + options.nodeBuffer);
  const tailStart = Math.max(0, nodes.length - options.tailNodeCount);

  for (let index = retainStart; index <= retainEnd; index += 1) {
    retainNode(retainedItemIds, retainedPayloads, retainedGroupKeys, nodes[index], options);
  }
  for (let index = tailStart; index < nodes.length; index += 1) {
    retainNode(retainedItemIds, retainedPayloads, retainedGroupKeys, nodes[index], options);
  }

  const activeItemIds = new Set<string>();
  for (const item of items) {
    if (!isActiveItem(item)) continue;
    activeItemIds.add(item.id);
    retainItem(retainedItemIds, retainedPayloads, item);
  }
  if (activeItemIds.size > 0) {
    for (const node of nodes) retainActiveGroupKeys(retainedGroupKeys, node, activeItemIds);
  }

  return {
    retention: {
      itemIds: retainedItemIds,
      payloads: retainedPayloads.values(),
      groupKeys: retainedGroupKeys,
    },
    signature: [
      nodes.length,
      range.first,
      range.last,
      retainStart,
      retainEnd,
      tailStart,
      activeRowUiRetentionSignature(items),
    ].join('|'),
  };
}

function isActiveItem(item: Item): boolean {
  return item.status === 'running' || item.status === 'streaming';
}

function retainItem(
  itemIds: Set<string>,
  payloads: Map<string, PayloadExpansionRetentionKey>,
  item: Pick<Item, 'id' | 'threadId' | 'payloadId'>,
): void {
  itemIds.add(item.id);
  if (!item.payloadId) return;
  const payloadKey = JSON.stringify([item.threadId, item.payloadId]);
  payloads.set(payloadKey, {
    threadId: item.threadId,
    payloadId: item.payloadId,
  });
}

function retainNode(
  itemIds: Set<string>,
  payloads: Map<string, PayloadExpansionRetentionKey>,
  groupKeys: Set<string>,
  node: TimelineNode,
  options: TimelineRowUiRetentionOptions,
): void {
  if (node.kind === 'leaf') {
    retainItem(itemIds, payloads, node.item);
    return;
  }

  if (node.kind === 'read_group') {
    for (const item of node.members) retainItem(itemIds, payloads, item);
    return;
  }

  groupKeys.add(node.groupKey);
  retainItem(itemIds, payloads, node.parent);

  if (node.kind === 'group' && !options.isGroupExpanded(node.groupKey)) return;
  for (const child of node.children) {
    retainNode(itemIds, payloads, groupKeys, child, options);
  }
}

function retainActiveGroupKeys(
  groupKeys: Set<string>,
  node: TimelineNode,
  activeItemIds: ReadonlySet<string>,
): boolean {
  if (node.kind === 'leaf') return activeItemIds.has(node.item.id);
  if (node.kind === 'read_group') {
    return node.members.some((item) => activeItemIds.has(item.id));
  }

  let containsActiveItem = activeItemIds.has(node.parent.id);
  for (const child of node.children) {
    if (retainActiveGroupKeys(groupKeys, child, activeItemIds)) containsActiveItem = true;
  }
  if (containsActiveItem) groupKeys.add(node.groupKey);
  return containsActiveItem;
}
