import type { Item } from '../../types/models';
import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';

// `parked` is never derived from a row: a parked background agent's launch
// row stays `running` (claude-wire.md §E6b), and only the served run state
// (stores/subagentRunState.svelte.ts) tells a host to show it.
export type IndicatorState = 'running' | 'backgrounded' | 'parked' | 'error' | 'declined' | null;

type ItemStatus = Item['status'];

interface IndicatorOptions {
  meta?: Record<string, unknown> | null;
}

export interface RowErrorData {
  code?: string;
  msg: string;
  tone: 'error' | 'declined';
}

export function indicatorStateForItem(
  item: Pick<Item, 'kind' | 'status' | 'isBackground' | 'payloadMeta'>,
  options: IndicatorOptions = {},
): IndicatorState {
  if (
    item.kind === 'tool_call' &&
    item.isBackground === true &&
    (item.status === 'running' || item.status === 'streaming')
  ) {
    return 'backgrounded';
  }
  if (item.status === 'running' || item.status === 'streaming') return 'running';
  if (item.status === 'declined') return 'declined';
  if (item.status === 'errored' || item.status === 'killed') return 'error';
  return deriveCompletionStatus(item, { meta: options.meta }) === 'failure' ? 'error' : null;
}

/**
 * A background agent's completion sibling that the session's death wrote
 * (triage stamps `status_source: "session_died"`, tool_lifecycle.go): the
 * agent was neither stopped by the user nor failed; the session ended under
 * it. Read off the sibling's `meta`, never its payload meta.
 */
export function completionEndedBySessionDeath(
  meta: Record<string, unknown> | null | undefined,
): boolean {
  return meta?.status_source === 'session_died';
}

export const SESSION_DIED_ROW_ERROR: RowErrorData = {
  tone: 'error',
  msg: 'Session ended before the agent finished',
};

/**
 * `stopped` is the copy for a `killed` row: a provider stop or kill, distinct
 * from a failure. Agent surfaces pass "Agent stopped"; the default names a
 * tool call.
 */
export function rowErrorForStatus(
  status: ItemStatus,
  fallback: string,
  stopped = 'Tool call stopped',
): RowErrorData | null {
  if (status === 'declined') return { tone: 'declined', msg: 'Tool call declined' };
  if (status === 'killed') return { tone: 'error', msg: stopped };
  if (status === 'errored') return { tone: 'error', msg: fallback };
  return null;
}

/**
 * Tool-row failure projection used by AgentRow/AdvisorRow. The
 * `??` fallback collapses two branches that several rows otherwise
 * duplicate: rowErrorForStatus knows how to project `declined` /
 * `killed` / `errored` into a tone+message pair, but a tool that
 * reached `failure` via a non-status signal (deriveCompletionStatus
 * inspecting payloadMeta.isError, command exitCode>0, etc.) still
 * needs a generic "X failed" row. Callers pass the per-row
 * failure copy (`"Agent failed"`, `"Advisor call failed"`).
 */
export function rowErrorWithFallback(
  item: Pick<Item, 'kind' | 'status' | 'isBackground' | 'payloadMeta'>,
  options: { meta?: Record<string, unknown> | null; fallback: string; stopped?: string },
): RowErrorData | null {
  if (deriveCompletionStatus(item, { meta: options.meta }) !== 'failure') return null;
  return rowErrorForStatus(item.status, options.fallback, options.stopped) ?? {
    tone: 'error',
    msg: options.fallback,
  };
}
