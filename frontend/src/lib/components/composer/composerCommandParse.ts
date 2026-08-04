// What a draft's leading word means at send time.
//
// Three questions, one parse: is the message an intercepted command AO must
// consume, is it a provider command the CLI should execute, or is it prose?
// All three are decided from the message text alone — there is no hidden
// per-send flag set by the menu, so a hand-typed `/usage` behaves exactly like
// a menu-selected one and a menu selection the user then edited stops
// behaving like a command.
//
// The word rule is a deliberate mirror of the CLI's, in
// `internal/provider/claude/slash_guard.go`: no leading trim (the CLI tests the
// raw string, so " /usage" is already prose to it), a leading `/`, then one or
// more name bytes, then whitespace or end of message. An interior `/` — the
// thing that keeps `/etc/hosts is world-readable` out — disqualifies the word.
//
// Deliberately NOT `utils/commandWords`: that rule is the AO command grammar
// (lowercase `[a-z][a-z0-9-]*`, any word position) and it cannot name an MCP
// prompt command like `mcp__linear__create_issue`.

/** Letters, digits, `_`, `-`, `:` — the same set the CLI's router accepts. */
const NAME_CHAR = /[A-Za-z0-9_:-]/;

const ASCII_SPACE = /[ \t\n\r\v\f]/;

/**
 * The command-shaped word at position 0 of `content`, without its slash, or
 * null when the message does not open with one.
 */
export function leadingCommandName(content: string): string | null {
  if (content.length === 0 || content[0] !== '/') return null;
  let i = 1;
  while (i < content.length && NAME_CHAR.test(content[i])) i += 1;
  if (i === 1) return null;
  // The first byte that cannot be part of a name has to be a word break;
  // anything else (a slash, a dot, a comma) means this was never a name.
  if (i < content.length && !ASCII_SPACE.test(content[i])) return null;
  return content.slice(1, i);
}

/** Everything after the leading command word, trimmed. Empty for a bare call. */
export function leadingCommandArgument(content: string): string {
  const name = leadingCommandName(content);
  if (name === null) return '';
  return content.slice(name.length + 1).trim();
}

export interface InterceptedInvocation {
  name: string;
  /** Text after the command word, trimmed. Empty means "bare". */
  arg: string;
}

/**
 * The intercepted command this message invokes, or null.
 *
 * Interception is name-based and position-0 only, so `/model` inside a
 * sentence is prose and reaches the provider (neutralised by the CLI guard)
 * exactly as it did before.
 */
export function parseInterceptedCommand(
  content: string,
  interceptedNames: ReadonlySet<string>,
): InterceptedInvocation | null {
  const name = leadingCommandName(content);
  if (name === null || !interceptedNames.has(name)) return null;
  return { name, arg: leadingCommandArgument(content) };
}

/**
 * The range a leading intercepted command occupies, for the accent overlay, or
 * null.
 *
 * Position 0 only, deliberately: an intercepted command mid-sentence is prose
 * and WILL be sent, so painting it there would promise a reroute that never
 * happens. Shape matches `commandWordRanges`' so the two lists concatenate.
 */
export function interceptedCommandRange(
  content: string,
  interceptedNames: ReadonlySet<string>,
): { name: string; start: number; end: number } | null {
  const name = leadingCommandName(content);
  if (name === null || !interceptedNames.has(name)) return null;
  return { name, start: 0, end: name.length + 1 };
}

/**
 * Whether this message should be sent with `providerCommand: true`.
 *
 * True only when the first word names a command the thread's provider has
 * actually reported. An unknown `/word` stays guarded — the CLI would route it
 * to its own router, answer "Unknown command" with `num_turns: 0`, and discard
 * everything the user wrote after it.
 */
export function isProviderCommandMessage(
  content: string,
  knownProviderNames: ReadonlySet<string>,
): boolean {
  const name = leadingCommandName(content);
  return name !== null && knownProviderNames.has(name);
}
