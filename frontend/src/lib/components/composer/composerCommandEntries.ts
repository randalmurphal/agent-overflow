// The composer command menu's data model: what rows exist, in what sections,
// and which name wins when two sources claim one.
//
// Deliberately pure — no bindings, no stores, no DOM — so every rule here is
// unit-testable without a Svelte runtime. The reactive assembly lives in
// `composerSlash.svelte.ts`; the sources it feeds this module live in
// `stores/providerCommands.svelte.ts` and `stores/codexSkills.svelte.ts`.
//
// Three families share the menu:
//
//  - AO commands (`slashCommands.ts`, e.g. `/workflow`) — literal text the
//    backend expands at send time. They keep their any-word-position trigger.
//  - Intercepted commands — handled app-side and NEVER sent. They take
//    precedence over a provider command of the same name, which is why Claude's
//    own `/model` and `/config` are shadowed here rather than reaching the CLI.
//  - Provider commands (Claude) / skills (Codex) — the CLI executes them
//    itself. Selection inserts text; nothing is expanded on this side.

import type { SlashCommand } from '../../stores/bindings';
import { SLASH_COMMANDS, slashCommandWord, type SlashCommand as AOCommand } from './slashCommands';

export type ComposerCommandKind = 'ao' | 'intercepted' | 'provider' | 'skill';

export interface ComposerCommandEntry {
  kind: ComposerCommandKind;
  /** Name without its sigil, as the provider reports it. */
  name: string;
  /** What the row prints as the entry's identity, e.g. `/model`, `$review-code`. */
  label: string;
  /** Literal text a selection writes over the trigger range, trailing space included. */
  insertText: string;
  description?: string;
  /** Dimmed hint printed after the name, e.g. `[model]`. */
  argumentHint?: string;
  /** Rendered marked-off and refused by the insert path. */
  disabled?: boolean;
  disabledReason?: string;
  /**
   * What the filter matches against, when the row is findable by more than its
   * name — a commit row is found by its subject line, not by its sha. Defaults
   * to `name`.
   */
  searchText?: string;
}

export interface ComposerCommandSection {
  id: string;
  header: string;
  entries: ComposerCommandEntry[];
}

// ---------------------------------------------------------------------------
// Intercepted commands.

export interface InterceptedCommandDef {
  name: string;
  description: string;
  argumentHint?: string;
  /**
   * Providers that expose the command. Omitted means every provider — the
   * reroutes (`/model`, `/effort`, …) are AO surfaces, not provider features.
   */
  providers?: readonly string[];
}

/**
 * Commands AO consumes at send time. They shadow a provider command of the
 * same name on purpose: Claude ships its own `/model` and `/config`, and a
 * user in AO means AO's picker, not the CLI's.
 */
export const INTERCEPTED_COMMANDS: readonly InterceptedCommandDef[] = [
  { name: 'model', description: 'Switch this thread’s model', argumentHint: '[model]' },
  { name: 'effort', description: 'Set reasoning effort', argumentHint: '[tier]' },
  { name: 'fast', description: 'Toggle fast mode' },
  { name: 'config', description: 'Open settings' },
  { name: 'clear', description: 'Start a new thread in this project' },
  { name: 'rename', description: 'Rename this thread', argumentHint: '[title]' },
  {
    name: 'compact',
    description: 'Compact this thread’s context now',
    providers: ['codex'],
  },
  {
    name: 'review',
    description: 'Run Codex’s code review on this workspace',
    argumentHint: '[target]',
    providers: ['codex'],
  },
];

/** The intercepted commands a thread on `provider` exposes, in registry order. */
export function interceptedCommandsFor(
  provider: string | null | undefined,
): InterceptedCommandDef[] {
  const id = (provider ?? '').trim();
  return INTERCEPTED_COMMANDS.filter(
    (command) => !command.providers || (id !== '' && command.providers.includes(id)),
  );
}

/** Name set of `interceptedCommandsFor`, for precedence and send-time parsing. */
export function interceptedCommandNames(
  provider: string | null | undefined,
): Set<string> {
  return new Set(interceptedCommandsFor(provider).map((command) => command.name));
}

// ---------------------------------------------------------------------------
// Claude's two sources, unioned.

