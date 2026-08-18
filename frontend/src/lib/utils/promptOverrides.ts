import type { PromptOverride, Settings } from '../types/settings';

// Prompt-override / disabled-tool lookups for the Settings → Prompts & Tools
// section. Mirrors internal/settings exactly: `claude` and `claude-tui` share
// the Claude keys (same binary — the interactive TUI honors
// `--system-prompt-file` and `--disallowedTools` exactly as headless does,
// spike-verified on 2.1.234, and `claudetui/launch.go` passes both), `codex`
// has its own, and everything else has none. Same routing shape as
// hiddenModels.ts. Everything here is pure so the section's components stay
// render-only and the list edits are unit-testable without a Svelte runtime.

export type PromptOverridesSettingsKey =
  | 'claudePromptOverrides'
  | 'codexPromptOverrides';

export type DisabledToolsSettingsKey =
  | 'claudeDisabledTools'
  | 'codexDisabledTools';

/**
 * Settings key holding a provider's prompt-override list, or null for
 * providers without one. Single owner of the provider→key routing for both
 * the read and the write path.
 */
export function promptOverridesSettingsKey(
  provider: string,
): PromptOverridesSettingsKey | null {
  switch (provider) {
    case 'claude':
    case 'claude-tui':
      return 'claudePromptOverrides';
    case 'codex':
      return 'codexPromptOverrides';
    default:
      return null;
  }
}

/** Settings key holding a provider's disabled-tool list, or null. */
export function disabledToolsSettingsKey(
  provider: string,
): DisabledToolsSettingsKey | null {
  switch (provider) {
    case 'claude':
    case 'claude-tui':
      return 'claudeDisabledTools';
    case 'codex':
      return 'codexDisabledTools';
    default:
      return null;
  }
}

/** A provider's prompt overrides in precedence order ([] when unset). */
export function promptOverridesFor(
  settings: Settings,
  provider: string,
): PromptOverride[] {
  const key = promptOverridesSettingsKey(provider);
  return key ? (settings[key] ?? []) : [];
}

/** A provider's disabled tools ([] when unset). */
export function disabledToolsFor(
  settings: Settings,
  provider: string,
): string[] {
  const key = disabledToolsSettingsKey(provider);
  return key ? (settings[key] ?? []) : [];
}

export interface PromptPlaceholder {
  token: string;
  meaning: string;
}

/**
 * Placeholders the backend substitutes at spawn. The list is closed —
 * anything else in `{{…}}` is left literal — so the legend is the complete
 * reference, not a sample.
 *
 * Hand-mirrored from the `Token*` constants in
 * `internal/promptoverride/promptoverride.go`, which is the authority: a
 * token added there and not here is simply unadvertised, but a token here
 * and not there renders literally into the prompt. A Go test parses this
 * array to pin the two sets together, so keep the `token: '{{X}}'`
 * literals exactly as written.
 */
export const PROMPT_PLACEHOLDERS: readonly PromptPlaceholder[] = [
  { token: '{{WORKDIR}}', meaning: "The session's working directory." },
  { token: '{{IS_GIT_REPO}}', meaning: 'Whether that directory is a git repository.' },
  {
    token: '{{GIT_BLOCK}}',
    meaning: 'Branch, status and recent commits. Empty outside a repository.',
  },
  { token: '{{PLATFORM}}', meaning: 'Host operating system.' },
  { token: '{{OS_VERSION}}', meaning: 'Host operating system version.' },
  { token: '{{MODEL_NAME}}', meaning: "The session model's display name." },
  { token: '{{MODEL_ID}}', meaning: "The session model's wire id." },
  {
    token: '{{MEMORY_DIR}}',
    meaning: "Claude's per-project memory directory, created when this is used.",
  },
] as const;

// {{MEMORY_DIR}} resolves inside the Claude home, so it means nothing in a
// Codex prompt: the renderer would substitute a path Codex has no notion of.
// The legend advertises per provider rather than showing every token
// everywhere.
const CLAUDE_ONLY_PLACEHOLDERS: ReadonlySet<string> = new Set(['{{MEMORY_DIR}}']);

export function placeholdersFor(provider: string): PromptPlaceholder[] {
  const key = promptOverridesSettingsKey(provider);
  if (key === 'claudePromptOverrides') return [...PROMPT_PLACEHOLDERS];
  return PROMPT_PLACEHOLDERS.filter((p) => !CLAUDE_ONLY_PLACEHOLDERS.has(p.token));
}

export interface CodexToolToggle {
  id: string;
  label: string;
  description: string;
}

