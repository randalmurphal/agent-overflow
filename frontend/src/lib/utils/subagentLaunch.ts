// Provider-neutral subagent-launch predicate plus the Codex spawn-row
// label helpers it shares with `CollabToolRow`.
//
// `subagentLaunchInfo` is THE launch predicate: one function that answers
// "does this row anchor a subagent?" for every shape AO renders as an
// agent card (docs/specs/agent-visibility.md § "Anchor set"). Everything
// above it — the timeline tree (`utils/subagentGrouping.ts`), the pane's
// live-eviction fold (`stores/threadSubagentMemory.ts`), and the agent
// pane — is provider-neutral and must key on this and nothing else, so a
// new launch shape lands in one place.
//
// The four shapes, and the wire fact each is read from:
//
//   - Claude `Agent` / `Task` tool_call — the ordinary subagent launch.
//     `isBackground` distinguishes awaited from async (claude-wire.md
//     §E5: the CLI stamps it from the `isAsync` / `async_launched` ack,
//     so a foreground launch is the ABSENCE of the flag, never `false`).
//   - Claude forked `Skill` tool_call — structural detection only, never
//     a skill-name list (claude-wire.md §E9). A fork is proven by any of
//     three signals, in cost order: a loaded child row attributed to the
//     Skill tool_use, the `skillFork` stamp the parser writes from the
//     completion's `tool_use_result {status:"forked", agentId,
//     commandName}`, or the store's `subagentDescendantCount` decoration
//     (history windows load no children at all, so neither of the first
//     two is available on a cold thread). An INLINE skill has none of
//     them and is not a launch — it stays a plain tool row.
//   - Claude `SendMessage` background carrier — the §E6 resume rebind.
//     The parser marks the resuming tool_use backgrounded and triage
//     stamps the rebound `task_id` on it, so "backgrounded SendMessage
//     carrying a task id" is the carrier and nothing else is.
//   - Codex `spawn_agent` (normalized to `toolName=collab_agent`) — child
//     threads, always asynchronous.
//
// `ctx.hasChildren` is supplied by the caller because only the caller
// knows which rows are loaded; it is consulted ONLY for `Skill` rows, so
// a context that builds an index lazily pays nothing on a thread with no
// skills in the window.

import type { Item } from '../types/models';
import { parseJsonObject } from './parseJsonObject';
import { displayModelLabel } from './modelLabels';
import { extractClaudeTaskID } from './claudeTaskMeta';
import {
  deriveClaudeSubagentLabel,
  readClaudeSubagentInputFromStrings,
} from './claudeSubagentLabel';

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

/**
 * `name [role]` — but only when the two actually differ. Codex spawns are
 * routinely nicknamed after their role, and "reviewer [reviewer]" is noise
 * that also costs the row its truncation budget. Matched case-insensitively:
 * the nickname is free-form user text, the role is an enum-ish slug.
 */
