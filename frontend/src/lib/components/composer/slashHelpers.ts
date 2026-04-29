export interface SlashTrigger {
  /** Partial command text after the leading `/`, excluding the slash itself. */
  text: string;
  /** Index of the leading `/` in the textarea value (always 0 today). */
  start: number;
}

/**
 * Detects an active `/command` trigger at the very start of the textarea.
 *
 * Rules — intentionally narrower than mentions:
 * - The `/` must be the first character of the entire message (index 0). A
 *   slash further into the buffer is part of content (e.g. `src/foo`), not a
 *   command invocation.
 * - The partial text after `/` is everything up to the caret, with no
 *   whitespace; a space closes the trigger (the user has moved past
 *   command-selection into normal typing).
 * - `//` is not a command — users sometimes type a stray double-slash and
 *   we'd rather stay quiet than pop a useless popover.
 * - The caret must sit at the end of the partial trigger (no split-edit
 *   case — the popover reflects live typing only).
 */
export function detectSlashTrigger(value: string, caret: number): SlashTrigger | null {
  if (caret < 0 || caret > value.length) return null;
  if (value.length === 0) return null;
  if (value[0] !== '/') return null;
  // `//...` — treat as literal content, not a command trigger.
  if (value.length >= 2 && value[1] === '/') return null;

  const partial = value.slice(1, caret);
  // A space closes the trigger: `/foo bar` is past command-selection.
  if (/\s/.test(partial)) return null;

  // Caret must be on the trigger span. If there's non-whitespace content
  // past the caret that would be joined onto the command (no space
  // separator), treat the trigger as closed — the user is editing
  // mid-word, not selecting.
  const after = value.slice(caret);
  if (after.length > 0 && !/^\s/.test(after)) return null;

  return { text: partial, start: 0 };
}