/**
 * The curated Codex tool toggles. Codex has no flat disallow list, so each id
 * stands for a set of per-thread config keys the backend writes — the ids are
 * a locked contract with it, not display strings. Shell / unified-exec and
 * apply_patch are deliberately absent (session-lobotomizing and
 * catalog-driven respectively).
 *
 * Hand-mirrored from `disabledToolConfigKeys` in
 * `internal/provider/codex/disabled_tools.go`, which is the authority: an
 * id it does not know is skipped with a log line at spawn, so a typo here
 * renders a switch that silently does nothing. A Go test parses this array
 * to pin the two id sets together, so keep the `id: 'x'` literals exactly
 * as written.
 */
export const CODEX_TOOL_TOGGLES: readonly CodexToolToggle[] = [
  {
    id: 'web_search',
    label: 'Web search',
    description: 'Lets the model search the web during a turn.',
  },
  {
    id: 'update_plan',
    label: 'Plan updates',
    description: "Codex's built-in plan / TODO tool.",
  },
  {
    id: 'view_image',
    label: 'View image',
    description: 'Lets the model read a local image file.',
  },
  {
    id: 'request_user_input',
    label: 'Ask the user',
    description: 'Experimental mid-turn question tool.',
  },
  {
    id: 'collab_agents',
    label: 'Collab / multi-agent tools',
    description: 'Spawning, messaging and closing child agents.',
  },
  {
    id: 'image_generation',
    label: 'Image generation',
    description: 'Lets the model generate images.',
  },
  {
    id: 'tool_suggest',
    label: 'Tool suggestions',
    description: "Codex's suggest-a-tool helper.",
  },
] as const;

const CODEX_TOOL_TOGGLE_IDS: ReadonlySet<string> = new Set(
  CODEX_TOOL_TOGGLES.map((toggle) => toggle.id),
);

/**
 * Stored ids the curated set above does not cover, in stored order. A
 * hand-edited settings file or a list written by a newer AO can hold one;
 * the backend skips it with a log line, so it disables nothing and the
 * switches alone would render it as neither on nor off — invisible, and
 * therefore unremovable through this UI.
 */
export function unknownCodexToggleIds(disabled: readonly string[]): string[] {
  return disabled.filter((id) => !CODEX_TOOL_TOGGLE_IDS.has(id));
}

/**
 * Common Claude built-ins offered as one-click additions. Claude takes raw
 * names on `--disallowedTools`, so this is a convenience list, never a
 * closed set — the free-form field is the real interface.
 */
export const CLAUDE_TOOL_SUGGESTIONS: readonly string[] = [
  'Workflow',
  'EnterPlanMode',
  'ExitPlanMode',
  'Agent',
  'WebSearch',
  'WebFetch',
  'NotebookEdit',
  'TaskCreate',
  'TaskUpdate',
  'TaskGet',
  'TaskList',
] as const;

/** A fresh, disabled, empty entry — appended by the Add control. */
export function newPromptOverride(): PromptOverride {
  return { enabled: false, models: [], prompt: '' };
}

/** The list with `patch` applied to entry `index` (no-op for a bad index). */
export function withEntryPatch(
  list: readonly PromptOverride[],
  index: number,
  patch: Partial<PromptOverride>,
): PromptOverride[] {
  if (index < 0 || index >= list.length) return [...list];
  return list.map((entry, i) => (i === index ? { ...entry, ...patch } : { ...entry }));
}

/** The list without entry `index`. */
export function withEntryRemoved(
  list: readonly PromptOverride[],
  index: number,
): PromptOverride[] {
  return list.filter((_, i) => i !== index);
}

/** The list with a fresh entry appended. */
export function withEntryAdded(list: readonly PromptOverride[]): PromptOverride[] {
  return [...list.map((entry) => ({ ...entry })), newPromptOverride()];
}

/**
 * `models` with `slug` toggled in or out, preserving the order slugs were
 * selected in so a re-render doesn't reshuffle the chips.
 */
export function toggleModelSelection(
  models: readonly string[],
  slug: string,
): string[] {
  return models.includes(slug)
    ? models.filter((m) => m !== slug)
    : [...models, slug];
}

// A blank prompt is not an override: `internal/promptoverride.Match` walks
// past an entry whose Prompt is empty after TrimSpace exactly as it walks
// past a disabled one. Two surfaces read that rule — the shadow walk below
// and the per-entry warnings — so it lives here once rather than as two
// `.trim() === ''` checks that can drift apart from Go independently.
function hasPrompt(entry: PromptOverride): boolean {
  return entry.prompt.trim() !== '';
}

