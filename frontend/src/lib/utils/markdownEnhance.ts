// Post-render enhancements for ChatMarkdown.
//
// The legacy `enhanceMarkdown(container)` orchestrator that ran shiki,
// mermaid, katex, copy buttons, and path linkification on freshly-
// painted `{@html}` output is gone — `<Streamdown>` now owns code /
// math / diagram rendering as opt-in component children, so per-pass
// teardown and re-mount of those enhancements no longer applies.
//
// Two pieces stayed: project-relative path linkification of prose text
// (Streamdown doesn't know what a "workspace path" is) and the
// document-level markdown-aware copy delegate. Both are post-process
// passes that work on any DOM tree, regardless of how it was
// constructed.

import { findPathRanges } from './pathLinkify';
import { OpenInEditor } from '../stores/bindings';
import { addToast } from '../stores/toast.svelte';
import { errString } from './errors';

// Re-export so ChatMarkdown only needs one import for both helpers.
export {
  ensureMarkdownCopyDelegate,
  __resetMarkdownCopyDelegateForTest,
} from './markdownCopyDelegate';

// Path-link enrichment.
//
// Walks the rendered markdown for text nodes whose ancestor chain is
// NOT a <pre> (block code) but tolerates being inside an inline <code>
// (so things like ``src/lib/foo.ts`` linkify, but anything inside a
// fenced code block does not). Each matching text node is replaced
// with a sequence of plain text + <a class="editor-link" ...>
// fragments. A single document-level click delegate (installed lazily
// on first use) handles the actual binding call.
//
// Linkifier ↔ click-delegate contract — every <a class="editor-link">
// the linkifier emits exposes:
//   data-path           — required, the path token (relative or absolute).
//   data-line           — optional, 1-indexed line number from `:N` suffix.
//   data-col            — optional, 1-indexed column from `:N:M` suffix.
//   data-workspace-path — optional, the absolute base used to resolve a
//                         relative `data-path`. Stamped at render time
//                         from the EnhanceOptions.workspacePath input so
//                         a relative click survives the surface unmounting
//                         between render and click. Empty / absent =
//                         "treat data-path as absolute or fail server-side."
// `handlePathLinkClick` reads these attributes and forwards them to the
// `OpenInEditor` Go binding (path, line, col, workspacePath).

const EDITOR_LINK_CLASS = 'editor-link';
let pathLinkDelegateInstalled = false;

function ensurePathLinkDelegate(): void {
  if (pathLinkDelegateInstalled) return;
  if (typeof document === 'undefined') return;
  pathLinkDelegateInstalled = true;
  document.addEventListener('click', handlePathLinkClick);
}

function handlePathLinkClick(event: MouseEvent): void {
  const target = event.target;
  if (!(target instanceof HTMLElement)) return;
  const link = target.closest<HTMLElement>(`.${EDITOR_LINK_CLASS}`);
  if (!link) return;
  const path = link.dataset.path;
  if (!path) return;
  // Anchors with href="#" would scroll to the top of the page; cancel
  // the default before the async binding call kicks off.
  event.preventDefault();
  const line = Number(link.dataset.line ?? '0') || 0;
  const col = Number(link.dataset.col ?? '0') || 0;
  // workspacePath was stamped at linkify time so a relative path stays
  // resolvable even if the rendering surface has unmounted between
  // render and click.
  const workspacePath = link.dataset.workspacePath ?? '';
  void invokePathLink(path, line, col, workspacePath);
}

async function invokePathLink(
  path: string,
  line: number,
  col: number,
  workspacePath: string,
): Promise<void> {
  try {
    await OpenInEditor(path, line, col, workspacePath);
  } catch (err) {
    addToast('error', errString(err));
  }
}

export function enhancePathLinks(container: HTMLElement, workspacePath: string): void {
  ensurePathLinkDelegate();
  // Collect candidate text nodes first so the in-place replacement
  // doesn't disturb the iterator (replaceWith mutates the parent's
  // child list).
  const textNodes: Text[] = [];
  const walker = document.createTreeWalker(container, NodeFilter.SHOW_TEXT, {
    acceptNode(node) {
      // Only Text nodes pass SHOW_TEXT, but TS still types this as
      // Node. Cast and inspect ancestor chain.
      const text = node as Text;
      const value = text.nodeValue;
      if (!value || value.length < 3) return NodeFilter.FILTER_REJECT;
      if (!hasPathSeparator(value)) return NodeFilter.FILTER_REJECT;
      if (insidePre(text)) return NodeFilter.FILTER_REJECT;
      if (insideEditorLink(text)) return NodeFilter.FILTER_REJECT;
      return NodeFilter.FILTER_ACCEPT;
    },
  });
  let current = walker.nextNode();
  while (current) {
    textNodes.push(current as Text);
    current = walker.nextNode();
  }
  for (const text of textNodes) {
    linkifyTextNode(text, workspacePath);
  }
}

function hasPathSeparator(text: string): boolean {
  // Cheap pre-filter so we don't run the full regex on every prose
  // text node. A path always has at least one `/`.
  return text.indexOf('/') !== -1;
}

function insidePre(node: Node): boolean {
  let cursor: Node | null = node.parentNode;
  while (cursor) {
    if (cursor instanceof HTMLElement && cursor.tagName === 'PRE') return true;
    cursor = cursor.parentNode;
  }
  return false;
}

function insideEditorLink(node: Node): boolean {
  let cursor: Node | null = node.parentNode;
  while (cursor) {
    if (cursor instanceof HTMLElement && cursor.classList.contains(EDITOR_LINK_CLASS)) {
      return true;
    }
    cursor = cursor.parentNode;
  }
  return false;
}

function linkifyTextNode(text: Text, workspacePath: string): void {
  const value = text.nodeValue ?? '';
  const ranges = findPathRanges(value);
  if (ranges.length === 0) return;
  const parent = text.parentNode;
  if (!parent) return;
  const fragment = document.createDocumentFragment();
  let cursor = 0;
  for (const range of ranges) {
    if (range.start > cursor) {
      fragment.appendChild(document.createTextNode(value.slice(cursor, range.start)));
    }
    const link = document.createElement('a');
    link.className = EDITOR_LINK_CLASS;
    // href="#" gives anchor styling + keyboard activation; the global
    // click delegate cancels the default navigation.
    link.href = '#';
    link.dataset.path = range.path;
    if (range.line) link.dataset.line = String(range.line);
    if (range.col) link.dataset.col = String(range.col);
    if (workspacePath) link.dataset.workspacePath = workspacePath;
    link.textContent = value.slice(range.start, range.end);
    fragment.appendChild(link);
    cursor = range.end;
  }
  if (cursor < value.length) {
    fragment.appendChild(document.createTextNode(value.slice(cursor)));
  }
  parent.replaceChild(fragment, text);
}

// Test-only export: lets specs reset delegate state so installation is
// observable across cases. Not part of the public API.
export function __resetPathLinkDelegateForTest(): void {
  if (pathLinkDelegateInstalled && typeof document !== 'undefined') {
    document.removeEventListener('click', handlePathLinkClick);
  }
  pathLinkDelegateInstalled = false;
}