/**
 * Resolve a Claude thread's command list from its live session frame and the
 * probe-seeded list.
 *
 * The rule, in one sentence: once a session frame has arrived its NAME SET is
 * authoritative, and the probe list only enriches the names it already
 * contains. That is the only ordering that stays honest in both directions —
 * `system/init` reports names with no descriptions and is the only surface
 * listing MCP prompt commands, while the probe answers for a probe identity
 * that may not match this thread's session.
 *
 * `sessionCommands === null` means no frame has arrived (unknown), so the
 * probe list stands alone. `probeCommands === null` means the probe has not
 * answered; a caller must not pass `[]` for that, since an empty array is a
 * real "this identity has none".
 */
/**
 * Fold the filesystem-enumerated Claude skills into the probe list, forming
 * the full "static" (no-session-needed) command source.
 *
 * The probe's `--safe-mode` initialize deliberately reports no skills, so
 * the two lists are disjoint in practice; when they do collide, the probe
 * entry wins — it is the CLI's own answer for the name. Null-ness carries
 * meaning: null probe + no skills stays null (nothing has answered), while
 * skills alone form a real list so a cold menu can show them before the
 * probe lands.
 */
export function mergeStaticClaudeCommands(
  probeCommands: readonly SlashCommand[] | null,
  skills: readonly { name: string; description?: string }[],
): SlashCommand[] | null {
  if (skills.length === 0) return probeCommands === null ? null : [...probeCommands];
  const out: SlashCommand[] = [...(probeCommands ?? [])];
  const taken = new Set(out.map((command) => command.name));
  for (const skill of skills) {
    if (taken.has(skill.name)) continue;
    taken.add(skill.name);
    out.push({ name: skill.name, description: skill.description ?? '', argumentHint: '' });
  }
  return out;
}

export function unionProviderCommands(
  sessionCommands: readonly SlashCommand[] | null,
  probeCommands: readonly SlashCommand[] | null,
): SlashCommand[] {
  if (sessionCommands === null) return [...(probeCommands ?? [])];
  const byName = new Map<string, SlashCommand>();
  for (const command of probeCommands ?? []) byName.set(command.name, command);
  return sessionCommands.map((command) => {
    const enrichment = byName.get(command.name);
    if (!enrichment) return command;
    return {
      ...command,
      description: command.description || enrichment.description,
      argumentHint: command.argumentHint || enrichment.argumentHint,
    };
  });
}

// ---------------------------------------------------------------------------
// Entry construction.

export function aoCommandEntry(command: AOCommand): ComposerCommandEntry {
  return {
    kind: 'ao',
    name: command.name,
    label: slashCommandWord(command),
    // Trailing space: the word is complete, the caret belongs at the start of
    // whatever the user types next, and the space is what closes the trigger.
    insertText: `${slashCommandWord(command)} `,
    description: command.description,
  };
}

export function interceptedCommandEntry(command: InterceptedCommandDef): ComposerCommandEntry {
  return {
    kind: 'intercepted',
    name: command.name,
    label: `/${command.name}`,
    insertText: `/${command.name} `,
    description: command.description,
    argumentHint: command.argumentHint,
  };
}

/**
 * Claude commands hidden from the MENU, never from send classification —
 * a hand-typed `/color` still sends as a provider command. These drive the
 * CLI's own terminal UI, so in AO their effect is invisible: `/color`
 * (theme), `/agents` (interactive agent editor), `/extra-usage` (display
 * toggle). The dunder prefix covers the CLI's internal entries.
 */
const HIDDEN_PROVIDER_COMMANDS = new Set(['color', 'agents', 'extra-usage']);

export function isHiddenProviderCommand(name: string): boolean {
  return name.startsWith('__') || HIDDEN_PROVIDER_COMMANDS.has(name);
}

export function providerCommandEntry(command: SlashCommand): ComposerCommandEntry {
  return {
    kind: 'provider',
    name: command.name,
    label: `/${command.name}`,
    // Insert-as-text execution: the CLI owns expansion, so the composer's job
    // ends at putting the word in the draft.
    insertText: `/${command.name} `,
    description: command.description,
    argumentHint: command.argumentHint,
  };
}

/** One Codex skill. `$name` is Codex's own invocation syntax, not ours. */
export function codexSkillEntry(skill: {
  name: string;
  description?: string;
  shortDescription?: string;
  displayName?: string;
  enabled?: boolean;
}): ComposerCommandEntry {
  const enabled = skill.enabled !== false;
  return {
    kind: 'skill',
    name: skill.name,
    label: `$${skill.name}`,
    insertText: `$${skill.name} `,
    description: skill.shortDescription || skill.description || undefined,
    disabled: !enabled,
    disabledReason: enabled ? undefined : 'Disabled in Codex config',
  };
}

