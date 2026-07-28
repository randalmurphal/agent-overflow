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
// The function is pure — fresh array out, no mutation of `nodes`.

import type { Item } from '../types/models';
import { readGroupKey, type TimelineLeaf, type TimelineNode } from './subagentGrouping';

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
    out.push({
      kind: 'read_group',
      groupKey: readGroupKey(members[0].id),
      threadId: members[0].threadId,
      members,
    });
    i = j;
  }
  return out;
}