export function codexSubagentDisplayLabel(label: string, role: string, fallback: string): string {
  const base = label.trim() || fallback.trim() || 'agent';
  const roleLabel = role.trim();
  if (!roleLabel || roleLabel.toLowerCase() === base.toLowerCase()) return base;
  return `${base} [${roleLabel}]`;
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

// ---------------------------------------------------------------------
// Provider-neutral launch predicate
// ---------------------------------------------------------------------

/**
 * What kind of node a launch anchors. `agent` is a conversational
 * subagent (Claude `Agent`/`Task`, a resumed async agent, a Codex
 * spawned child); `skill` is a forked Skill, which runs as a subagent
 * whose context is a copy of the parent's (claude-wire.md §E9) and has
 * no task id, so it can never be killed or backgrounded by id.
 */
export type SubagentLaunchKind = 'agent' | 'skill';

export interface SubagentLaunchInfo {
  kind: SubagentLaunchKind;
  provider: 'claude' | 'codex';
  /**
   * The launch runs asynchronously — the main turn is not blocked on it.
   * Codex children are always async; a Claude agent is async when the CLI
   * stamped `is_background` (an explicit `run_in_background`, the §E5
   * async ack, or a mid-flight `background_tasks` control_request).
   */
  background: boolean;
  /** Display name for the card header (`Explore`, `code-review`, …). */
  name: string;
  /** Claude's `subagent_type` when the launch input carried one. */
  agentType?: string;
}

/**
 * What the predicate needs from the caller: whether any LOADED row is
 * attributed to `itemId`. Only forked-Skill detection consults it.
 */
export interface SubagentLaunchContext {
  hasChildren(itemId: string): boolean;
}

/**
 * Context for callers with no loaded rows to offer (a single row read in
 * isolation — a tray entry, a background-section entry). Forked skills
 * then rest on their two meta signals, which is exactly the cold-thread
 * case those surfaces are in anyway.
 */
export const NO_LOADED_SUBAGENT_CHILDREN: SubagentLaunchContext = {
  hasChildren: () => false,
};

/**
 * Build a context over a list of loaded items. The parent-id index is
 * built on FIRST USE, not eagerly: `subagentLaunchInfo` reaches
 * `hasChildren` only for `Skill` rows, so a window with none never pays
 * for the pass.
 */
export function subagentLaunchContextFrom(
  items: readonly Item[],
): SubagentLaunchContext {
  let parentIds: Set<string> | null = null;
  return {
    hasChildren(itemId: string): boolean {
      if (parentIds === null) {
        parentIds = new Set<string>();
        for (const item of items) {
          const parentId = item.parentId ?? '';
          if (parentId) parentIds.add(parentId);
        }
      }
      return parentIds.has(itemId);
    },
  };
}

/**
 * Meta key the store stamps on every anchor it finds children for
 * (internal/store/subagent_items.go `decorateSubagentAnchors`). Read here
 * rather than in `subagentGrouping.ts` because forked-Skill detection is
 * one of its two consumers and this module must not import upward into
 * the grouping pass.
 */
export function subagentDescendantCountFromMeta(
  meta: Record<string, unknown> | null,
): number {
  const raw = meta?.subagentDescendantCount;
  return typeof raw === 'number' && Number.isFinite(raw) && raw > 0
    ? Math.floor(raw)
    : 0;
}

/**
 * Cheap tool-name prefilter — a strict SUPERSET of `subagentLaunchInfo`,
 * parsing no meta. Used by the grouping pass's no-signal fast path, which
 * only has to be conservative: a false positive costs one grouping walk,
 * a false negative would render a launch as a leaf.
 */
export function isPotentialSubagentLaunch(item: Item): boolean {
  if (item.kind !== 'tool_call') return false;
  const toolName = (item.toolName ?? '').trim();
  return (
    toolName === 'Agent'
    || toolName === 'Task'
    || toolName === 'Skill'
    || toolName === 'SendMessage'
    || toolName === 'collab_agent'
  );
}

function trimmedInputString(
  input: Record<string, unknown> | null,
  key: string,
): string {
  const value = input?.[key];
  return typeof value === 'string' ? value.trim() : '';
}

/**
 * Structural fork detection (claude-wire.md §E9) — "did this Skill fork"
 * is answered by attribution and by the completion's `status:"forked"`,
 * never by a skill-name list. Three signals, checked cheapest first:
 *
 *  1. a loaded row is attributed to the Skill tool_use;
 *  2. `meta.skillFork` — what the parser stamps from the fork's
 *     completion (`{agentId, commandName}`), which survives on the launch
 *     row because triage merges completion meta into it;
 *  3. the store's descendant-count decoration, the only signal available
 *     on a history window (which loads no child rows at all).
 */
function isForkedSkillLaunch(item: Item, ctx: SubagentLaunchContext): boolean {
  if (ctx.hasChildren(item.id)) return true;
  const meta = parseJsonObject(item.meta);
  const fork = meta?.skillFork;
  if (fork !== null && typeof fork === 'object' && !Array.isArray(fork)) return true;
  return subagentDescendantCountFromMeta(meta) > 0;
}

function forkedSkillName(item: Item): string {
  const input = readClaudeSubagentInputFromStrings(item.payloadMeta, item.meta);
  const fromInput = trimmedInputString(input, 'skill') || trimmedInputString(input, 'command');
  if (fromInput) return fromInput;
  const fork = parseJsonObject(item.meta)?.skillFork;
  if (fork !== null && typeof fork === 'object' && !Array.isArray(fork)) {
    const commandName = (fork as Record<string, unknown>).commandName;
    if (typeof commandName === 'string' && commandName.trim()) return commandName.trim();
  }
  return 'Skill';
}

/**
 * Name for a §E6 resume carrier. The resuming tool is a `SendMessage`, so
 * its own input says nothing about the agent; the parser stamps the
 * ORIGINAL agent's `description` off the rebind `task_started`, and
 * triage rewrites the row Summary to `Agent: <description>` from the
 * original launch. Read the stamp first, fall back to un-prefixing the
 * summary, and never render the bare tool name.
 */
const RESUME_SUMMARY_PREFIX = 'Agent: ';

function resumedAgentName(item: Item): string {
  const description = parseJsonObject(item.meta)?.description;
  if (typeof description === 'string' && description.trim()) return description.trim();
  const summary = (item.summary ?? '').trim();
  if (summary.startsWith(RESUME_SUMMARY_PREFIX)) {
    const stripped = summary.slice(RESUME_SUMMARY_PREFIX.length).trim();
    if (stripped) return stripped;
  }
  return 'Agent';
}

/**
 * Whether a launch runs DETACHED from the main turn — stamped at launch
 * (the §E5 async ack, an explicit `run_in_background`, a Codex spawn, a
 * SendMessage resume carrier) or mid-flight (the background button /
 * Ctrl+B, which triage stamps on the launch row as
 * `meta.subagentBackgroundedAt`).
 *
 * The one rule the card and the grouping share for "does this launch keep
 * its pre-card launch row?": a detached launch's row never changes after
 * the spawn (the tray invariant — it stays `running` forever and its
 * outcome lands on a separate `complete:<id>` sibling), so it is never a
 * group. A Claude detached launch's card sits at its completion point
 * (`SubagentGroupNode.anchor`); a Codex spawn has no card.
 */
export function launchRunsDetached(
  info: SubagentLaunchInfo | null,
  meta: Record<string, unknown> | null,
): boolean {
  return info?.background === true || meta?.subagentBackgroundedAt !== undefined;
}

/**
 * THE launch predicate. Returns null for every row that does not anchor a
 * subagent — including an inline (non-forking) `Skill` and a
 * `SendMessage` that is just a message.
 */
export function subagentLaunchInfo(
  item: Item,
  ctx: SubagentLaunchContext,
): SubagentLaunchInfo | null {
  if (item.kind !== 'tool_call') return null;
  const toolName = (item.toolName ?? '').trim();

  if (toolName === 'Agent' || toolName === 'Task') {
    const input = readClaudeSubagentInputFromStrings(item.payloadMeta, item.meta);
    const agentType = trimmedInputString(input, 'subagent_type');
    return {
      kind: 'agent',
      provider: 'claude',
      // Optional on the wire: undefined and false both mean awaited, so
      // this is deliberately not a strict `=== false` negation.
      background: item.isBackground === true,
      // `Task` is the older name for the same tool; both resolve through
      // the `Agent` label rules so one launch never renders two ways.
      name: deriveClaudeSubagentLabel(input, 'Agent'),
      ...(agentType ? { agentType } : {}),
    };
  }

  if (toolName === 'Skill') {
    if (!isForkedSkillLaunch(item, ctx)) return null;
    return {
      kind: 'skill',
      provider: 'claude',
      background: item.isBackground === true,
      name: forkedSkillName(item),
    };
  }

  if (toolName === 'SendMessage') {
    // A resume carrier is backgrounded AND bound to the resumed agent's
    // task id; an ordinary SendMessage is neither.
    if (item.isBackground !== true) return null;
    if (!extractClaudeTaskID(item)) return null;
    return {
      kind: 'agent',
      provider: 'claude',
      background: true,
      name: resumedAgentName(item),
    };
  }

  if (isCodexSubagentLaunchItem(item)) {
    const codex = codexSubagentLaunchInfo(item);
    const agentType = codex.agentRole.trim();
    return {
      kind: 'agent',
      provider: 'codex',
      // Codex children are their own threads; the parent never awaits one
      // inline, it waits on them explicitly through `wait_agent`.
      background: true,
      name: codex.agentLabel,
      ...(agentType ? { agentType } : {}),
    };
  }

  return null;
}

/**
 * The COLLAPSED one-line summary of a Codex child's outcome, read off
 * the spawn launch's completion sibling (`payloadMeta.preview`, the
 * first 240 chars of the delivered FINAL_ANSWER — triage's
 * `completionPayload`). A settled agent's conclusion is what a reader
 * wants on the collapsed row, not its last progress beat.
 *
 * Preview ONLY. The answer itself is not rendered from here: a Codex
 * child streams its whole transcript to the parent thread as rows
 * parented to the launch (`isUnsafeChildProjectionEvent` lets assistant
 * text through), so the final answer already IS a normal message in the
 * card body and the pane. Rendering the preview as a second body block
 * showed the same text twice, unformatted and cut mid-word.
 *
 * Empty for Claude launches: their sibling carries the formulaic ack
 * whose rendering the spec deletes, and the real transcript exists as
 * attributed rows.
 */
export function codexCompletionPreview(
  launch: Item | null | undefined,
  completion: Item | null | undefined,
): string {
  if (!launch || !completion) return '';
  const info = subagentLaunchInfo(launch, NO_LOADED_SUBAGENT_CHILDREN);
  if (info?.provider !== 'codex') return '';
  const preview = parseJsonObject(completion.payloadMeta)?.preview;
  return typeof preview === 'string' ? preview.trim() : '';
}

/**
 * The one-line task description a Codex spawn shows beside its label.
 *
 * Codex-shaped on purpose, so a spawn never reads the CLAUDE input keys
 * by coincidence — `description` there is returned untruncated, and
 * Codex growing a key by that name would put an unclamped string on the
 * row.
 *
 * MultiAgentV1 carries the real prompt in plaintext, so that wins.
 *
 * MultiAgentV2 has nothing to add and returns '': the model service
 * encrypts `spawn_agent.message` and the child's NEW_TASK payload alike
 * (docs/references/codex-wire.md), so the model-chosen task name is the
 * only plaintext statement of what the agent was asked to do — and the
 * card title and the pane breadcrumb ALREADY show it, because a V2 spawn
 * carries no nickname and `agentLabel` falls back to the agent path's own
 * tail. Repeating it would render "audit_internal_tail -
 * audit_internal_tail" and spend the row's truncation budget on nothing.
 */
export function codexSubagentTaskDescription(info: CodexSubagentLaunchInfo): string {
  const text = info.prompt || agentPathLabel(info.agentPath);
  if (!text || text.toLowerCase() === info.agentLabel.trim().toLowerCase()) return '';
  return text.length > 80 ? `${text.slice(0, 80)}\u2026` : text;
}
