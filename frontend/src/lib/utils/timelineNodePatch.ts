import type { Item } from '../types/models';
import type {
  TimelineNode,
  TimelineLeaf,
  SubagentGroupNode,
  WaitGroupNode,
  ReadGroupNode,
} from './subagentGrouping';
import { pickLatestChildSummary } from './subagentGrouping';

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
): { next: TimelineNode[]; changed: boolean } {
  let changed = false;
  const next: TimelineNode[] = new Array(children.length);
  for (let i = 0; i < children.length; i++) {
    const patched = patchNode(children[i], getItem);
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
): SubagentGroupNode {
  const freshParent = getItem(node.parent.id);
  const parentChanged = freshParent !== undefined && freshParent !== node.parent;
  const { next: nextChildren, changed: childrenChanged } = patchChildren(node.children, getItem);
  if (!parentChanged && !childrenChanged) return node;
  return {
    kind: 'group',
    parent: parentChanged ? freshParent : node.parent,
    groupKey: node.groupKey,
    children: nextChildren,
    descendantCount: node.descendantCount,
    latestChildSummary: childrenChanged
      ? pickLatestChildSummary(nextChildren)
      : node.latestChildSummary,
  };
}

function patchWaitGroup(
  node: WaitGroupNode,
  getItem: (id: string) => Item | undefined,
): WaitGroupNode {
  const freshParent = getItem(node.parent.id);
  const parentChanged = freshParent !== undefined && freshParent !== node.parent;
  const { next: nextChildren, changed: childrenChanged } = patchLeafChildren(node.children, getItem);
  if (!parentChanged && !childrenChanged) return node;
  return {
    kind: 'wait_group',
    parent: parentChanged ? freshParent : node.parent,
    groupKey: node.groupKey,
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
): TimelineNode {
  switch (node.kind) {
    case 'leaf': return patchLeaf(node, getItem);
    case 'group': return patchSubagentGroup(node, getItem);
    case 'wait_group': return patchWaitGroup(node, getItem);
    case 'read_group': return patchReadGroup(node, getItem);
  }
}

export function patchTimelineNodeItemRefs(
  skeleton: readonly TimelineNode[],
  getItem: (id: string) => Item | undefined,
): TimelineNode[] {
  let changed = false;
  const result: TimelineNode[] = new Array(skeleton.length);
  for (let i = 0; i < skeleton.length; i++) {
    const patched = patchNode(skeleton[i], getItem);
    if (patched !== skeleton[i]) changed = true;
    result[i] = patched;
  }
  return changed ? result : (skeleton as TimelineNode[]);
}
