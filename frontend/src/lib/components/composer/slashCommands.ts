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

/**
 * Where a completion menu opens over this text is `composerCommandTrigger.ts`,
 * and which rows it offers is `composerCommandEntries.ts`. Neither lives here:
 * the menu now lists provider-executed commands and Codex skills alongside
 * these, and this module stays the AO registry plus the one question it can
 * answer about a draft — which words the backend will expand.
 */
