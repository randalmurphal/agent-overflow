import { MAX_ADVANCE_PER_TICK_CHARS } from '../../lib/markdown/smoothing/PerItemSmoother';

/**
 * The most characters one processed reveal tick may land from `text`: the
 * smoother's per-tick cap, or one word unit (a non-whitespace run with its
 * trailing whitespace) when a unit of the text is longer, since the reveal
 * never splits a word and lands an oversized unit whole once its budget
 * covers it.
 */
export function revealTickBound(text: string): number {
  let longest = 0;
  for (const unit of text.match(/\S+\s*/g) ?? []) longest = Math.max(longest, unit.length);
  return Math.max(MAX_ADVANCE_PER_TICK_CHARS, longest);
}
