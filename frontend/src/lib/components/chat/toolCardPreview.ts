import type { Item } from '../../types/models';
import type { ToolKindVisual } from './toolCardHeader';

export function parseToolCardMeta(raw: string | undefined): Record<string, unknown> | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (parsed && typeof parsed === 'object') return parsed as Record<string, unknown>;
  } catch {
    return null;
  }
  return null;
}

export function toolCardInputPreview(
  item: Item,
  classification: ToolKindVisual,
  summaryMeta: Record<string, unknown> | null,
  itemMeta: Record<string, unknown> | null,
): string {
  if (item.toolName === 'wait_agent') {
    return waitAgentPreview(item, itemMeta);
  }
  const fromSummary = (item.summary ?? '').trim();
  if (fromSummary) return fromSummary;
  if (summaryMeta) {
    const title = summaryMeta.title;
    if (typeof title === 'string' && title.trim()) return title.trim();
  }
  return classification.displayName;
}

function waitAgentPreview(item: Item, meta: Record<string, unknown> | null): string {
  const count = receiverThreadCount(meta);
  const noun = count === 1 ? 'agent' : 'agents';
  const verb = item.status === 'running' || item.status === 'streaming'
    ? 'Waiting on'
    : 'Waited on';
  return count > 0 ? `${verb} ${count} ${noun}` : `${verb} agents`;
}

function receiverThreadCount(meta: Record<string, unknown> | null): number {
  const input = meta?.input;
  if (!input || typeof input !== 'object') return 0;
  const receiverThreadIds = (input as Record<string, unknown>).receiverThreadIds;
  return Array.isArray(receiverThreadIds) ? receiverThreadIds.length : 0;
}
