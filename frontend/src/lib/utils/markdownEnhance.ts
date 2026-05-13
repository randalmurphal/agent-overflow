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

import { findPathRanges, type PathRange } from './pathLinkify';
import type { PathRef } from '../types/models';
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

/**
 * Wrap path-shaped tokens in `container` with editor-open anchors.
 *
 * Two modes:
 * - **Allowlist mode** (`pathRefs` is an array, possibly empty): use
 *   only the paths Go validated against the workspace. Empty array =
 *   "Go saw nothing real," so nothing wraps. This is the
 *   bug-free path the assistant_text surface takes; pre-pathlinks
 *   history rows that have no `pathRefs` key in their meta still pass
 *   `[]` here so they render as plain text.
 * - **Local-regex mode** (`pathRefs` is `undefined`): fall back to the
 *   client-side `findPathRanges` heuristic. Non-assistant surfaces
 *   (Discussion `ChannelView`, ProposedPlan, AskUserQuestion,
 *   ComposerPendingUserInputPanel) pass `undefined` so they keep
 *   today's behavior — which CAN produce false positives, but no worse
 *   than before this refactor. Future work can wire those surfaces to
 *   their own validation pipelines.
 *
 * Either way the wrapping engine is identical: longest-first scan over
 * candidates with overlap avoidance, optional `:line:col` suffix
 * capture, and an `@`-prefix that widens the wrapped span when one is
 * present in the surrounding text and the boundary check passes.
 */
export function enhancePathLinks(
  container: HTMLElement,
  workspacePath: string,
  pathRefs?: PathRef[],
): void {
  ensurePathLinkDelegate();
  // Empty allowlist short-circuits: Go has positively asserted there
  // are no real paths in this prose, so the DOM walk is pure waste.
  if (pathRefs !== undefined && pathRefs.length === 0) return;
  // Build the per-path regex set once per call. Without this, every
  // candidate text node re-compiled N regex objects — pathological
  // when an allowlist of N paths crosses M qualifying text nodes
  // (N*M compiles). Sort longest-first so a path nested inside a
  // longer one defers to the outer match.
  const allowlistRegexes = pathRefs ? buildAllowlistRegexes(pathRefs) : undefined;

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
    linkifyTextNode(text, workspacePath, allowlistRegexes);
  }
}

interface AllowlistRegex {
  path: string;
  re: RegExp;
}

function buildAllowlistRegexes(refs: PathRef[]): AllowlistRegex[] {
  const seen = new Set<string>();
  const out: AllowlistRegex[] = [];
  for (const ref of refs) {
    if (!ref.path || seen.has(ref.path)) continue;
    seen.add(ref.path);
    // Mirror PATH_PATTERN's lookbehind + optional `@` + suffix shape,
    // anchored on the literal path string. `g` flag is required —
    // RegExp.exec relies on `lastIndex` advancing per match.
    const escaped = escapeRegex(ref.path);
    const re = new RegExp(
      `(?:^|(?<=[\\s(\\[{,;'"\`<>=]))(@)?${escaped}(?::(\\d+)(?::(\\d+))?)?`,
      'g',
    );
    out.push({ path: ref.path, re });
  }
  // Longest-first so `elsewhere/src/foo.ts` wraps before `src/foo.ts`
  // when both are in the allowlist; the inner overlap check then
  // skips the shorter nested match.
  out.sort((a, b) => b.path.length - a.path.length);
  return out;
}

/**
 * Find every occurrence of every allowlisted path in `value`. Each
 * occurrence's span widens to include an `@` prefix when one is
 * present with a safe boundary preceding it (`@src/foo.ts` after a
 * space wraps as `<a>@src/foo.ts</a>`; `name@host/path.ts` does
 * not wrap at all because the email's `@` fails the boundary check).
 * Longest-first ordering plus per-text-node overlap tracking means a
 * shorter path nested inside a longer one is wrapped only at the
 * outer match — no double-wrap.
 */
function findRangesFromAllowlist(value: string, regexes: AllowlistRegex[]): PathRange[] {
  if (regexes.length === 0) return [];
  const wrapped: Array<{ start: number; end: number }> = [];
  const out: PathRange[] = [];
  for (const { path, re } of regexes) {
    // Reset stateful global regex; the same compiled instance is
    // reused across text nodes, so a previous text node's final
    // `lastIndex` would otherwise skip prefix matches here.
    re.lastIndex = 0;
    let m: RegExpExecArray | null;
    while ((m = re.exec(value)) !== null) {
      const start = m.index;
      const end = m.index + m[0].length;
      if (intersectsAny(start, end, wrapped)) continue;
      out.push({
        start,
        end,
        path,
        line: m[2] ? Number(m[2]) : undefined,
        col: m[3] ? Number(m[3]) : undefined,
      });
      wrapped.push({ start, end });
    }
  }
  out.sort((a, b) => a.start - b.start);
  return out;
}

function intersectsAny(
  start: number,
  end: number,
  ranges: Array<{ start: number; end: number }>,
): boolean {
  for (const r of ranges) {
    if (start < r.end && end > r.start) return true;
  }
  return false;
}

function escapeRegex(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
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

function linkifyTextNode(
  text: Text,
  workspacePath: string,
  allowlistRegexes: AllowlistRegex[] | undefined,
): void {
  const value = text.nodeValue ?? '';
  const ranges = allowlistRegexes !== undefined
    ? findRangesFromAllowlist(value, allowlistRegexes)
    : findPathRanges(value);
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
