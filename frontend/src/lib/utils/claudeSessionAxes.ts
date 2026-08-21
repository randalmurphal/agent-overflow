// The Claude-only session axes Settings writes: output style, subagent
// limits, and the tool memory limit — all three rendered into Claude Code's
// `--settings` block at spawn — plus extended thinking, which rides CLI flags
// at spawn and a control request on a RUNNING session.
//
// The peer inbox (`claudeCrossSession`) also rides that block but lives in
// claudeCrossSession.ts: it needs a spawn environment gate and a `--name`
// besides the settings key, and it is the one axis here whose change the
// backend reconciles onto running sessions.
//
// The option lists and the two validators here MIRROR
// internal/settings/claudesession.go. They are not the enforcement — the
// backend refuses a bad value on the patch path — they exist so a field can
// say WHY before the write. UpdateSettings rejects the whole patch over one
// bad value and the caller sees only "Failed to save setting" with the field
// rolled back under it, which is unactionable.

import type {
  ClaudeOutputStyle,
  ClaudeSubagentLimits,
  ClaudeThinking,
  ClaudeThinkingDisplay,
  ClaudeThinkingMode,
} from '../types/settings';

/** One selectable value plus the single line that explains it. */
export type AxisOption<T extends string> = {
  value: T | '';
  label: string;
  description: string;
};

// The style descriptions are Claude Code's own (2.1.237 built-in style
// definitions), not paraphrases: the CLI is the thing that behaves this way,
// and a restatement drifts from it on the next release.
//
// The empty option is FIRST and is not "off" — it is the absence of the
// settings key, which leaves Claude Code's own resolution in place. Naming it
// "Claude Code default" rather than "None" is the whole difference between a
// user picking it deliberately and a user thinking they switched a feature
// off.
export const CLAUDE_OUTPUT_STYLE_OPTIONS: AxisOption<ClaudeOutputStyle>[] = [
  {
    value: '',
    label: 'Claude Code default',
    description: 'Whatever style Claude Code or your settings files select.',
  },
  {
    value: 'Concise',
    label: 'Concise',
    description: 'Responds tersely, leading with results and skipping preamble and narration.',
  },
  {
    value: 'Proactive',
    label: 'Proactive',
    description: 'Executes immediately, minimizes interruptions, and prefers action over planning.',
  },
  {
    value: 'Explanatory',
    label: 'Explanatory',
    description: 'Explains its implementation choices and the codebase patterns behind them.',
  },
  {
    value: 'Learning',
    label: 'Learning',
    description: 'Pauses and asks you to write small pieces of code for hands-on practice.',
  },
];

/** Mirrors MaxClaudeSubagentLimit. Zero is "unset", never "none allowed". */
export const CLAUDE_SUBAGENT_LIMIT_MAX = 512;

/** Mirrors MaxClaudeToolMemoryLimitLen. */
export const CLAUDE_TOOL_MEMORY_LIMIT_MAX_LEN = 32;

// Mirrors the CLI's own parser, /^(\d+(?:\.\d+)?)\s*([kmgt]?)(?:i?b)?$/i.
const TOOL_MEMORY_SIZE = /^\d+(\.\d+)?\s*[kmgt]?(i?b)?$/i;

// The CLI's falsy set, plus "none" — the word a user reaches for first.
const TOOL_MEMORY_DISABLE_WORDS = new Set(['0', 'false', 'no', 'off', 'none']);

/**
 * Clamps a subagent limit into what the backend will store. A negative or
 * fractional entry becomes 0 ("unset") rather than being refused: the number
 * input can only produce one by being typed into, and silently meaning
 * "leave it to Claude Code" is the safe reading of an unfinished entry.
 */
export function clampSubagentLimit(value: number): number {
  if (!Number.isFinite(value)) return 0;
  const floored = Math.floor(value);
  if (floored <= 0) return 0;
  return Math.min(floored, CLAUDE_SUBAGENT_LIMIT_MAX);
}

/** True when neither axis carries a value, i.e. the whole group is unset. */
export function subagentLimitsAreEmpty(limits: ClaudeSubagentLimits): boolean {
  return clampSubagentLimit(limits.maxSpawnDepth ?? 0) === 0 &&
    clampSubagentLimit(limits.maxConcurrent ?? 0) === 0;
}

/**
 * Returns the refusal reason for a tool-memory-limit draft, or null when the
 * backend would accept it. An empty draft is always fine — it is the "let
 * Claude Code decide" value.
 */
