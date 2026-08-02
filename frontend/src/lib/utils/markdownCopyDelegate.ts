// Markdown-aware copy delegate.
//
// The browser default for `copy` over rendered markdown drops every
// markdown marker — `**bold**` becomes `bold`, `1.` list numbers
// vanish, fenced code loses its fences, etc. A document-level `copy`
// listener (installed lazily on first markdown enhancement) intercepts
// when the selection is inside a `.markdown-body` surface and replaces
// the clipboard text with the round-tripped markdown produced by
// `serializeRangeToMarkdown`. Selections outside any markdown surface
// fall through untouched.
//
// A selection can hold more than one range — Gecko builds one per
// ctrl+click / ctrl+drag, and copying such a selection is the only way
// to grab two non-adjacent passages at once. Reading `getRangeAt(0)`
// dropped every range after the first, so that copy silently lost most
// of what was highlighted. All of them are serialized (see
// `usableRanges`). Blink and WebKit collapse a selection to a single
// range, so this is dormant there and single-range output is unchanged.
//
// The clipboard gets two flavors: the markdown as `text/plain`, and an
// allowlisted HTML rendering of that same markdown as `text/html`, so a
// paste into Google Docs / Slack / Teams / Outlook / Confluence keeps
// headings, bold, tables and code instead of raw `**`/`|` syntax. Both
// come from `markdownClipboard.ts`; nothing about the flavor pair is
// decided here.
//
// Intentionally has no try/catch around EITHER serializer: if one ever
// throws we want the browser default to take over rather than copying
// nothing — leaving `preventDefault` un-called means the browser's
// own clipboard fill still happens. Both serializers therefore run
// BEFORE `preventDefault`, and the early checks bail before it too.

import {
  applyMarkdownFlavors,
  markdownClipboardFlavors,
} from './markdownClipboard';
import { serializeRangeToMarkdown } from './markdownSerialize';

let installed = false;

export function ensureMarkdownCopyDelegate(): void {
  if (installed) return;
  if (typeof document === 'undefined') return;
  installed = true;
  document.addEventListener('copy', handleMarkdownCopy);
}

function handleMarkdownCopy(event: ClipboardEvent): void {
  if (!event.clipboardData) return;
  const selection = window.getSelection();
  if (!selection) return;
  const ranges = usableRanges(selection);
  // One range in markdown is enough to claim the copy, the same leniency
  // `rangeTouchesMarkdownBody` applies to a single range's endpoints. The
  // rest are serialized alongside it — the walker renders a non-markdown
  // range as the plain text the browser would have produced for it.
  if (!ranges.some(rangeTouchesMarkdownBody)) return;

  const parts: string[] = [];
  for (const range of ranges) {
    const md = serializeRangeToMarkdown(range);
    if (md !== null) parts.push(md);
  }
  if (parts.length === 0) return;

  // Blank line between ranges: they are disjoint passages, and a single
  // newline would fuse two paragraphs into one on paste-back — and would
  // fuse them in the html flavor too, which is lexed from this string.
  const flavors = markdownClipboardFlavors(parts.join('\n\n'));

  event.preventDefault();
  applyMarkdownFlavors(event.clipboardData, flavors);
}

/**
 * Every non-collapsed range of the selection, in document order.
 *
 * Collapsed ranges are dropped here rather than downstream so they cannot
 * contribute an empty part and a stray separator — Gecko leaves a collapsed
 * range behind for a ctrl+click that lands without a drag, and the caret's
 * own range rides along in some ctrl+drag sequences. Order is enforced
 * rather than assumed: `addRange` appends, so the range order is the order
 * the user clicked, and copy output must read the way the document does.
 *
 * Deliberately outside any try/catch, like the serializer call it feeds:
 * `compareBoundaryPoints` throws across documents, and throwing here — still
 * before `preventDefault` — leaves the browser's own clipboard fill intact.
 */
function usableRanges(selection: Selection): Range[] {
  const ranges: Range[] = [];
  for (let i = 0; i < selection.rangeCount; i += 1) {
    const range = selection.getRangeAt(i);
    if (!range.collapsed) ranges.push(range);
  }
  if (ranges.length < 2) return ranges;
  return ranges.sort((a, b) => a.compareBoundaryPoints(Range.START_TO_START, b));
}

function rangeTouchesMarkdownBody(range: Range): boolean {
  // Checking the commonAncestorContainer alone is too strict: a
  // drag-select that overshoots an assistant message by even one
  // character (into the timestamp row below, or whitespace in a
  // wrapper div) lands the LCA on a node that has no `.markdown-body`
  // ancestor, so the delegate bails and browser-default copy wins —
  // exactly the case people hit when copy/pasting a chat reply.
  // Both endpoints are checked so a selection that starts or ends
  // inside a markdown surface still gets markdown-aware copy. The
  // serializer's cloneContents will only walk what was actually
  // selected, so the non-markdown chrome at the edge becomes plain
  // text in the result, which is the same thing the browser would
  // have produced for that span.
  return endpointInMarkdownBody(range.startContainer)
    || endpointInMarkdownBody(range.endContainer);
}

function endpointInMarkdownBody(node: Node): boolean {
  const el = node.nodeType === Node.ELEMENT_NODE
    ? (node as Element)
    : node.parentElement;
  return el?.closest('.markdown-body') != null;
}

/** Test-only export: lets specs reset delegate state so installation
 * is observable across cases. Not part of the public API. */
export function __resetMarkdownCopyDelegateForTest(): void {
  if (installed && typeof document !== 'undefined') {
    document.removeEventListener('copy', handleMarkdownCopy);
  }
  installed = false;
}
