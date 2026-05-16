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
 * Maximum number of display rows rendered for a diff file inside chat.
 * Files over this cap render the same number of rows plus a side-panel CTA.
 */
export const INLINE_DIFF_PREVIEW_LINE_COUNT = 15;

/**
 * Payload preview budget for inline chat diff rows. The sidebar owns the full
 * diff; chat only needs enough bytes to parse the capped preview above.
 */
export const INLINE_DIFF_PAYLOAD_PREVIEW_BYTES = 8 * 1024;
