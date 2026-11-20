// Which rows an in-flight edit-and-resend revert is about to destroy, for
// the dim the timeline paints on them (`pendingCutAfter`, editResendFlow).
//
// The cut is positional: Claude's is item-granular after the anchor, and
// Codex's drops the anchor's whole turn (itemIndex -1). The anchor is the
// person's message, which ends any activity run, so on Claude no run
// straddles it. On Codex one can: the agent message that opened the
// anchor's turn is a run member (under either rule when it follows the
// previous turn's work), so the work of the previous turn and the
// anchor turn's rows before a steered anchor share one run. That run's row
// is dimmed by the row wrapper only when its first member is doomed; its
// children are dimmed one by one otherwise (`ActivityRun`), through this
// context, which MessageTimeline provides.

import { timelineNodeItemIndex, timelineNodeTurnIndex, type TimelineNode } from '../../utils/subagentGrouping';
import type { PendingCutPosition } from './editResendFlow.svelte';

/** Whether `node` sits strictly after the cut, compared by its first item. */
export function isAfterPendingCut(node: TimelineNode, cut: PendingCutPosition | null): boolean {
  if (!cut) return false;
  const turnIndex = timelineNodeTurnIndex(node);
  if (turnIndex !== cut.turnIndex) return turnIndex > cut.turnIndex;
  return timelineNodeItemIndex(node) > cut.itemIndex;
}

/** What a run reads to dim the members a cut destroys inside it. */
export interface PendingCutRows {
  isCut(node: TimelineNode): boolean;
}

export const PENDING_CUT_ROWS_CONTEXT = Symbol('pending-cut-rows');

/** The dim a doomed member wears inside a run: the row wrapper's `chat-row-pending-cut` opacity. */
export const PENDING_CUT_DIM_CLASS = 'opacity-[0.42]';
