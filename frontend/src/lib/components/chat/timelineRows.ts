import type { Item } from '../../types/models';
import {
  isToolTextBoundary,
  itemTurnIndexKey,
  type TimelineNode,
} from '../../utils/subagentGrouping';
import { isCommandAgentResult } from '../../utils/commandAgentResult';

export interface TimelineRowDecorationSets {
  toolTextBoundaryIndexes: ReadonlySet<number>;
  responseDividerIndexes: ReadonlySet<number>;
  responsePillIndexes: ReadonlySet<number>;
}

/**
 * A response divider goes on assistant prose directly below an activity run,
 * whatever turn the run belongs to: a turn the provider starts (a
 * background-task wake) has no user row, and the completion that woke it
 * stays in the previous turn. Only the pill reads turns.
 *
 * Turn identity is an INPUT, not `item.turnIndex` read in place: the
 * surface decides what a "turn" is (`ThreadPane.timelineTurns`). The chat
 * timeline keys on the provider turn; the agent pane keys its whole
 * scoped window as one run, so the main thread settling never stamps a
 * "Response" pill on a subagent that is still working.
 *
 * `nodes` are the rows on screen; every returned set indexes into them.
 * `structure` is every row the pane holds, revealed or not, and decides
 * which row is a turn's final response. The two differ while the reveal
 * gate withholds a turn's tail: the turn is wire-complete, its final text
 * sits behind the frontier, and the last assistant text ON SCREEN is an
 * intermediate one. Deciding on the screen rows put the pill there and
 * moved it when the tail revealed. Decided on the structure, the pill
 * waits for its row.
 */
export function timelineRowDecorations(
  nodes: readonly TimelineNode[],
  activeTurnKey: number | null,
  turnKeyOf: (item: Item) => number = itemTurnIndexKey,
  structure: readonly TimelineNode[] = nodes,
): TimelineRowDecorationSets {
  const finalResponseIds = finalResponseIdsByTurn(structure, activeTurnKey, turnKeyOf);
  const toolTextBoundaryIndexes = new Set<number>();
  const responseDividerIndexes = new Set<number>();
  const responsePillIndexes = new Set<number>();

  for (let index = 0; index < nodes.length; index += 1) {
    const node = nodes[index];

    if (isToolTextBoundary(nodes, index)) {
      toolTextBoundaryIndexes.add(index);
    }

    const responseDivider = node.kind === 'leaf'
      && (node.item.kind === 'assistant_text' || isCommandAgentResult(node.item))
      && nodes[index - 1]?.kind === 'activity_run';
    if (responseDivider) {
      responseDividerIndexes.add(index);
      if (finalResponseIds.has(node.item.id)) {
        responsePillIndexes.add(index);
      }
    }
  }

  return {
    toolTextBoundaryIndexes,
    responseDividerIndexes,
    responsePillIndexes,
  };
}

function finalResponseIdsByTurn(
  nodes: readonly TimelineNode[],
  activeTurnKey: number | null,
  turnKeyOf: (item: Item) => number,
): Set<string> {
  const lastByTurn = new Map<number, string>();
  for (const node of nodes) {
    if (node.kind !== 'leaf') continue;
    if (node.item.kind !== 'assistant_text' && !isCommandAgentResult(node.item)) continue;
    lastByTurn.set(turnKeyOf(node.item), node.item.id);
  }
  if (activeTurnKey !== null) lastByTurn.delete(activeTurnKey);
  return new Set(lastByTurn.values());
}

export function responsePillLabel(node: TimelineNode): 'Result' | 'Response' {
  return node.kind === 'leaf' && isCommandAgentResult(node.item) ? 'Result' : 'Response';
}
