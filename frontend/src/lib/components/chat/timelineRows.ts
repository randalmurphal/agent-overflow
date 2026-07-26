import {
  finalAssistantTextIdsByTurn,
  isToolTextBoundary,
  nodeRole,
  timelineNodeTurnIndex,
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
function nodeTurnIndexes(node: TimelineNode): number[] {
  if (node.kind !== 'activity_run') return [timelineNodeTurnIndex(node)];
  const turns: number[] = [];
  for (const child of node.children) {
    const turnIndex = timelineNodeTurnIndex(child);
    if (turns[turns.length - 1] !== turnIndex) turns.push(turnIndex);
  }
  return turns;
}

export function timelineRowDecorations(
  nodes: readonly TimelineNode[],
  activeTurnIndex: number | null,
): TimelineRowDecorationSets {
  const finalAssistantTextIds = finalAssistantTextIdsByTurn(nodes, activeTurnIndex);
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
      && lastRenderableRoleByTurn.get(node.item.turnIndex) === 'tool';
    if (responseDivider) {
      responseDividerIndexes.add(index);
      if (finalAssistantTextIds.has(node.item.id)) {
        responsePillIndexes.add(index);
      }
    }

    if (role === 'tool' || role === 'text') {
      for (const turnIndex of nodeTurnIndexes(node)) {
        lastRenderableRoleByTurn.set(turnIndex, role);
      }
    }
  }

  return {
    toolTextBoundaryIndexes,
    responseDividerIndexes,
    responsePillIndexes,
  };
}
