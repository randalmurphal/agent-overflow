import type { Item } from '../../types/models';
import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';

export type IndicatorState = 'running' | 'backgrounded' | 'error' | 'declined' | null;

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

export function rowErrorForStatus(status: ItemStatus, fallback: string): RowErrorData | null {
  if (status === 'declined') return { tone: 'declined', msg: 'Tool call declined' };
  if (status === 'killed') return { tone: 'error', msg: 'Tool call stopped' };
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
  options: { meta?: Record<string, unknown> | null; fallback: string },
): RowErrorData | null {
  if (deriveCompletionStatus(item, { meta: options.meta }) !== 'failure') return null;
  return rowErrorForStatus(item.status, options.fallback) ?? {
    tone: 'error',
    msg: options.fallback,
  };
}
