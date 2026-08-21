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

// --- Collab interactions on the spawn card -----------------------------------
//
// MultiAgentV2 has no per-interaction transcript row: `send_message` and
// `followup_task` both end in one `subAgentActivity kind:"interacted"` item,
// and the child answers on its own schedule. Triage records each one on the
// owning spawn launch under `codex_collab_interactions`
// (internal/triage/codex_background.go) instead of minting a top-level row, so
// the whole conversation with one agent stays on that agent's card.

export const COLLAB_INTERACTION_KINDS = ['interacted', 'progress', 'resumed'] as const;
export type CollabInteractionKind = (typeof COLLAB_INTERACTION_KINDS)[number];

export interface CollabInteraction {
  id: string;
  kind: CollabInteractionKind;
  /**
   * The RAW Codex function-call name (`send_message` | `followup_task`).
   * Empty when the raw stream was unavailable — a resumed session sees only
   * the typed activity item, and the typed wire genuinely cannot tell the two
   * verbs apart. An empty value must be labelled neutrally; it must NEVER be
   * inferred from whether a child turn followed
   * (docs/architecture/invariants.md #25).
   */
  tool: string;
  /**
   * The bounded body of a child -> parent `MESSAGE` progress note (first line,
   * capped backend-side). Empty on every other kind, and on an ENCRYPTED
   * progress envelope, whose payload never leaves the ciphertext — so an empty
   * value means "no body on the wire", never "the child said nothing".
   */
  text: string;
  at: number;
}

/**
 * The spawn card's lifecycle state, derived from the child's last wire signal.
 *
 * - `live` — at least one child has not reported a terminal status.
 * - `interrupted` — a child ended via `interrupt_agent` (or `notFound`).
 * - `delivered` — a `FINAL_ANSWER` mailbox envelope reached the parent's model
 *   context (`codex_collab_delivered_at`).
 * - `idle` — every child is terminal but nothing has been drained into the
 *   parent. The child finished; its answer has not arrived (or never will).
 *
 * There is deliberately no `closed` state: MultiAgentV2 has no close verb, and
 * `subAgentActivity` has only three kinds (started | interacted | interrupted).
 * An `errored` child reads as `idle` here — how a child ENDED is what the row's
 * status indicator already says; this axis is only about the mailbox.
 */
export const COLLAB_CARD_STATES = ['live', 'interrupted', 'delivered', 'idle'] as const;
export type CollabCardState = (typeof COLLAB_CARD_STATES)[number];

function isInteractionKind(value: unknown): value is CollabInteractionKind {
  return COLLAB_INTERACTION_KINDS.includes(value as CollabInteractionKind);
}

export function collabInteractions(meta: Record<string, unknown> | null): CollabInteraction[] {
  const raw = meta?.codex_collab_interactions;
  if (!Array.isArray(raw)) return [];
  const out: CollabInteraction[] = [];
  for (const entry of raw) {
    if (!entry || typeof entry !== 'object' || Array.isArray(entry)) continue;
    const record = entry as Record<string, unknown>;
    const id = stringValue(record, 'id');
    const kind = record.kind;
    if (!id || !isInteractionKind(kind)) continue;
    out.push({
      id,
      kind,
      tool: stringValue(record, 'tool'),
      text: stringValue(record, 'text'),
      at: typeof record.at === 'number' && Number.isFinite(record.at) ? record.at : 0,
    });
  }
  return out;
}

/**
 * One sub-line's text: a verb, never a name.
 *
 * The card header already names the child, and a `codexCollabInteraction`
 * carries no receiver id, so a sub-line could not be attributed to a specific
 * agent even on a launch that somehow held more than one receiver. Repeating
 * the header's label on every line is therefore both noise and a claim the
 * record cannot back.
 *
 * `followup_task` is the only verb that names itself, because it is the only
 * one the raw stream can prove; `send_message` and an unknown verb both read
 * as "message sent", which is the weaker of the two claims and therefore the
 * safe one to make without evidence
 * (docs/architecture/invariants.md #25 — never infer the verb from whether a
 * child turn followed).
 */
export function collabInteractionLabel(entry: CollabInteraction): string {
  switch (entry.kind) {
    case 'resumed':
      return 'resumed';
    case 'progress':
      // The note itself when the envelope carried one. It is the only place
      // that text survives — the raw carrier is an internal event and nothing
      // else persists it — so a plaintext beat says what the child reported
      // rather than merely that it did.
      return entry.text ? `progress reported: ${entry.text}` : 'progress reported';
    case 'interacted':
      return entry.tool === 'followup_task' ? 'follow-up task sent' : 'message sent';
  }
}

function childTerminalStatuses(meta: Record<string, unknown> | null): Record<string, string> {
  const raw = meta?.codex_child_terminal_statuses;
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {};
  const out: Record<string, string> = {};
  for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof value === 'string') out[key] = value.trim();
  }
  return out;
}

export function collabCardState(
  meta: Record<string, unknown> | null,
  receiverThreadIds: readonly string[],
): CollabCardState | null {
  if (receiverThreadIds.length === 0) return null;
  const statuses = childTerminalStatuses(meta);
  let interrupted = false;
  for (const id of receiverThreadIds) {
    const status = statuses[id] ?? '';
    // An empty entry is a child that went terminal and then started a NEW turn
    // (reactivateCodexSpawnChild clears it rather than deleting the key), so it
    // counts as live exactly like a child that was never terminal.
    if (!status) return 'live';
    if (status === 'interrupted' || status === 'notFound') interrupted = true;
  }
  if (interrupted) return 'interrupted';
  const deliveredAt = meta?.codex_collab_delivered_at;
  return typeof deliveredAt === 'number' && deliveredAt > 0 ? 'delivered' : 'idle';
}
