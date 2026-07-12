import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';
import { displayModelLabel } from './modelLabels';

interface ParsedCodexSubagentInput {
  tool: string;
  activityKind: string;
  prompt: string;
  model: string;
  reasoningEffort: string;
  receiverThreadIds: string[];
  agentNickname: string;
  agentRole: string;
  agentPath: string;
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
    activityKind: stringValue(input, 'activityKind'),
    prompt: stringValue(input, 'prompt'),
    model: stringValue(input, 'model'),
    reasoningEffort: stringValue(input, 'reasoningEffort', 'reasoning_effort'),
    receiverThreadIds: stringArray(input, 'receiverThreadIds'),
    agentNickname: stringValue(input, 'newAgentNickname', 'agentNickname', 'nickname'),
    agentRole: stringValue(input, 'newAgentRole', 'agentRole', 'agent_type', 'agentType'),
    agentPath: stringValue(input, 'taskName', 'agentPath', 'task_name', 'agent_path'),
  };
}

function agentPathLabel(agentPath: string): string {
  const segments = agentPath.split('/').map((segment) => segment.trim()).filter(Boolean);
  return segments.at(-1) ?? '';
}

export function codexSubagentDisplayLabel(label: string, role: string, fallback: string): string {
  const base = label.trim() || fallback.trim() || 'agent';
  return role.trim() ? `${base} [${role.trim()}]` : base;
}

export function codexModelEffortAffix(model: string, reasoningEffort: string): string {
  const modelLabel = model ? displayModelLabel('codex', model) : '';
  return [modelLabel, reasoningEffort].filter(Boolean).join(' ');
}

export function codexAgentMetadataAffix(role: string, model: string, reasoningEffort: string): string {
  const modelLabel = model ? displayModelLabel('codex', model) : '';
  return [role.trim() || 'default', modelLabel, reasoningEffort.trim()].filter(Boolean).join(' - ');
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
  const isV2Activity = parsed.activityKind !== '';
  const failedWithoutReceiver =
    parsed.receiverThreadIds.length === 0 &&
    (item.status === 'errored' || item.status === 'killed' || item.status === 'declined');
  const fallbackLabel = agentPathLabel(parsed.agentPath) || (
    parsed.receiverThreadIds.length === 1
      ? 'Agent'
      : parsed.receiverThreadIds.length > 1
        ? `${parsed.receiverThreadIds.length} agents`
        : 'agent'
  );
  const identityLabel = parsed.agentNickname.trim() || fallbackLabel;
  const roleLabel = isV2Activity
    ? identityLabel
    : codexSubagentDisplayLabel(parsed.agentNickname, parsed.agentRole, fallbackLabel);
  const modelAffix = isV2Activity
    ? codexAgentMetadataAffix(parsed.agentRole, parsed.model, parsed.reasoningEffort)
    : codexModelEffortAffix(parsed.model, parsed.reasoningEffort);
  return {
    ...parsed,
    prompt: isV2Activity ? '' : parsed.prompt,
    agentRole: isV2Activity ? parsed.agentRole || 'default' : parsed.agentRole,
    agentLabel: roleLabel,
    modelAffix,
    title: failedWithoutReceiver ? 'Agent spawn failed' : `Spawned ${roleLabel}`,
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
