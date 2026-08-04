// Where a completion menu is open in the composer's text, and over what range
// a selection writes.
//
// Pure text rules, no registry: the caller decides whether the trigger has any
// rows to show and closes the menu when it does not. That split is what lets
// `/workflowish` stay plain text — the trigger is open, nothing matches, the
// caller closes it — without this module knowing what a command is.

import { hasWordSeparator, isWordSeparator } from '../../utils/commandWords';

export interface CommandTrigger {
  /** What the user has typed after the slash, up to the caret. */
  query: string;
  /** Inclusive index of the `/`. Carried so the caller replaces a range. */
  start: number;
  /** Exclusive end of the replacement range: the caret. */
  end: number;
  /**
   * True only when the `/` is literally the first character of the draft.
   *
   * Provider commands, skills, and the intercepted commands are offered ONLY
   * here, because that is the only position where a send would treat the word
   * as a command — the CLI's router tests the raw string's first byte and AO's
   * interception mirrors it. AO's own commands (`/workflow`) are expanded by
   * the backend at any word position, so they ignore this flag.
   */
  atStart: boolean;
}

/**
 * Detect an open command completion at the caret, or null.
 *
 * Word-boundary rules, deliberately the same shape as `detectMentionTrigger`:
 * the `/` must sit at the start of the value or immediately after a separator,
 * and the caret must still be inside that word — a space between the `/` and
 * the caret closes the menu. A path segment (`src/lib`) never opens the menu
 * because its `/` follows a letter.
 *
 * "Separator" is `commandWordRanges`' separator, not a second definition of
 * one: the menu must never offer a completion for a word the AO matcher would
 * then refuse to colour.
 */
export function detectCommandTrigger(value: string, caret: number): CommandTrigger | null {
  if (caret < 1 || caret > value.length) return null;
  const before = value.slice(0, caret);
  const slash = before.lastIndexOf('/');
  if (slash < 0) return null;
  if (slash > 0 && !isWordSeparator(before[slash - 1])) return null;
  const query = before.slice(slash + 1);
  if (hasWordSeparator(query)) return null;
  return { query, start: slash, end: caret, atStart: slash === 0 };
}

export interface ReviewTargetTrigger {
  /** Text typed after `/review `, up to the caret. */
  query: string;
  /** Inclusive index where the target argument starts. */
  start: number;
  /** Exclusive end of the replacement range: the caret. */
  end: number;
}

const REVIEW_PREFIX = '/review';

/**
 * Detect the second completion level: `/review <target>`.
 *
 * Open while the caret sits in the argument of a leading `/review`, closed as
 * soon as the user commits to free text (`/review custom …`) or moves onto a
 * second line. `start` deliberately anchors at the first argument character
 * rather than at the caret's word, so selecting a target replaces whatever
 * partial target was typed — a branch name can contain a `/`, and a
 * word-scoped range would leave half of a previous pick behind.
 */
export function detectReviewTargetTrigger(
  value: string,
  caret: number,
): ReviewTargetTrigger | null {
  if (!value.startsWith(REVIEW_PREFIX)) return null;
  let start = REVIEW_PREFIX.length;
  if (start >= value.length || !isWordSeparator(value[start])) return null;
  while (start < value.length && isWordSeparator(value[start])) {
    // A newline ends the single-line argument: the user is writing prose now.
    if (value[start] === '\n' || value[start] === '\r') return null;
    start += 1;
  }
  if (caret < start || caret > value.length) return null;
  const query = value.slice(start, caret);
  if (query.includes('\n') || query.includes('\r')) return null;
  // `custom` takes free-form instructions; once its own word is settled there
  // is nothing left to complete.
  if (/^custom\s/.test(query)) return null;
  return { query, start, end: caret };
}
