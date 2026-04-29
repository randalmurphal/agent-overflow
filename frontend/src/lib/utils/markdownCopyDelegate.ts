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
// Intentionally has no try/catch around the serializer: if it ever
// throws we want the browser default to take over rather than copying
// nothing — leaving `preventDefault` un-called means the browser's
// own clipboard fill still happens. Reordering the early checks so
// the listener bails before `preventDefault` keeps that property.

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
  if (!selection || selection.rangeCount === 0 || selection.isCollapsed) return;
  const range = selection.getRangeAt(0);
  if (!rangeTouchesMarkdownBody(range)) return;

  const md = serializeRangeToMarkdown(range);
  if (md === null) return;

  event.preventDefault();
  event.clipboardData.setData('text/plain', md);
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
