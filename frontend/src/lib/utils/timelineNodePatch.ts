import type { Item } from '../types/models';
import type {
  TimelineNode,
  TimelineLeaf,
  SubagentGroupNode,
  SubagentLiveAggregates,
  WaitGroupNode,
  ReadGroupNode,
} from './subagentGrouping';
import {
  decoratedSubagentAggregates,
  pickLatestChildSummary,
} from './subagentGrouping';

function patchLeaf(
  node: TimelineLeaf,
  getItem: (id: string) => Item | undefined,
): TimelineLeaf {
  const fresh = getItem(node.item.id);
  if (!fresh || fresh === node.item) return node;
  return { kind: 'leaf', item: fresh, orphan: node.orphan };
}

function patchChildren(
  children: TimelineNode[],
  getItem: (id: string) => Item | undefined,
  aggregates: SubagentLiveAggregates | undefined,
): { next: TimelineNode[]; changed: boolean } {
  let changed = false;
  const next: TimelineNode[] = new Array(children.length);
  for (let i = 0; i < children.length; i++) {
    const patched = patchNode(children[i], getItem, aggregates);
    if (patched !== children[i]) changed = true;
    next[i] = patched;
  }
  return { next: changed ? next : children, changed };
}

function patchLeafChildren(
  children: TimelineLeaf[],
  getItem: (id: string) => Item | undefined,
): { next: TimelineLeaf[]; changed: boolean } {
  let changed = false;
  const next: TimelineLeaf[] = new Array(children.length);
  for (let i = 0; i < children.length; i++) {
    const patched = patchLeaf(children[i], getItem);
    if (patched !== children[i]) changed = true;
    next[i] = patched;
  }
  return { next: changed ? next : children, changed };
}

function patchSubagentGroup(
  node: SubagentGroupNode,
  getItem: (id: string) => Item | undefined,
  aggregates: SubagentLiveAggregates | undefined,
): SubagentGroupNode {
  const freshParent = getItem(node.parent.id);
  const parentChanged = freshParent !== undefined && freshParent !== node.parent;
  const { next: nextChildren, changed: childrenChanged } = patchChildren(
    node.children,
    getItem,
    aggregates,
  );
  if (!parentChanged && !childrenChanged) return node;
  const parent = parentChanged ? freshParent : node.parent;
  // Only a replaced parent ref can carry different decorated aggregates,
  // so the meta re-parse is skipped otherwise. A live upsert can also
  // overwrite the decorated meta entirely (count and summary vanish),
  // which is why display values ratchet: the count takes max() against
  // the node's previous value, and the preview falls back to the node's
  // previous text rather than going blank. Both self-heal to fresh
  // values on the next structural rebuild.
  let descendantCount = node.descendantCount;
  let decoratedSummary = '';
  if (parentChanged) {
    const decorated = decoratedSubagentAggregates(parent);
    descendantCount = Math.max(node.descendantCount, decorated.count);
    decoratedSummary = decorated.summary;
  }
  return {
    kind: 'group',
    parent,
    groupKey: node.groupKey,
    children: nextChildren,
    descendantCount,
    loadedDescendantCount: node.loadedDescendantCount,
    latestChildSummary: childrenChanged
      ? (pickLatestChildSummary(nextChildren, aggregates?.(parent.id))
        || decoratedSummary
        || node.latestChildSummary)
      : (node.latestChildSummary || decoratedSummary),
  };
}

function patchWaitGroup(
  node: WaitGroupNode,
  getItem: (id: string) => Item | undefined,
): WaitGroupNode {
  const freshParent = getItem(node.parent.id);
  const parentChanged = freshParent !== undefined && freshParent !== node.parent;
  // Re-resolve the folded completion (the header item) so its status / per-agent
  // statuses refresh on streaming deltas. Only look it up when present — and note
  // the rebuilt node MUST carry `completion` through, or it would be dropped on
  // every patch.
  // freshCompletion is only resolved when node.completion exists, so a defined
  // freshCompletion already implies a present completion to compare against.
  const freshCompletion = node.completion ? getItem(node.completion.id) : undefined;
  const completionChanged = freshCompletion !== undefined && freshCompletion !== node.completion;
  const { next: nextChildren, changed: childrenChanged } = patchLeafChildren(node.children, getItem);
  if (!parentChanged && !completionChanged && !childrenChanged) return node;
  return {
    kind: 'wait_group',
    parent: parentChanged ? freshParent : node.parent,
    groupKey: node.groupKey,
    completion: completionChanged ? freshCompletion : node.completion,
    children: nextChildren,
    descendantCount: node.descendantCount,
  };
}

function patchReadGroup(
  node: ReadGroupNode,
  getItem: (id: string) => Item | undefined,
): ReadGroupNode {
  let changed = false;
  const nextMembers: Item[] = new Array(node.members.length);
  for (let i = 0; i < node.members.length; i++) {
    const fresh = getItem(node.members[i].id);
    if (fresh && fresh !== node.members[i]) {
      changed = true;
      nextMembers[i] = fresh;
    } else {
      nextMembers[i] = node.members[i];
    }
  }
  if (!changed) return node;
  return {
    kind: 'read_group',
    groupKey: node.groupKey,
    threadId: node.threadId,
    members: nextMembers,
  };
}

function patchNode(
  node: TimelineNode,
  getItem: (id: string) => Item | undefined,
  aggregates: SubagentLiveAggregates | undefined,
): TimelineNode {
  switch (node.kind) {
    case 'leaf': return patchLeaf(node, getItem);
    case 'group': return patchSubagentGroup(node, getItem, aggregates);
    case 'wait_group': return patchWaitGroup(node, getItem);
    case 'read_group': return patchReadGroup(node, getItem);
  }
}

export function patchTimelineNodeItemRefs(
  skeleton: readonly TimelineNode[],
  getItem: (id: string) => Item | undefined,
  aggregates?: SubagentLiveAggregates,
): TimelineNode[] {
  let changed = false;
  const result: TimelineNode[] = new Array(skeleton.length);
  for (let i = 0; i < skeleton.length; i++) {
    const patched = patchNode(skeleton[i], getItem, aggregates);
    if (patched !== skeleton[i]) changed = true;
    result[i] = patched;
  }
  return changed ? result : (skeleton as TimelineNode[]);
}
