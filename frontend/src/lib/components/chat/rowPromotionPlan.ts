// What a row promotion will look like once the pane installs it, decided
// before it starts so the presentation can open exactly the box the
// installed row will occupy.
//
// A promotion replaces the last row of the timeline's last activity run
// with a prose row (stores/threadRowPromotions.svelte.ts). The plan is the
// timeline's own decoration pass run over the rows as they will be, so the
// gap, the divider and the pill cannot drift from what the row gets.

import type { Item } from '../../types/models';
import type { TimelineLeaf, TimelineNode } from '../../utils/subagentGrouping';
import { timelineRowDecorations, type TimelineRowDecorationSets } from './timelineRows';

export interface RowPromotionPlan {
  readonly itemId: string;
  /** The run row the promoted row leaves, and the stage renders in. */
  readonly runId: string;
  /** The run holds no other row, so it leaves with the promoted one. */
  readonly removesRun: boolean;
  /** The installed row sits at a tool/text boundary (the `mt-4` gap). */
  readonly boundary: boolean;
  /** The installed row carries a response divider. */
  readonly divider: boolean;
  /** That divider is the turn's final one, with the pill. */
  readonly pill: boolean;
  /** The run row carries the boundary gap today. */
  readonly runBoundary: boolean;
  /** The installed row as a node, for the pill's label and duration. */
  readonly node: TimelineLeaf;
}

export interface RowPromotionPlanInput {
  itemId: string;
  /** The row the pane will install. */
  row: Item;
  /** The rows on screen (`TimelineRowProjection.revealedNodes`). */
  revealed: readonly TimelineNode[];
  /** Every row the pane holds (`TimelineRowProjection.groupedNodes`). */
  structure: readonly TimelineNode[];
  /** Decorations of the rows on screen today. */
  decorations: TimelineRowDecorationSets;
  activeTurnKey: number | null;
  turnKeyOf: (item: Item) => number;
}

/**
 * The plan for promoting `itemId`, or null when the timeline cannot present
 * it: the row is not the mounted last row of an open run at the end of the
 * revealed rows, or the run's shape after the change is not known here.
 */
export function planRowPromotion(input: RowPromotionPlanInput): RowPromotionPlan | null {
  const { itemId, row, revealed } = input;
  const runIndex = revealed.length - 1;
  const run = revealed[runIndex];
  if (run?.kind !== 'activity_run' || run.collapsed || run.unshippedAfter > 0) return null;
  const last = run.children[run.children.length - 1];
  if (last.kind !== 'leaf' || last.item.id !== itemId) return null;
  if (run.mountedFrom + run.mountedRows < run.children.length) return null;
  const removesRun = run.children.length === 1;
  // A run whose other members the pane does not hold keeps a row of its own.
  if (removesRun && run.unshippedBefore > 0) return null;

  const node: TimelineLeaf = { kind: 'leaf', item: row };
  const after = revealed.slice(0, runIndex);
  if (!removesRun) after.push({ ...run, children: run.children.slice(0, -1) });
  after.push(node);
  const structureAfter = input.structure.map(
    (entry) => entry.kind === 'leaf' && entry.item.id === itemId ? node : entry,
  );
  const sets = timelineRowDecorations(after, input.activeTurnKey, input.turnKeyOf, structureAfter);
  const index = after.length - 1;
  return {
    itemId,
    runId: run.runId,
    removesRun,
    boundary: sets.toolTextBoundaryIndexes.has(index),
    divider: sets.responseDividerIndexes.has(index),
    pill: sets.responsePillIndexes.has(index),
    runBoundary: input.decorations.toolTextBoundaryIndexes.has(runIndex),
    node,
  };
}
