import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';

interface ParsedCodexSubagentInput {
  tool: string;
  prompt: string;
  model: string;
  reasoningEffort: string;
  receiverThreadIds: string[];
  agentNickname: string;
  agentRole: string;
}

export interface CodexSubagentLaunchInfo extends ParsedCodexSubagentInput {
  agentLabel: string;
  modelAffix: string;
  title: string;
}

function parsedInput(item: Item): Record<string, unknown> {
  const payloadInput = parseJsonObject(item.payloadMeta)?.input;
  if (isObjectInput(payloadInput)) return payloadInput;
  const metaInput = parseJsonObject(item.meta)?.input;
  return isObjectInput(metaInput) ? metaInput : {};
}

function isObjectInput(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function isSpawnAgentTool(raw: string): boolean {
  return raw === '' || raw === 'spawn_agent' || raw === 'spawnAgent';
}

function stringValue(input: Record<string, unknown>, ...keys: string[]): string {
  for (const key of keys) {
    const value = input[key];
    if (typeof value === 'string' && value.trim()) return value.trim();
  }
  return '';
}

function stringArray(input: Record<string, unknown>, key: string): string[] {
  const value = input[key];
  return Array.isArray(value)
    ? value.filter((entry): entry is string => typeof entry === 'string' && entry.trim().length > 0)
    : [];
}

function parseCodexSubagentInput(item: Item): ParsedCodexSubagentInput {
  const input = parsedInput(item);
  return {
    tool: stringValue(input, 'tool'),
    prompt: stringValue(input, 'prompt'),
    model: stringValue(input, 'model'),
    reasoningEffort: stringValue(input, 'reasoningEffort', 'reasoning_effort'),
    receiverThreadIds: stringArray(input, 'receiverThreadIds'),
    agentNickname: stringValue(input, 'newAgentNickname', 'agentNickname', 'nickname'),
    agentRole: stringValue(input, 'newAgentRole', 'agentRole', 'agent_type', 'agentType'),
  };
}

export function codexSubagentDisplayLabel(label: string, role: string, fallback: string): string {
  const base = label.trim() || fallback.trim() || 'agent';
  return role.trim() ? `${base} [${role.trim()}]` : base;
}

/**
 * Codex's spawned-child launch is normalized as `toolName=collab_agent`.
 * Other collab controls use dedicated tool names (`send_input`, `wait_agent`,
 * etc.), so a collab_agent row is the stable frontend signal for the spawn
 * card. If metadata carries the raw collab tool, require it to be spawn_agent.
 */
export function isCodexSubagentLaunchItem(item: Item): boolean {
  if (item.kind !== 'tool_call') return false;
  if ((item.toolName ?? '').trim() !== 'collab_agent') return false;
  return isSpawnAgentTool(parseCodexSubagentInput(item).tool);
}

export function codexSubagentLaunchInfo(item: Item): CodexSubagentLaunchInfo {
  const parsed = parseCodexSubagentInput(item);
  const fallbackLabel = parsed.receiverThreadIds.length === 1
    ? 'Agent'
    : parsed.receiverThreadIds.length > 1
      ? `${parsed.receiverThreadIds.length} agents`
      : 'agent';
  const roleLabel = codexSubagentDisplayLabel(parsed.agentNickname, parsed.agentRole, fallbackLabel);
  const modelAffix = [parsed.model, parsed.reasoningEffort].filter(Boolean).join(' ');
  return {
    ...parsed,
    agentLabel: roleLabel,
    modelAffix,
    title: `Spawned ${roleLabel}`,
  };
}

export function codexSubagentReceiverLabels(items: readonly Item[]): Map<string, string> {
  const labels = new Map<string, string>();
  for (const item of items) {
    if (!isCodexSubagentLaunchItem(item)) continue;
    const launchInfo = codexSubagentLaunchInfo(item);
    for (const receiverThreadId of launchInfo.receiverThreadIds) {
      labels.set(receiverThreadId, launchInfo.agentLabel);
    }
  }
  return labels;
}
