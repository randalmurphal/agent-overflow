// Composer slash commands (D31): the registry the completion menu lists and
// the rules that decide when a draft is invoking one.
//
// Nothing here expands anything. A command is literal text in the draft —
// `/workflow` and whatever the user types after it — and the backend resolves
// the expansion at send time (`app_composer_commands.go`). This module exists
// so the composer can offer the words and colour one when it matches, and it
// is deliberately pure: no bindings, no stores, no DOM.
//
// The backend's parse of the same rule is `internal/usermessage/command.go`
// and its table is `app_composer_commands.go`. The two sides are parallel by
// hand rather than generated; the backend is authoritative, so a name added
// here without one there would colour a word that expands to nothing.

export interface SlashCommand {
  /** The word the user types, without its leading slash. */
  name: string;
  /** One line, shown in the menu. Says what the agent gains. */
  description: string;
}

export const SLASH_COMMANDS: readonly SlashCommand[] = [
  {
    name: 'workflow',
    description: 'Give the agent this project’s workflows and the agent-overflow CLI',
  },
];

/** The literal text a command occupies in the draft, e.g. `/workflow`. */
export function slashCommandWord(command: SlashCommand): string {
  return `/${command.name}`;
}

/**
 * The leading command word of a draft, when it invokes a registered one.
 *
 * Rules, mirrored exactly by the Go parser:
 * - the very first character must be `/` — a draft starting with whitespace
 *   is not an invocation;
 * - the word runs to the first whitespace character (or the end);
 * - the word must match a registered command EXACTLY, so `/workflows`,
 *   `/Workflow`, and `/tmp/log` are ordinary text.
 */
export function leadingSlashCommand(value: string): SlashCommand | null {
  if (!value.startsWith('/')) return null;
  const word = value.slice(1).split(/\s/, 1)[0] ?? '';
  return SLASH_COMMANDS.find((command) => command.name === word) ?? null;
}

/** Registered commands whose name starts with `query`, in registry order. */
export function matchSlashCommands(query: string): SlashCommand[] {
  return SLASH_COMMANDS.filter((command) => command.name.startsWith(query));
}

export interface SlashTrigger {
  /** What the user has typed after the slash, up to the caret. */
  query: string;
  /** Commands matching `query`, in registry order. Never empty. */
  results: SlashCommand[];
  /** Inclusive index of the `/` — always 0; carried so the caller replaces a range. */
  start: number;
  /** Exclusive end of the replacement range: the caret. */
  end: number;
}

/**
 * Detect an open slash-command completion at the caret, or null.
 *
 * The trigger only ever opens on the FIRST word of the draft: `/` anywhere
 * else is a path, a fraction, or prose, and hijacking it would make the
 * composer unusable for the text people actually type. The caret must still
 * be inside that first word — moving past the space closes the menu — and at
 * least one command must match, so typing past a full name (`/workflowish`)
 * simply leaves the text alone.
 */
export function detectSlashTrigger(value: string, caret: number): SlashTrigger | null {
  if (caret < 1 || caret > value.length) return null;
  if (!value.startsWith('/')) return null;
  const before = value.slice(0, caret);
  if (/\s/.test(before)) return null;
  const query = before.slice(1);
  const results = matchSlashCommands(query);
  if (results.length === 0) return null;
  return { query, results, start: 0, end: caret };
}
