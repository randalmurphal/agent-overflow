export interface MentionTrigger {
  query: string;
  start: number; // inclusive, index of "@" in the textarea value
  end: number;   // exclusive, caret position
}

/**
 * Detects an active "@query" mention in the textarea up to the caret.
 * Returns null when the caret is not inside a mention context.
 *
 * Rules (matches forge's behaviour):
 * - The "@" must be at start-of-string or preceded by whitespace.
 * - The query is everything after "@" up to caret, excluding whitespace.
 * - An @ with no trailing query still counts as open (to show the popover).
 */
export function detectMentionTrigger(value: string, caret: number): MentionTrigger | null {
  if (caret < 0 || caret > value.length) return null;
  const before = value.slice(0, caret);
  const atIndex = before.lastIndexOf('@');
  if (atIndex < 0) return null;

  // Must be start-of-string or after whitespace.
  if (atIndex > 0) {
    const prev = before[atIndex - 1];
    if (prev && !/\s/.test(prev)) return null;
  }

  const query = before.slice(atIndex + 1);
  // Query can't include whitespace — a space closes the mention.
  if (/\s/.test(query)) return null;
  return { query, start: atIndex, end: caret };
}

/**
 * Replace the active mention span with the chosen file's path.
 * Returns both the new value and the caret position that should follow.
 */
export function applyMention(
  value: string,
  trigger: MentionTrigger,
  replacement: string,
): { value: string; caret: number } {
  const insertion = `@${replacement} `;
  const next = value.slice(0, trigger.start) + insertion + value.slice(trigger.end);
  return { value: next, caret: trigger.start + insertion.length };
}
