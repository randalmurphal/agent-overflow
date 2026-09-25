import type { Item } from '../../types/models';
import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
import { parseJsonObject } from '../../utils/parseJsonObject';

// `parked` is a parked stop's own status (claude-wire.md §E6b): the sibling
// a background agent's paused run writes. Its launch row stays `running`,
// and a live surface reads a current pause from the served run state
// (stores/subagentRunState.svelte.ts). `settled` is a settled background
// launch: the `backgrounded` box with its dots hidden. Anything other than
// the indicator treats it as `null`.
export type IndicatorState = 'running' | 'backgrounded' | 'settled' | 'parked' | 'error' | 'declined' | null;

type ItemStatus = Item['status'];

interface IndicatorOptions {
  /** The status row's parsed payload meta: a completion's failure signals. */
  payloadMeta?: Record<string, unknown> | null;
}

export interface RowErrorData {
  code?: string;
  msg: string;
  tone: 'error' | 'declined';
}

/**
 * A background launch row keeps status `running` for good; its outcome is a
 * completion sibling. Its `backgrounded` dots show until the launch settles
 * and then turn off once: the one change a launch row shows after it is
 * written (docs/specs/agent-visibility.md#immutable-agent-history). The
 * `settled` state keeps the dots' box and hides the dots, so nothing on the
 * row moves. Settled is the store's `live_background_active` bit on the
 * row's own `meta`, set false at the ending sibling or the session's death
 * and not at a parked stop. No live state is read.
 */
export function indicatorStateForItem(
  item: Pick<Item, 'kind' | 'status' | 'isBackground' | 'payloadMeta' | 'meta'>,
  options: IndicatorOptions = {},
): IndicatorState {
  if (
    item.kind === 'tool_call' &&
    item.isBackground === true &&
    (item.status === 'running' || item.status === 'streaming')
  ) {
    return backgroundLaunchSettled(item) ? 'settled' : 'backgrounded';
  }
  if (item.status === 'running' || item.status === 'streaming') return 'running';
  if (item.status === 'parked') return 'parked';
  if (item.status === 'declined') return 'declined';
  if (item.status === 'errored' || item.status === 'killed') return 'error';
  return deriveCompletionStatus(item, { meta: options.payloadMeta }) === 'failure' ? 'error' : null;
}

function backgroundLaunchSettled(item: Pick<Item, 'meta'>): boolean {
  return parseJsonObject(item.meta)?.live_background_active === false;
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
