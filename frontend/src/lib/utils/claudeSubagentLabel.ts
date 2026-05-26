// Shared helpers for rendering a Claude `Agent` tool call's header text
// (label + model affix + description). Used by both `SubagentGroup`
// (foreground subagents that render as a transcript group) and
// `GenericToolCallRow` (backgrounded subagents that render as leaves
// and in the activity-rail tray). Keeping the derivations in one place
// is what guarantees the two surfaces stay visually aligned —
// pre-extraction, the tray rendered the bare classifier label
// ("Subagent") with no model affix and the raw "Agent: …" summary as
// the preview, which looked completely separate from the subagent card.
//
// Naming mirrors the sibling Codex util (`subagentLaunch.ts` exports
// `codexSubagentLaunchInfo` etc.) — every export here carries the
// `claude` prefix so call sites read unambiguously next to imported
// Codex helpers.

import { parseJsonObject } from './parseJsonObject';
import { displayModelLabel } from './modelLabels';

function readStringField(obj: Record<string, unknown> | null, key: string): string {
  if (!obj) return '';
  const v = obj[key];
  return typeof v === 'string' ? v.trim() : '';
}

/**
 * Split CamelCase / kebab-case / snake_case into words and capitalise
 * each. "spawnAgent" → "Spawn Agent"; "general-purpose" → "General
 * Purpose".
 */
export function titleCaseClaudeSubagentToken(raw: string): string {
  if (!raw) return '';
  const tokens = raw
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .split(/[-_\s]+/)
    .filter((t) => t.length > 0);
  return tokens.map((t) => t.charAt(0).toUpperCase() + t.slice(1)).join(' ');
}

/**
 * Resolve the input object for a Claude subagent launch. The Claude
 * parser stamps tool input on `marshalToolMeta` output, which lands on
 * both `payloadMeta` (via the EventToolStart payload) and `meta` (via
 * the items.meta merge) — payloadMeta wins because triage settles it
 * later in the pipeline. Returns null when neither carries an object.
 *
 * Centralised so the precedence rule stays in one place; both
 * SubagentGroup and GenericToolCallRow used to re-implement this with
 * a literal `?? ` chain.
 */
export function readClaudeSubagentInput(
  payloadMeta: Record<string, unknown> | null,
  parentMeta: Record<string, unknown> | null,
): Record<string, unknown> | null {
  const raw = payloadMeta?.input ?? parentMeta?.input;
  if (raw && typeof raw === 'object' && !Array.isArray(raw)) {
    return raw as Record<string, unknown>;
  }
  return null;
}

/**
 * Convenience wrapper: parse `payloadMeta` and `meta` from an item-like
 * value and resolve the subagent input through `readClaudeSubagentInput`.
 * Caller passes the JSON strings directly so the helper can be reused
 * outside Svelte components without re-implementing the parse.
 */
export function readClaudeSubagentInputFromStrings(
  payloadMetaJson: string | undefined,
  metaJson: string | undefined,
): Record<string, unknown> | null {
  return readClaudeSubagentInput(
    parseJsonObject(payloadMetaJson),
    parseJsonObject(metaJson),
  );
}

/**
 * Title-cased label for an Agent tool call header — e.g. "Explore",
 * "General Purpose". Falls back through input.subagent_type → toolName
 * → "Subagent" so the row never renders an empty label.
 */
export function deriveClaudeSubagentLabel(
  input: Record<string, unknown> | null,
  toolName: string,
): string {
  if (toolName === 'Agent') {
    return titleCaseClaudeSubagentToken(readStringField(input, 'subagent_type') || 'Agent');
  }
  return titleCaseClaudeSubagentToken(toolName || 'Subagent');
}

/**
 * Subagent model affix string for an Agent tool call. Resolution order:
 *   1. parentMeta.subagent_model — stamped by the Claude parser on the
 *      first subagent assistant envelope (most authoritative).
 *   2. input.model — user-supplied alias on the tool input (surfaces
 *      something for the brief window before the first subagent
 *      assistant message lands).
 *   3. '' otherwise.
 *
 * Non-Agent tool names return '' so a row with `isSubagent` false in
 * the classifier never renders a stray "()" affix.
 */
export function deriveClaudeSubagentModelLabel(
  input: Record<string, unknown> | null,
  parentMeta: Record<string, unknown> | null,
  toolName: string,
): string {
  if (toolName !== 'Agent') return '';
  const stamped =
    parentMeta && typeof parentMeta.subagent_model === 'string'
      ? parentMeta.subagent_model.trim()
      : '';
  if (stamped) return displayModelLabel('claude', stamped);
  const requested = readStringField(input, 'model');
  if (requested) return displayModelLabel('claude', requested);
  return '';
}

/**
 * Single-line description rendered next to the label — input.description
 * verbatim, or input.prompt truncated to 80 chars when description is
 * missing. Empty when neither is present.
 */
export function deriveClaudeSubagentDescription(
  input: Record<string, unknown> | null,
): string {
  const desc = readStringField(input, 'description');
  if (desc) return desc;
  const prompt = readStringField(input, 'prompt');
  if (prompt) return prompt.length > 80 ? `${prompt.slice(0, 80)}…` : prompt;
  return '';
}
