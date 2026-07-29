// Composer slash commands (D31): the registry the completion menu lists and
// the rules that decide when a draft is invoking one.
//
// Nothing here expands anything. A command is literal text in the draft —
// `/workflow` and whatever the user types after it — and the backend resolves
// the expansion at send time (`app_composer_commands.go`). This module exists
// so the composer can offer the words and colour the ones that match, and it
// is deliberately pure: no bindings, no stores, no DOM.
//
// The backend's parse of the same rule is `internal/usermessage/command.go`
// and its table is `app_composer_commands.go`. The two sides are parallel by
// hand rather than generated; the backend is authoritative, so a name added
// here without one there would colour a word that expands to nothing.
//
// Where a command may SIT in the text is not decided here — `utils/commandWords`
// owns that, because the transcript needs the same answer.

import {
  commandWordRanges,
  hasWordSeparator,
  isWordSeparator,
  type CommandWordRange,
} from '../../utils/commandWords';

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

export interface SlashCommandMatch extends CommandWordRange {
  command: SlashCommand;
}

/**
 * Every registered command word in `value`, in order of appearance.
 *
 * A command counts at ANY word position (D31, amended): the front of the
 * draft, mid-sentence, after a newline. Word shape comes from
 * `commandWordRanges`, mirrored exactly by the Go parser; this adds the one
 * question the registry can answer, which is whether anything claims the name.
 *
 * The list is what the composer paints. What it SENDS is decided on the other
 * side of the wire — the backend expands the first registered word once, no
 * matter how many times the draft names it.
 */
export function slashCommandMatches(value: string): SlashCommandMatch[] {
  const matches: SlashCommandMatch[] = [];
  for (const range of commandWordRanges(value)) {
    const command = SLASH_COMMANDS.find((entry) => entry.name === range.name);
    if (command) matches.push({ ...range, command });
  }
  return matches;
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
  /** Inclusive index of the `/`. Carried so the caller replaces a range. */
  start: number;
  /** Exclusive end of the replacement range: the caret. */
  end: number;
}

/**
 * Detect an open slash-command completion at the caret, or null.
 *
 * Word-boundary rules, deliberately the same shape as `detectMentionTrigger`:
 * the `/` must sit at the start of the value or immediately after a separator,
 * and the caret must still be inside that word — a space between the `/` and
 * the caret closes the menu. At least one command must match, so typing past a
 * full name (`/workflowish`) simply leaves the text alone, and a path segment
 * (`src/lib`) never opens the menu because its `/` follows a letter.
 *
 * "Separator" is `commandWordRanges`' separator, not a second definition of
 * one: the menu must never offer a completion for a word the matcher would
 * then refuse to colour.
 */
export function detectSlashTrigger(value: string, caret: number): SlashTrigger | null {
  if (caret < 1 || caret > value.length) return null;
  const before = value.slice(0, caret);
  const slash = before.lastIndexOf('/');
  if (slash < 0) return null;
  if (slash > 0 && !isWordSeparator(before[slash - 1])) return null;
  const query = before.slice(slash + 1);
  if (hasWordSeparator(query)) return null;
  const results = matchSlashCommands(query);
  if (results.length === 0) return null;
  return { query, results, start: slash, end: caret };
}
