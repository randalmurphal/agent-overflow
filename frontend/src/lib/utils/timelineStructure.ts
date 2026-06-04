import type { Item } from '../types/models';
import { extractClaudeTaskID } from './claudeTaskMeta';

function notificationFilterFingerprint(item: Item): string {
  if (item.kind === 'notification') return `notification:${extractClaudeTaskID(item) ?? ''}`;
  if (item.kind === 'tool_completion') return `completion:${extractClaudeTaskID(item) ?? ''}`;
  if (item.kind !== 'tool_call') return '';
  const taskId = extractClaudeTaskID(item);
  if (!taskId) return '';
  const completedFlag = item.status === 'completed' ? 'completed' : 'not-completed';
  return `tool:${completedFlag}:${taskId}`;
}

function subagentGroupingFingerprint(item: Item): string {
  return [
    item.parentId ?? '',
    item.completionOf ?? '',
    item.toolName ?? '',
    item.isBackground === true ? 'background' : 'foreground',
    item.kind === 'terminal_interaction' ? item.meta ?? '' : '',
    item.kind === 'tool_call' && item.toolName === 'wait_agent' ? item.meta ?? '' : '',
    item.kind === 'tool_call' && (item.toolName === 'Agent' || item.toolName === 'Task') ? item.meta ?? '' : '',
    item.kind === 'tool_call' && item.toolName === 'collab_agent'
      ? [item.meta ?? '', item.payloadMeta ?? ''].join('\x1f')
      : '',
  ].join('\x1f');
}

function readGroupingFingerprint(item: Item): string {
  return [
    item.kind,
    item.toolName ?? '',
    item.isBackground === true ? 'background' : 'foreground',
  ].join('\x1f');
}

function receiverLabelFingerprint(item: Item): string {
  if (item.kind !== 'tool_call' || item.toolName !== 'collab_agent') return '';
  return [item.meta ?? '', item.payloadMeta ?? ''].join('\x1f');
}

function structuralMetaFingerprint(item: Item): string {
  if (item.kind === 'notification') return item.meta ?? '';
  if (item.kind === 'tool_completion') return item.meta ?? '';
  if (item.completionOf) return item.meta ?? '';
  return '';
}

function structuralPayloadFingerprint(item: Item): string {
  if (item.kind === 'tool_completion') return item.payloadId ?? '';
  return '';
}

export function itemTimelineStructureKey(item: Item): string {
  return [
    item.id,
    item.threadId,
    item.turnIndex,
    item.itemIndex,
    item.kind,
    subagentGroupingFingerprint(item),
    readGroupingFingerprint(item),
    notificationFilterFingerprint(item),
    receiverLabelFingerprint(item),
    structuralMetaFingerprint(item),
    structuralPayloadFingerprint(item),
  ].join('\x1e');
}

export function itemTimelineStructureChanged(previous: Item | undefined, next: Item): boolean {
  return previous === undefined || itemTimelineStructureKey(previous) !== itemTimelineStructureKey(next);
}