export interface CommandMenuSources {
  provider: string | null | undefined;
  /**
   * Whether the `/` sits at the very start of the draft.
   *
   * AO's own commands are offered at any word position because the backend
   * expands them there. Everything else — intercepted commands, provider
   * commands, skills — is offered ONLY at position 0, because that is the only
   * place a send would treat the word as a command.
   */
  atStart: boolean;
  /** Live session frame's commands, or null when no frame has arrived. */
  sessionCommands: readonly SlashCommand[] | null;
  /** Probe-seeded commands, or null when the probe has not answered. */
  probeCommands: readonly SlashCommand[] | null;
  /** Codex skills for the thread's workspace; empty when not loaded. */
  skills: readonly {
    name: string;
    description?: string;
    shortDescription?: string;
    displayName?: string;
    enabled?: boolean;
  }[];
  /**
   * Claude skills enumerated from the filesystem for the thread's
   * workspace; empty when not loaded. Merged into the static command
   * source via `mergeStaticClaudeCommands` — a live session's name set
   * still wins once a frame exists.
   */
  claudeSkills: readonly { name: string; description?: string }[];
}

/**
 * Every section the menu can show, before filtering.
 *
 * Provider/skill names colliding with an intercepted command are dropped, not
 * rendered twice: the intercepted one is what a send would run, and a menu
 * offering both would be offering a choice that does not exist.
 */
export function buildCommandSections(sources: CommandMenuSources): ComposerCommandSection[] {
  const intercepted = sources.atStart ? interceptedCommandsFor(sources.provider) : [];
  const shadowed = new Set(interceptedCommandsFor(sources.provider).map((c) => c.name));
  const sections: ComposerCommandSection[] = [];

  const ao = SLASH_COMMANDS.map(aoCommandEntry);
  const appEntries = [...ao, ...intercepted.map(interceptedCommandEntry)];
  if (appEntries.length > 0) {
    sections.push({ id: 'ao', header: 'Agent Overflow', entries: appEntries });
  }
  if (!sources.atStart) return sections;

  const providerId = (sources.provider ?? '').trim();
  if (providerId === 'codex') {
    const skills = sources.skills
      .filter((skill) => !shadowed.has(skill.name))
      .map(codexSkillEntry);
    if (skills.length > 0) {
      sections.push({ id: 'skills', header: 'Codex skills', entries: skills });
    }
    return sections;
  }

  const staticCommands = mergeStaticClaudeCommands(sources.probeCommands, sources.claudeSkills);
  const commands = unionProviderCommands(sources.sessionCommands, staticCommands)
    .filter((command) => !shadowed.has(command.name) && !isHiddenProviderCommand(command.name))
    .map(providerCommandEntry);
  if (commands.length > 0) {
    sections.push({ id: 'provider', header: 'Provider commands', entries: commands });
  }
  return sections;
}

/**
 * Narrow `sections` to the rows matching `query`, dropping emptied sections.
 *
 * Substring rather than prefix because provider names are not always typed
 * from the front (`mcp__linear__issue`), with prefix matches ordered first so
 * the obvious completion still lands on row one. An empty query matches
 * everything and preserves source order.
 */
export function filterCommandSections(
  sections: readonly ComposerCommandSection[],
  query: string,
): ComposerCommandSection[] {
  const needle = query.trim().toLowerCase();
  if (needle === '') return sections.map((section) => ({ ...section, entries: [...section.entries] }));
  const filtered: ComposerCommandSection[] = [];
  for (const section of sections) {
    const prefix: ComposerCommandEntry[] = [];
    const inner: ComposerCommandEntry[] = [];
    for (const entry of section.entries) {
      const haystack = (entry.searchText ?? entry.name).toLowerCase();
      if (haystack.startsWith(needle)) prefix.push(entry);
      else if (haystack.includes(needle)) inner.push(entry);
    }
    if (prefix.length + inner.length === 0) continue;
    filtered.push({ ...section, entries: [...prefix, ...inner] });
  }
  return filtered;
}

/** Flat row order, which is what the keyboard's active index walks. */
export function flattenSections(
  sections: readonly ComposerCommandSection[],
): ComposerCommandEntry[] {
  return sections.flatMap((section) => section.entries);
}
