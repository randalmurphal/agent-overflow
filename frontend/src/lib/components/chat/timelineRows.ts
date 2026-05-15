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
    const turnIndex = timelineNodeTurnIndex(node);

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
      lastRenderableRoleByTurn.set(turnIndex, role);
    }
  }

  return {
    toolTextBoundaryIndexes,
    responseDividerIndexes,
    responsePillIndexes,
  };
}
