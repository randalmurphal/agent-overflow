// What an agent card (SubagentGroup.svelte) says about the stop it sits
// at: its failure line, a failed output read, and the time it covers.
import type { Item } from '../../types/models';
import type { CompletionStatus } from '../../utils/toolCompletionStatus';
import type { ParkedStop } from '../../utils/parkedStop';
import {
  completionEndedBySessionDeath,
  rowErrorForStatus,
  SESSION_DIED_ROW_ERROR,
  type RowErrorData,
} from './rowState';

/** The failure line of a card whose stop failed; null otherwise. */
export function subagentCardRowError(
  statusItem: Pick<Item, 'status'>,
  completionStatus: CompletionStatus,
  statusMeta: Record<string, unknown> | null,
): RowErrorData | null {
  if (completionStatus !== 'failure') return null;
  // A sibling the session's death wrote: the agent was neither stopped by
  // the user nor failed, so "stopped" would misreport what happened.
  if (statusItem.status === 'killed' && completionEndedBySessionDeath(statusMeta)) {
    return SESSION_DIED_ROW_ERROR;
  }
  return rowErrorForStatus(statusItem.status, 'Agent failed', 'Agent stopped') ?? {
    tone: 'error',
    msg: 'Agent failed',
  };
}

/**
 * A failed output-file read triage recorded on the stop
 * (notification_output_state/error, output_file_state/error on older
 * rows), or ''. A silently incomplete card body reads exactly like a
 * complete one, so the failure renders inline.
 */
export function subagentCardOutputError(statusMeta: Record<string, unknown> | null): string {
  const state = statusMeta?.notification_output_state ?? statusMeta?.output_file_state;
  if (state !== 'error') return '';
  const error = statusMeta?.notification_output_error ?? statusMeta?.output_file_error;
  return typeof error === 'string' && error ? error : 'Task output could not be read.';
}

/**
 * The span a card's duration covers, in epoch ms. A parked card covers its
 * own run, from the launch or the wake that started it to the stop. Any
 * other card starts at the launch (a Codex execution at its own start) and
 * ends at what carries the terminal: for a background agent the stop's
 * sibling, whose updatedAt is when the task reported back. A running card
 * ends now.
 */
export function subagentCardSpan(card: {
  parent: Pick<Item, 'createdAt'>;
  statusItem: Pick<Item, 'createdAt' | 'updatedAt'>;
  completionMeta: Record<string, unknown> | null;
  parkedStop: ParkedStop | null;
  running: boolean;
  now: number;
}): { start: number; end: number } {
  if (card.parkedStop) {
    return { start: card.parkedStop.runStartedAt || card.parent.createdAt, end: card.statusItem.createdAt };
  }
  const start = Number(card.completionMeta?.codex_execution_started_at ?? card.parent.createdAt);
  return { start, end: card.running ? card.now : card.statusItem.updatedAt };
}
