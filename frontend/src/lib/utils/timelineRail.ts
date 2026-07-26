// Rail participation: which timeline rows sit on the continuous
// left-border rail that runs alongside a block of agent activity.
//
// Extracted from `timelineRowProjection.svelte.ts` because two consumers
// need the identical rule and it must not drift between them: the row
// renderer draws the rail, and `activityRunGrouping.ts` uses rail
// participation as its run-membership predicate. Rail continuity and run
// continuity are the same property — that is what makes the rail coherent
// as a run's collapse control.

import type { Item } from '../types/models';
import type { TimelineNode } from './subagentGrouping';

// Leaf item kinds that participate in the rail. Subagent / wait group
// containers also participate so the rail stays continuous through nested
// cards and the agent card's chevron/icon/label gutter aligns with adjacent
// tool rows — see docs/specs/tool-call-ui-redesign/README.md.
export const RAIL_LEAF_KINDS: ReadonlySet<string> = new Set([
  'tool_call',
  'tool_completion',
  'thinking',
]);

export const RAIL_GROUP_KINDS: ReadonlySet<string> = new Set([
  'group',
  'wait_group',
  'read_group',
]);

// Tool rows whose body is a structured full-width card rather than the
// compact chev/icon/label/preview pattern — these break out of the rail so
// the vertical line doesn't run alongside the structured body (which would
// otherwise look like it belongs with the tool gutter even though the card
// spans the whole row). Proposed-plan rows are the only entry today; extend
// the set alongside any future card-style payload kind.
//
// Because this reads a payloadKind that can arrive AFTER the row first
// renders, a row can leave the rail mid-stream — which means it can also
// split an activity run in two. `activityRunGrouping` handles that.
export const RAIL_EXEMPT_PAYLOAD_KINDS: ReadonlySet<string> = new Set(['proposed_plan']);

/**
 * Does this node sit on the rail? `leafItem` is the CURRENT item for a leaf
 * node (re-resolved through the pane, not the projected snapshot) so a
 * late-arriving payloadKind is honored; pass null for structural nodes.
 */
export function timelineNodeHasRail(node: TimelineNode, leafItem: Item | null): boolean {
  if (leafItem) {
    return RAIL_LEAF_KINDS.has(leafItem.kind)
      && !RAIL_EXEMPT_PAYLOAD_KINDS.has(leafItem.payloadKind ?? '');
  }
  return RAIL_GROUP_KINDS.has(node.kind);
}
