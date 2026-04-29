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
  const ancestor = range.commonAncestorContainer;
  const ancestorEl = ancestor.nodeType === Node.ELEMENT_NODE
    ? (ancestor as Element)
    : ancestor.parentElement;
  if (!ancestorEl?.closest('.markdown-body')) return;

  const md = serializeRangeToMarkdown(range);
  if (md === null) return;

  event.preventDefault();
  event.clipboardData.setData('text/plain', md);
}

/** Test-only export: lets specs reset delegate state so installation
 * is observable across cases. Not part of the public API. */
export function __resetMarkdownCopyDelegateForTest(): void {
  if (installed && typeof document !== 'undefined') {
    document.removeEventListener('copy', handleMarkdownCopy);
  }
  installed = false;
}
