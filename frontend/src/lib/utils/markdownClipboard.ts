// Dual-flavor clipboard writes for markdown chat content.
//
// One place decides what a markdown copy puts on the clipboard:
// `text/plain` carries the markdown (editors, terminals, code fields
// keep getting exactly what they got before) and `text/html` carries
// the allowlisted rendering from `markdownHtmlSerialize.ts`, which is
// what Google Docs / Slack / Teams / Outlook / Confluence prefer.
//
// Two writers because there are two kinds of copy, and each has one
// correct API:
//
//   - `copyMarkdownToClipboard` — a Copy button. Programmatic, async,
//     `navigator.clipboard.write` with a `ClipboardItem`.
//   - `applyMarkdownFlavors` — the selection `copy` event delegate.
//     Inside a copy event, `event.clipboardData` IS the clipboard being
//     assembled; `setData` is synchronous, needs no permission, and
//     works in every engine. Calling `navigator.clipboard.write` there
//     would race the event's own write and ask for a permission the
//     event already granted.
//
// Both take their flavors from `markdownClipboardFlavors`, so the two
// paths cannot drift in what they put where.
//
// Feature detection, not engine sniffing: we ship under WebKitGTK,
// WKWebView and WebView2, and `ClipboardItem` / `clipboard.write` are
// checked at the call. Without them (or if the write rejects) the
// markdown-only `writeText` path runs exactly as it did before this
// flavor existed — a copy never fails because the html flavor could
// not be produced.

import { copyToClipboard } from './clipboard';
import { markdownToClipboardHtml } from './markdownHtmlSerialize';

export type MarkdownClipboardFlavors = {
  /** The markdown itself. */
  plain: string;
  /** Allowlisted HTML, or `''` when there is nothing renderable. */
  html: string;
};

/**
 * The clipboard flavors for one markdown payload.
 *
 * Deliberately not wrapped in a try/catch: a serializer throw is a bug,
 * and each caller has a different correct response to it (the copy
 * event lets it escape so the browser's own copy still runs; the
 * button path degrades to plain text). Swallowing it here would take
 * that choice away from both.
 */
export function markdownClipboardFlavors(markdown: string): MarkdownClipboardFlavors {
  return { plain: markdown, html: markdownToClipboardHtml(markdown) };
}

/**
 * Write both flavors into a `copy`/`cut` event's DataTransfer.
 *
 * The html flavor is skipped when empty so a target that prefers
 * `text/html` is never handed a blank document.
 */
export function applyMarkdownFlavors(
  data: DataTransfer,
  flavors: MarkdownClipboardFlavors,
): void {
  data.setData('text/plain', flavors.plain);
  if (flavors.html) data.setData('text/html', flavors.html);
}

/**
 * Copy markdown to the system clipboard with both flavors.
 *
 * Drop-in for `copyToClipboard` on markdown-bearing surfaces; same
 * `Promise<boolean>` contract.
 *
 * The rich path is reached with no `await` in front of it so the call
 * still sits in the user-gesture task — WKWebView rejects a
 * `clipboard.write` that resumes after an await (see
 * `diagramClipboard.ts`). Callers that must resolve the text first
 * (lazy payload getters) inherit whatever their await already cost;
 * they are no worse off than the plain path they used before.
 */
export async function copyMarkdownToClipboard(markdown: string): Promise<boolean> {
  const item = richClipboardItem(markdown);
  if (item) {
    try {
      await navigator.clipboard.write([item]);
      return true;
    } catch (err) {
      // The clipboard still has to end up with the markdown, so this
      // degrades rather than failing — but it is not swallowed: a
      // rejected write here means an engine we believed supported the
      // rich path does not, which is exactly what we want to see.
      console.error('Rich clipboard write failed; copying markdown only:', err);
    }
  }
  return copyToClipboard(markdown);
}

/**
 * A both-flavors `ClipboardItem`, or `null` when the environment or the
 * content can't support one.
 *
 * Synchronous by construction — see the gesture note above.
 */
function richClipboardItem(markdown: string): ClipboardItem | null {
  if (typeof ClipboardItem !== 'function') return null;
  if (typeof navigator === 'undefined') return null;
  if (typeof navigator.clipboard?.write !== 'function') return null;

  let flavors: MarkdownClipboardFlavors;
  try {
    flavors = markdownClipboardFlavors(markdown);
  } catch (err) {
    console.error('Markdown → HTML serialization failed; copying markdown only:', err);
    return null;
  }
  if (!flavors.html) return null;

  return new ClipboardItem({
    'text/plain': new Blob([flavors.plain], { type: 'text/plain' }),
    'text/html': new Blob([flavors.html], { type: 'text/html' }),
  });
}
