// Post-pass over `groupItemsBySubagent`'s output that folds runs of
// adjacent Read tool_call leaves into a single `read_group` node.
//
// The pass runs only at the top level of the timeline — Reads nested
// inside a SubagentGroupNode / WaitGroupNode
// stay inside their parent group untouched, both because those
// surfaces already have their own packing and because the rail
// continuity for subagent transcripts is owned by the group
// container. Status (running, errored, declined, completed) does not
// split groups; the renderer derives visible status/error state from
// the current members.
//
// Single Read rows also project through `read_group`. That keeps the
// virtualized row key and component shell stable when a second adjacent
// Read arrives: the row appends another file link instead of swapping
// from GenericToolCallRow to ReadGroupRow.
//
// The function is pure and never mutates `nodes`; it returns the SAME
// array reference when there is no Read leaf to fold (matching
// `sliceRevealedNodes` / `groupActivityRuns`), a fresh array otherwise.
//
// Group nodes are cached per first-member Item: a run of Reads whose
// member Items are all reference-identical to the previous pass reuses
// the previous node wholesale. Sound because `writeItemAt` replaces a
// row's Item on every content write (an Item reference is a content
// version) and a read_group's shape derives from nothing but its
// members. Reference-stable groups keep the activity-run build cache
// (`activityRunGrouping.ts`) hitting on runs that contain Reads.

import type { Item } from '../types/models';
import { readGroupKey, type ReadGroupNode, type TimelineLeaf, type TimelineNode } from './subagentGrouping';

const readGroupByFirstMember = new WeakMap<Item, ReadGroupNode>();

function cachedReadGroup(members: Item[]): ReadGroupNode {
  const cached = readGroupByFirstMember.get(members[0]);
  if (cached !== undefined && cached.members.length === members.length) {
    let unchanged = true;
    for (let k = 0; k < members.length; k += 1) {
      if (cached.members[k] !== members[k]) {
        unchanged = false;
        break;
      }
    }
    if (unchanged) return cached;
  }
  const node: ReadGroupNode = {
    kind: 'read_group',
    groupKey: readGroupKey(members[0].id),
    threadId: members[0].threadId,
    members,
  };
  readGroupByFirstMember.set(members[0], node);
  return node;
}

function isReadLeaf(node: TimelineNode): node is TimelineLeaf {
  if (node.kind !== 'leaf') return false;
  const item = node.item;
  if (item.kind !== 'tool_call') return false;
  if (item.toolName !== 'Read') return false;
  // Background launches don't normally fire for Read, but defending
  // here keeps a future provider quirk from sweeping a tray row into
  // the compact bundle.
  if (item.isBackground === true) return false;
  return true;
}

export function groupConsecutiveReads(nodes: TimelineNode[]): TimelineNode[] {
  if (nodes.length === 0) return nodes;
  // Same array reference when there is nothing to fold — a window with no
  // Read leaves (most agent burns) must not pay a full copy per pass.
  let hasReadLeaf = false;
  for (let i = 0; i < nodes.length; i += 1) {
    if (isReadLeaf(nodes[i])) {
      hasReadLeaf = true;
      break;
    }
  }
  if (!hasReadLeaf) return nodes;
  const out: TimelineNode[] = [];
  let i = 0;
  while (i < nodes.length) {
    const head = nodes[i];
    if (!isReadLeaf(head)) {
      out.push(head);
      i += 1;
      continue;
    }
    let j = i + 1;
    while (j < nodes.length && isReadLeaf(nodes[j])) j += 1;
    const runLength = j - i;
    const members: Item[] = new Array(runLength);
    for (let k = 0; k < runLength; k += 1) {
      members[k] = (nodes[i + k] as TimelineLeaf).item;
    }
    out.push(cachedReadGroup(members));
    i = j;
  }
  return out;
}