export function toolMemoryLimitError(draft: string): string | null {
  const value = draft.trim();
  if (value === '') return null;
  if (value.length > CLAUDE_TOOL_MEMORY_LIMIT_MAX_LEN) {
    return `Keep this under ${CLAUDE_TOOL_MEMORY_LIMIT_MAX_LEN} characters.`;
  }
  if (TOOL_MEMORY_DISABLE_WORDS.has(value.toLowerCase())) return null;
  if (!TOOL_MEMORY_SIZE.test(value)) {
    return 'Use a size like 4G, 512m or 2GiB — or "none" to lift the limit.';
  }
  return null;
}

/** Normalizes a draft for storage: trimmed, or empty for "unset". */
export function normalizeToolMemoryLimit(draft: string): string {
  return draft.trim();
}

// -- Extended thinking -------------------------------------------------
//
// The one axis in this file that is not spawn-only: the backend applies a
// change to running headless Claude sessions. The copy below has to carry
// two facts the user cannot see anywhere else — that a fixed budget only
// binds on models with an explicit budget, and that going BACK to Claude
// Code's own choice is the one change that waits for the session to be idle.

/** Mirrors MinClaudeThinkingBudgetTokens — the Anthropic API's own floor. */
export const CLAUDE_THINKING_BUDGET_MIN = 1024;

/** Mirrors MaxClaudeThinkingBudgetTokens. */
export const CLAUDE_THINKING_BUDGET_MAX = 128000;

/**
 * What the budget field starts at when the user first picks "Fixed budget".
 * Deliberately mid-range rather than the minimum: 1024 is the floor the API
 * accepts, not a useful amount of thinking.
 */
export const CLAUDE_THINKING_BUDGET_DEFAULT = 8000;

export const CLAUDE_THINKING_MODE_OPTIONS: AxisOption<ClaudeThinkingMode>[] = [
  {
    value: '',
    label: 'Claude’s default',
    description:
      'Claude Code decides per model — adaptive thinking where the model supports it.',
  },
  {
    value: 'off',
    label: 'Off',
    description: 'No extended thinking. Claude answers without a thinking block.',
  },
  {
    value: 'budget',
    label: 'Fixed budget',
    description: 'Caps thinking at the number of tokens you set below.',
  },
];

export const CLAUDE_THINKING_DISPLAY_OPTIONS: AxisOption<ClaudeThinkingDisplay>[] = [
  {
    value: '',
    label: 'Default',
    description: 'Shows summarized thinking, the same as picking Summarized.',
  },
  {
    value: 'summarized',
    label: 'Summarized',
    description: 'Streams a summary of Claude’s thinking into the thread.',
  },
  {
    value: 'omitted',
    label: 'Hidden',
    description: 'Claude still thinks; the text just never reaches the thread.',
  },
];

/**
 * Clamps a typed budget into what the backend will store. Unlike the
 * subagent limits this does NOT fall back to zero: zero means "disabled" to
 * the CLI, so an unfinished entry has to land inside the accepted range
 * rather than silently turning thinking off.
 */
export function clampThinkingBudget(value: number): number {
  if (!Number.isFinite(value)) return CLAUDE_THINKING_BUDGET_DEFAULT;
  const floored = Math.floor(value);
  if (floored < CLAUDE_THINKING_BUDGET_MIN) return CLAUDE_THINKING_BUDGET_MIN;
  return Math.min(floored, CLAUDE_THINKING_BUDGET_MAX);
}

/**
 * Builds the value to persist for a thinking change. Written as one object
 * because it is one settings key — patching a half of it would drop the
 * other half — and it drops the budget outside budget mode for the same
 * reason the backend does: a stored budget nothing reads is not a setting.
 */
export function thinkingPatch(
  mode: ClaudeThinkingMode | '',
  budgetTokens: number,
  display: ClaudeThinkingDisplay | '',
): ClaudeThinking {
  const next: ClaudeThinking = { mode, display };
  if (mode === 'budget') next.budgetTokens = clampThinkingBudget(budgetTokens);
  return next;
}

/**
 * True when this change cannot reach a RUNNING session and has to wait for
 * it to go idle. Exactly one transition qualifies: leaving an explicit mode
 * for Claude Code's own choice. The CLI accepts `max_thinking_tokens: null`
 * and does nothing with it, so there is no reset request — only a respawn
 * without the flag.
 */
export function thinkingChangeDefersRestart(
  from: ClaudeThinkingMode | '',
  to: ClaudeThinkingMode | '',
): boolean {
  return from !== '' && to === '';
}