/**
 * Why an enabled entry can never replace a prompt. Both flags stay false for
 * a DISABLED entry: its switch already says it is off, and repeating that as
 * a warning would make a deliberately parked entry look broken.
 */
export interface EntryInertness {
  /** Match skips the entry outright, so nothing about it matters. */
  noPrompt: boolean;
  /** Match reaches it, but no session model can be in an empty list. */
  noModels: boolean;
}

export function entryInertness(entry: PromptOverride): EntryInertness {
  return {
    noPrompt: entry.enabled && !hasPrompt(entry),
    noModels: entry.enabled && entry.models.length === 0,
  };
}

/**
 * Models of entry `index` that an EARLIER entry the backend would actually
 * match already claims. The backend takes the first match, so these slugs
 * never reach this entry — worth saying out loud rather than letting the
 * user wonder why a prompt is ignored. An earlier entry Match skips claims
 * nothing, so warning about it would send the user chasing a shadow that
 * isn't there.
 */
export function shadowedModels(
  list: readonly PromptOverride[],
  index: number,
): string[] {
  const entry = list[index];
  if (!entry || !entry.enabled) return [];
  const claimed = new Set<string>();
  for (let i = 0; i < index; i++) {
    const earlier = list[i];
    if (!earlier.enabled || !hasPrompt(earlier)) continue;
    for (const slug of earlier.models) claimed.add(slug);
  }
  return entry.models.filter((slug) => claimed.has(slug));
}

/**
 * A tool name as it will be persisted: trimmed. Names go to the CLI verbatim
 * otherwise — case included, since Claude's tool names are case-sensitive.
 */
export function normalizeToolName(raw: string): string {
  return raw.trim();
}

/** Byte cap on one entry, mirroring settings.MaxDisabledToolLen. */
export const MAX_DISABLED_TOOL_BYTES = 128;

const TOOL_NAME_ENCODER = new TextEncoder();

// `\p{White_Space}` is Unicode's White_Space property, which is exactly
// what Go's `unicode.IsSpace` tests, so this is a mirror rather than an
// approximation. JS's `\s` is not: it omits U+0085 (NEL), which Go counts
// as whitespace — and a character we allow that Go rejects fails the whole
// save, which is the failure this validator exists to prevent.
const TOOL_NAME_WHITESPACE = /\p{White_Space}/u;

/**
 * Why `internal/settings.validateDisabledTool` would refuse this name, or
 * null when it would take it. A blank name is not an error here — there is
 * nothing to say about a name that has not been typed — so callers gate on
 * a non-empty normalized draft separately.
 *
 * This exists because UpdateSettings rejects the WHOLE patch on one bad
 * entry: an unmirrored rule reaches the user as a generic "Failed to save
 * setting" toast plus a silent rollback of the list they were editing, with
 * nothing naming the entry that did it.
 */
export function disabledToolNameError(raw: string): string | null {
  const tool = normalizeToolName(raw);
  if (tool === '') return null;
  if (tool.startsWith('-')) return `Tool names can't start with "-".`;
  if (TOOL_NAME_WHITESPACE.test(tool)) return "Tool names can't contain whitespace.";
  if (TOOL_NAME_ENCODER.encode(tool).length > MAX_DISABLED_TOOL_BYTES) {
    return `Tool names are limited to ${MAX_DISABLED_TOOL_BYTES} bytes.`;
  }
  return null;
}

/** The list with `name` added, or unchanged when blank or already present. */
export function withToolAdded(
  list: readonly string[],
  name: string,
): string[] {
  const normalized = normalizeToolName(name);
  if (normalized === '' || list.includes(normalized)) return [...list];
  return [...list, normalized];
}

/** The list with `name` removed. */
export function withToolRemoved(
  list: readonly string[],
  name: string,
): string[] {
  return list.filter((tool) => tool !== name);
}

/**
 * Insert `token` into `text` at `[start, end)`, replacing any selection.
 * Returns the new text and where the caret belongs after it. Pure so the
 * legend's insert behaviour is testable without a DOM selection.
 */
export function insertAtSelection(
  text: string,
  start: number,
  end: number,
  token: string,
): { text: string; caret: number } {
  const from = Math.max(0, Math.min(start, text.length));
  const to = Math.max(from, Math.min(end, text.length));
  return {
    text: text.slice(0, from) + token + text.slice(to),
    caret: from + token.length,
  };
}
