// Shared threshold + helpers for deciding when an inline preview is too long
// and should be deferred behind a "Show all" button backed by GetPayloadData.
//
// The goal is bounded pane memory: any renderer that might receive
// provider-authored content (tool results, command output, thinking,
// diff fragments) should gate large strings through LazyContentBlock so the
// full payload only lands in the DOM when the user opts in.

/**
 * Upper bound, in characters, for content that is safe to inline render.
 * Chosen as ~2 KiB because that fits a typical tool-result detail line or a
 * short diff preview without scrolling; anything larger is almost certainly
 * a file dump we should lazy-load.
 *
 * The check is done in JS chars (UTF-16 code units), which is a reasonable
 * proxy for bytes at this threshold — a byte-exact measurement would cost
 * a TextEncoder round-trip on every render and we don't need that precision
 * to decide between "inline" and "show-all button".
 */
export const MAX_INLINE_BYTES = 2048;

/**
 * True when `preview` exceeds MAX_INLINE_BYTES. See the note on
 * MAX_INLINE_BYTES: `.length` is a byte estimate (UTF-16 code units); the
 * threshold is coarse enough that exactness doesn't matter for the decision.
 */
export function shouldLazyLoad(preview: string): boolean {
  return preview.length > MAX_INLINE_BYTES;
}

/**
 * Returns `text` unchanged when it fits under `max`, otherwise slices it and
 * appends an ellipsis marker. Exported so renderers that need a visual
 * preview (not a full "Show all" flow) can stay consistent.
 */
export function truncateForPreview(text: string, max = MAX_INLINE_BYTES): string {
  if (text.length <= max) return text;
  return text.slice(0, max) + '…';
}

/**
 * Hard line-count cap for an inline diff file block. At or below this,
 * the full file diff renders inline with no internal scroll. Above
 * this, the block renders a small teaser + a "Show full diff in side
 * panel" CTA — keeps the chat scroll surface as the only scroll
 * container, and pushes large diffs to the proper viewer (the diff
 * sidebar). Picked by feel — ~3 viewport-heights of one diff at the
 * timeline's text-xs leading-tight font sizing. Tunable.
 */
export const MAX_INLINE_DIFF_LINES = 200;

/**
 * Number of lines rendered as the teaser when a file exceeds
 * MAX_INLINE_DIFF_LINES. Just enough to give context (path + first
 * hunk's intent) without consuming serious vertical real estate.
 */
export const DIFF_TEASER_LINE_COUNT = 30;
