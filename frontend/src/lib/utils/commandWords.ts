// The one frontend rule for where a composer slash command sits in text (D31).
//
// Two surfaces need it and neither may drift from the other: the composer
// colours the words in the draft it is about to send, and the transcript
// colours the same words in a row that already sent. Both go through
// `commandWordRanges`.
//
// The backend's copy of the rule is `internal/usermessage/command.go`
// (`CommandWords`), and it is authoritative — expansion happens there. The
// tables in `slashCommands.test.ts` and `command_test.go` mirror each other
// case for case; that is the parity check.

export interface CommandWordRange {
  /** The command name, without its leading slash. */
  name: string;
  /** Inclusive index of the `/`. */
  start: number;
  /** Exclusive end of the word. */
  end: number;
}

// Word separators: the Unicode White_Space property, which is exactly what Go's
// `unicode.IsSpace` tests. JavaScript's `\s` is that set minus U+0085 plus
// U+FEFF, so both corrections are spelled out rather than left to diverge.
const SEPARATOR = /[^\S\uFEFF]|\u0085/;

const COMMAND_NAME = /^[a-z][a-z0-9-]*$/;

/** Whether one character ends a word. */
export function isWordSeparator(char: string): boolean {
  return SEPARATOR.test(char);
}

/** Whether `text` contains any word separator. */
export function hasWordSeparator(text: string): boolean {
  return SEPARATOR.test(text);
}

/**
 * Every command-shaped word in `value`, in order of appearance.
 *
 * A word is a maximal run of non-whitespace, so a candidate `/` must sit at the
 * start of the value or immediately after whitespace, and its name runs to the
 * next whitespace character (or the end). The name must be lowercase
 * `[a-z][a-z0-9-]*`, which is what keeps `/tmp/scratch`, `/Users`, a bare `/`,
 * and a trailing-comma `/workflow,` out.
 *
 * Whether a name is REGISTERED is the caller's question — `slashCommands.ts`
 * asks it of the live registry, the transcript asks it of the marker its row
 * was stored with.
 */
export function commandWordRanges(value: string): CommandWordRange[] {
  const ranges: CommandWordRange[] = [];
  let i = 0;
  while (i < value.length) {
    if (SEPARATOR.test(value[i])) {
      i += 1;
      continue;
    }
    let end = i;
    while (end < value.length && !SEPARATOR.test(value[end])) end += 1;
    if (value[i] === '/') {
      const name = value.slice(i + 1, end);
      if (COMMAND_NAME.test(name)) ranges.push({ name, start: i, end });
    }
    i = end;
  }
  return ranges;
}

export interface CommandSegment {
  text: string;
  /** True for the command words themselves — the parts that render accented. */
  command: boolean;
}

/**
 * Split `value` into alternating plain / command segments for rendering.
 *
 * Ranges must be non-overlapping and sorted, which is what `commandWordRanges`
 * returns; callers filter that list, they never build one. Empty plain
 * segments are dropped so a renderer never emits a pointless node, but the
 * command segments are kept in order even when adjacent.
 */
export function commandSegments(
  value: string,
  ranges: readonly Pick<CommandWordRange, 'start' | 'end'>[],
): CommandSegment[] {
  const segments: CommandSegment[] = [];
  let cursor = 0;
  for (const range of ranges) {
    if (range.start > cursor) segments.push({ text: value.slice(cursor, range.start), command: false });
    segments.push({ text: value.slice(range.start, range.end), command: true });
    cursor = range.end;
  }
  if (cursor < value.length) segments.push({ text: value.slice(cursor), command: false });
  return segments;
}
