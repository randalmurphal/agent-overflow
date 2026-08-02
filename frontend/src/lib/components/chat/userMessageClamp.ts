// Long user messages render clamped in the transcript, behind a fade-out and
// a "Show more" control. The threshold, and the pre-gate that decides which
// messages are candidates at all, live here so the row, the pane registry and
// the tests all read one number.

/**
 * Rendered lines a user message's text may occupy before it is clamped.
 *
 * The bubble and the composer textarea share their type scale EXACTLY
 * (`text-[0.8125rem] leading-[1.55]` on both), and the composer stops growing
 * at 200px — about 9.5 lines of that text, padding included. So a message an
 * author could see whole while typing is at most ~9 lines, and clamping at 12
 * leaves every one of them intact: the clamp only ever folds text that was
 * already scrolling inside the composer when it was sent (pasted logs, dumped
 * output, multi-part instructions). In pixels that is 12 x 13px x 1.55 ~ 242px.
 *
 * The clamp is applied as `${USER_MESSAGE_CLAMP_LINES}lh`, so this number is
 * literally a line count against whatever line box the cascade resolves —
 * there is no second px value that can drift out of sync with it.
 */
export const USER_MESSAGE_CLAMP_LINES = 12;

/**
 * Lower bound on the characters one rendered line of bubble text can hold.
 *
 * A pane is at least `PANE_DENSITY_MIN_WIDTHS.compact` (560px) wide, the
 * bubble caps at 82% of the timeline column and spends 32px of that on
 * horizontal padding, so the narrowest text column the layout can produce is
 * ~427px. At the bubble's 13px type that is 60+ characters for ordinary prose
 * and ~38 even for uniformly wide glyphs. 20 is far under both: it only has to
 * be a LOWER bound, because the pre-gate must never call a message that can
 * clamp a non-candidate.
 */
const MIN_CHARS_PER_RENDERED_LINE = 20;

/**
 * `scrollHeight` and `clientHeight` round independently (fractional line
 * boxes, borders, DPR), so treat sub-pixel overflow as "fits" — the same rule
 * `utils/paneWidths.ts` applies to horizontal overflow.
 */
export const USER_MESSAGE_CLAMP_EPSILON_PX = 1;

/**
 * Whether `text` could possibly exceed the clamp at ANY width the pane
 * layout can take. Deliberately one-sided: a `false` is a guarantee, a `true`
 * only means "measure it".
 *
 * Everything the clamp needs — the max-height, the fade, the measurement, the
 * toggle — hangs off this answer, so a short message keeps the exact DOM,
 * cascade and reactivity it had before the feature existed: one `<p>`, no
 * observer, no effect, no button.
 *
 * The bound is per hard line (soft wrapping can only ADD rendered lines to a
 * hard one, never merge two), and it stops as soon as the count passes the
 * clamp, so the scan is bounded by the threshold rather than by the message.
 */
export function userMessageMayClamp(text: string): boolean {
  let renderedLines = 0;
  let lineChars = 0;
  for (let i = 0; i <= text.length; i += 1) {
    if (i < text.length && text.charCodeAt(i) !== 10 /* \n */) {
      lineChars += 1;
      continue;
    }
    renderedLines += Math.max(1, Math.ceil(lineChars / MIN_CHARS_PER_RENDERED_LINE));
    if (renderedLines > USER_MESSAGE_CLAMP_LINES) return true;
    lineChars = 0;
  }
  return false;
}
