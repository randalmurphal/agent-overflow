import type { Item } from '../../types/models';
import {
  finalAssistantTextIdsByTurn,
  isToolTextBoundary,
  itemTurnIndexKey,
  nodeRole,
  timelineNodeRepresentativeItem,
  type TimelineNode,
} from '../../utils/subagentGrouping';

export interface TimelineRowDecorationSets {
  toolTextBoundaryIndexes: ReadonlySet<number>;
  responseDividerIndexes: ReadonlySet<number>;
  responsePillIndexes: ReadonlySet<number>;
}

/**
 * Every turn a node covers. All but one node covers exactly one; an
 * activity run can span several, because providers emit per wire round and
 * a round that produces only tool calls is not separated from the next by
 * any prose row (see internal/triage/CLAUDE.md § Wire-round vs
 * logical-turn). Recording only the run's root turn would leave the later
 * turns with no `tool` role on record, and the assistant message closing
 * one of them would lose its response divider.
 */
function nodeTurnKeys(node: TimelineNode, turnKeyOf: (item: Item) => number): number[] {
  if (node.kind !== 'activity_run') return [turnKeyOf(timelineNodeRepresentativeItem(node))];
  const turns: number[] = [];
  for (const child of node.children) {
    const turnKey = turnKeyOf(timelineNodeRepresentativeItem(child));
    if (turns[turns.length - 1] !== turnKey) turns.push(turnKey);
  }
  return turns;
}

/**
 * Turn identity is an INPUT, not `item.turnIndex` read in place: the
 * surface decides what a "turn" is (`ThreadPane.timelineTurns`). The chat
 * timeline keys on the provider turn; the agent pane keys its whole
 * scoped window as one run, so the main thread settling never stamps a
 * "Response" pill on a subagent that is still working.
 */
export function timelineRowDecorations(
  nodes: readonly TimelineNode[],
  activeTurnKey: number | null,
  turnKeyOf: (item: Item) => number = itemTurnIndexKey,
): TimelineRowDecorationSets {
  const finalAssistantTextIds = finalAssistantTextIdsByTurn(nodes, activeTurnKey, turnKeyOf);
  const toolTextBoundaryIndexes = new Set<number>();
  const responseDividerIndexes = new Set<number>();
  const responsePillIndexes = new Set<number>();
  const lastRenderableRoleByTurn = new Map<number, 'tool' | 'text'>();

  for (let index = 0; index < nodes.length; index += 1) {
    const node = nodes[index];
    const role = nodeRole(node);

    if (isToolTextBoundary(nodes, index)) {
      toolTextBoundaryIndexes.add(index);
    }

    const responseDivider = node.kind === 'leaf'
      && node.item.kind === 'assistant_text'
      && lastRenderableRoleByTurn.get(turnKeyOf(node.item)) === 'tool';
    if (responseDivider) {
      responseDividerIndexes.add(index);
      if (finalAssistantTextIds.has(node.item.id)) {
        responsePillIndexes.add(index);
      }
    }

    if (role === 'tool' || role === 'text') {
      for (const turnKey of nodeTurnKeys(node, turnKeyOf)) {
        lastRenderableRoleByTurn.set(turnKey, role);
      }
    }
  }

  return {
    toolTextBoundaryIndexes,
    responseDividerIndexes,
    responsePillIndexes,
  };
}
