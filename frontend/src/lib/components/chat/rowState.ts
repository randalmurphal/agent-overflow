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

export function indicatorAriaLabel(state: IndicatorState): string | undefined {
  if (state === 'running') return 'Running';
  if (state === 'backgrounded') return 'Backgrounded';
  if (state === 'error') return 'Errored';
  if (state === 'declined') return 'Declined';
  return undefined;
}
