import type { Item } from '../../types/models';
import type { TimelineRowGeometryKey } from '../../stores/threadRowUiState.svelte';
import {
  timelineNodeKey,
  type TimelineNode,
} from '../../utils/subagentGrouping';

// Pure builders for the row-geometry cache key: (row key, geometry
// signature, width, owner item ids). The signature captures everything
// that can change a row's rendered height — item identity/status/indices,
// summary and payload-meta lengths, group expansion, member composition,
// and the shell signature threaded in by MessageTimeline — so a cache hit
// means "same content shape at this width" and the cached height is safe
// to reserve. Consumed by MessageTimeline per rendered row and by the
// reservation state machine in timelineRowGeometry.ts.

export function timelineRowGeometryKey(
  node: TimelineNode,
  currentLeafItem: Item | null,
  width: number,
  isGroupExpanded: (groupKey: string) => boolean,
  rowShellSignature: string,
): TimelineRowGeometryKey {
  return {
    key: timelineNodeKey(node),
    signature: timelineNodeGeometrySignature(
      node,
      currentLeafItem,
      isGroupExpanded,
      rowShellSignature,
    ),
    width,
    ownerItemIds: timelineNodeOwnerItemIds(node, currentLeafItem),
  };
}

function timelineNodeGeometrySignature(
  node: TimelineNode,
  currentLeafItem: Item | null,
  isGroupExpanded: (groupKey: string) => boolean,
  rowShellSignature: string,
): string {
  const prefix = `shell:${rowShellSignature}`;
  if (node.kind === 'leaf') {
    return `${prefix}|leaf:${itemGeometrySignature(currentLeafItem ?? node.item)}`;
  }

  if (node.kind === 'read_group') {
    return [
      prefix,
      'read',
      node.groupKey,
      ...node.members.map(itemGeometrySignature),
    ].join('|');
  }

  if (node.kind === 'wait_group') {
    return [
      prefix,
      'wait',
      node.groupKey,
      itemGeometrySignature(node.parent),
      node.completion ? itemGeometrySignature(node.completion) : '',
      node.descendantCount,
      node.children.length,
      ...node.children.slice(0, 25).map((child) =>
        timelineNodeGeometrySignature(child, null, isGroupExpanded, 'nested-wait-child')),
    ].join('|');
  }

  const expanded = isGroupExpanded(node.groupKey);
  return [
    prefix,
    node.kind,
    node.groupKey,
    expanded ? 'expanded' : 'collapsed',
    itemGeometrySignature(node.parent),
    node.descendantCount,
    node.loadedDescendantCount,
    node.latestChildSummary.length,
  ].join('|');
}

function itemGeometrySignature(item: Item): string {
  const payloadMeta = item.payloadMeta ?? '';
  return [
    item.threadId,
    item.id,
    item.kind,
    item.status,
    item.turnIndex,
    item.itemIndex,
    item.updatedAt,
    item.summary.length,
    item.payloadId ?? '',
    item.payloadKind ?? '',
    payloadMeta.length,
    item.completionOf ?? '',
    item.parentId ?? '',
    item.isBackground === true ? 'bg' : '',
  ].join(':');
}

function timelineNodeOwnerItemIds(node: TimelineNode, currentLeafItem: Item | null): string[] {
  if (node.kind === 'leaf') return [(currentLeafItem ?? node.item).id];
  if (node.kind === 'read_group') return node.members.map((item) => item.id);
  if (node.kind === 'wait_group') {
    return [
      node.parent.id,
      ...(node.completion ? [node.completion.id] : []),
    ];
  }
  return [node.parent.id];
}
