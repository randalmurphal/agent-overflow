import type { Item } from '../../types/models';
import {
  codexSubagentDisplayLabel,
  codexSubagentLaunchInfo,
  isCodexSubagentLaunchItem,
  type CodexSubagentLaunchInfo,
} from '../../utils/subagentLaunch';
import {
  stringArrayValue,
  waitAgentDisplayReceiverIds,
  waitAgentRequestedReceiverIds,
} from '../../utils/waitAgentDisplay';

export interface ReceiverAgentLabel {
  threadId: string;
  label: string;
}

export function stringValue(obj: Record<string, unknown>, key: string): string {
  const value = obj[key];
  return typeof value === 'string' ? value.trim() : '';
}

export function previewText(raw: string, maxLength = 160): string {
  const normalized = raw.replace(/\s+/g, ' ').trim();
  if (normalized.length <= maxLength) return normalized;
  return `${normalized.slice(0, maxLength).trimEnd()}...`;
}

export function collabInputFromMeta(
  meta: Record<string, unknown> | null,
  payloadMeta: Record<string, unknown> | null,
): Record<string, unknown> {
  const raw = meta?.input ?? payloadMeta?.input;
  return raw && typeof raw === 'object' && !Array.isArray(raw)
    ? raw as Record<string, unknown>
    : {};
}

function labelForAgentRecord(record: Record<string, unknown>): ReceiverAgentLabel | null {
  const threadId = stringValue(record, 'threadId') || stringValue(record, 'thread_id');
  if (!threadId) return null;
  const nickname =
    stringValue(record, 'newAgentNickname') ||
    stringValue(record, 'agentNickname') ||
    stringValue(record, 'agent_nickname') ||
    stringValue(record, 'nickname');
  const role =
    stringValue(record, 'newAgentRole') ||
    stringValue(record, 'agentRole') ||
    stringValue(record, 'agent_role') ||
    stringValue(record, 'agentType') ||
    stringValue(record, 'agent_type');
  if (!nickname && !role) return null;
  return { threadId, label: codexSubagentDisplayLabel(nickname, role, 'Agent') };
}

export function receiverAgentLabels(obj: Record<string, unknown>, keys: string[]): ReceiverAgentLabel[] {
  return keys
    .flatMap((key) => {
      const raw = obj[key];
      return Array.isArray(raw) ? raw : [];
    })
    .filter((entry): entry is Record<string, unknown> =>
      Boolean(entry) && typeof entry === 'object' && !Array.isArray(entry))
    .map(labelForAgentRecord)
    .filter((entry): entry is ReceiverAgentLabel => entry !== null);
}

export function collabToolName(item: Item, input: Record<string, unknown>): string {
  if (isCodexSubagentLaunchItem(item)) {
    return codexSubagentLaunchInfo(item).tool || 'spawn_agent';
  }
  return stringValue(input, 'tool') || item.toolName || '';
}

export function collabSpawnInfo(item: Item): CodexSubagentLaunchInfo | null {
  return isCodexSubagentLaunchItem(item) ? codexSubagentLaunchInfo(item) : null;
}

export function receiverIdsForTool(
  tool: string,
  input: Record<string, unknown>,
  spawnInfo: CodexSubagentLaunchInfo | null,
): string[] {
  if (spawnInfo) return spawnInfo.receiverThreadIds;
  if (tool === 'wait_agent') return waitAgentDisplayReceiverIds(input);
  return stringArrayValue(input, 'receiverThreadIds');
}

export function receiverLabelMap(
  input: Record<string, unknown>,
  usesRequestedWaitReceivers: boolean,
): Map<string, string> {
  const labels = new Map<string, string>();
  const receiverLabels = receiverAgentLabels(input, ['receiverAgents', 'agentStatuses']);
  const requestedReceiverLabels = receiverAgentLabels(input, ['requestedReceiverAgents']);
  const primaryLabels = usesRequestedWaitReceivers ? requestedReceiverLabels : receiverLabels;
  const fallbackLabels = usesRequestedWaitReceivers ? receiverLabels : requestedReceiverLabels;
  for (const agent of primaryLabels) labels.set(agent.threadId, agent.label);
  for (const agent of fallbackLabels) {
    if (!labels.has(agent.threadId)) labels.set(agent.threadId, agent.label);
  }
  return labels;
}

export function usesRequestedWaitReceivers(tool: string, input: Record<string, unknown>): boolean {
  return tool === 'wait_agent' && waitAgentRequestedReceiverIds(input).length > 0;
}
