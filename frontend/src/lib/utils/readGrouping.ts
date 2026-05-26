// Post-pass over `groupItemsBySubagent`'s output that folds runs of
// adjacent Read tool_call leaves into a single `read_group` node.
//
// The pass runs only at the top level of the timeline — Reads nested
// inside a SubagentGroupNode / WaitGroupNode
// stay inside their parent group untouched, both because those
// surfaces already have their own packing and because the rail
// continuity for subagent transcripts is owned by the group
// container. Status (running, errored, declined, completed) is
// intentionally ignored when grouping — the row shows just file
// names, so failure-state distinctions don't matter at this granularity.
//
// `MIN_GROUP_SIZE = 2` keeps single Read rows rendering as normal
// GenericToolCallRow leaves with their full disclosure / EditorLink
// chrome; only an actual run of consecutive reads collapses.
//
// The function is pure — fresh array out, no mutation of `nodes`.

import type { Item } from '../types/models';
import type { TimelineLeaf, TimelineNode } from './subagentGrouping';

const MIN_GROUP_SIZE = 2;

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
    if (runLength < MIN_GROUP_SIZE) {
      out.push(head);
      i += 1;
      continue;
    }
    const members: Item[] = new Array(runLength);
    for (let k = 0; k < runLength; k += 1) {
      members[k] = (nodes[i + k] as TimelineLeaf).item;
    }
    out.push({
      kind: 'read_group',
      groupKey: `reads:${members[0].id}`,
      threadId: members[0].threadId,
      members,
    });
    i = j;
  }
  return out;
}
