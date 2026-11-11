// In-text find highlights for one pane's scroll container.
//
// Matches paint through the CSS Custom Highlight API (`CSS.highlights`,
// `::highlight(name)`), so they sit over rendered Markdown, code and diffs
// without a DOM change. Each pane registers its own names
// (`findHighlightName`) with one rule per name in a shared constructed
// stylesheet, so two open bars never paint each other's query.
//
// The walk covers only what is mounted under `root` (a virtualized list
// mounts its visible rows) and reruns when that DOM changes, coalesced
// through the refresh scheduler. Text is matched by `findMatch.ts` within a
// block, across inline markup. Where the API is missing the matches are
// still computed, for providers that count or step through them, and
// nothing paints. An empty query or `dispose` releases every range, the
// observer and the names.

import { createRefreshScheduler, type RefreshScheduler } from './refreshScheduler';
import { findNeedle, lowerPreservingLength } from './findMatch';

export interface FindMatch {
  readonly range: Range;
  /** The `rowKeyAttribute` of the row holding the match, when rows are keyed. */
  readonly rowKey: string | null;
}

/** The current match: every match in one row, or one match by index. */
export type FindCurrent = { row: string } | { match: number } | null;

export interface FindHighlighterOptions {
  root: HTMLElement;
  /** From `findHighlightName`; the current match paints as `${name}-current`. */
  name: string;
  rowSelector?: string;
  rowKeyAttribute?: string;
  /** Called after every recompute with the new matches. */
  onMatches?(matches: readonly FindMatch[]): void;
}

export interface FindHighlighter {
  setQuery(query: string): void;
  setCurrent(current: FindCurrent): void;
  /** Matches in document order, for the current query. */
  readonly matches: readonly FindMatch[];
  dispose(): void;
}

/** Bounds the walk's output on a pathological query such as "e". */
const MAX_MATCHES = 5000;
const RECOMPUTE_DELAY_MS = 40;
const RECOMPUTE_MAX_WAIT_MS = 200;
const SKIP_SELECTOR = 'script,style,noscript,textarea,[data-find-skip]';
const BLOCK_SELECTOR = [
  'address', 'article', 'aside', 'blockquote', 'dd', 'details', 'div', 'dl', 'dt',
  'figcaption', 'figure', 'footer', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'header',
  'li', 'main', 'nav', 'ol', 'p', 'pre', 'section', 'summary', 'table', 'td', 'th',
  'tr', 'ul',
].join(',');

export function findHighlightsSupported(): boolean {
  return typeof CSS !== 'undefined'
    && 'highlights' in CSS
    && typeof Highlight === 'function'
    && typeof document !== 'undefined'
    && 'adoptedStyleSheets' in document
    && typeof CSSStyleSheet === 'function';
}

/** The highlight name for a layout pane; a valid CSS identifier. */
export function findHighlightName(paneId: string): string {
  return `ao-find-${paneId.replace(/[^A-Za-z0-9_-]/g, '_')}`;
}

// ---- Rules ----------------------------------------------------------------

let sheet: CSSStyleSheet | null = null;
const ruleUsers = new Map<string, number>();

function ruleTexts(name: string): string[] {
  return [
    `::highlight(${name}) { background-color: var(--find-match); }`,
    `::highlight(${name}-current) { background-color: var(--find-match-current); }`,
  ];
}

function retainRules(name: string): void {
  const users = ruleUsers.get(name) ?? 0;
  ruleUsers.set(name, users + 1);
  if (users > 0) return;
  if (!sheet) {
    sheet = new CSSStyleSheet();
    document.adoptedStyleSheets = [...document.adoptedStyleSheets, sheet];
  }
  for (const text of ruleTexts(name)) sheet.insertRule(text, sheet.cssRules.length);
}

function releaseRules(name: string): void {
  const users = ruleUsers.get(name) ?? 0;
  if (users > 1) {
    ruleUsers.set(name, users - 1);
    return;
  }
  ruleUsers.delete(name);
  if (!sheet) return;
  const prefixes = [`::highlight(${name})`, `::highlight(${name}-current)`];
  for (let i = sheet.cssRules.length - 1; i >= 0; i -= 1) {
    const text = sheet.cssRules[i].cssText;
    if (prefixes.some((prefix) => text.startsWith(prefix))) sheet.deleteRule(i);
  }
}

// ---- Matching -------------------------------------------------------------

interface Segment {
  nodes: Text[];
  /** Start offset of each node in `text`. */
  starts: number[];
  text: string;
  block: Element | null;
}

function collectSegments(root: HTMLElement): Segment[] {
  const visible = new Map<Element, boolean>();
  const isVisible = (element: Element): boolean => {
    let answer = visible.get(element);
    if (answer === undefined) {
      answer = typeof element.checkVisibility === 'function' ? element.checkVisibility() : true;
      visible.set(element, answer);
    }
    return answer;
  };
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  const segments: Segment[] = [];
  let current: Segment | null = null;
  for (let node = walker.nextNode() as Text | null; node; node = walker.nextNode() as Text | null) {
    const parent = node.parentElement;
    if (!parent || node.data.length === 0) continue;
    if (parent.closest(SKIP_SELECTOR) || !isVisible(parent)) continue;
    const block = parent.closest(BLOCK_SELECTOR);
    if (!current || current.block !== block) {
      current = { nodes: [], starts: [], text: '', block };
      segments.push(current);
    }
    current.starts.push(current.text.length);
    current.nodes.push(node);
    current.text += lowerPreservingLength(node.data);
  }
  return segments;
}

function pointAt(segment: Segment, offset: number, end: boolean): [Text, number] {
  let index = segment.starts.length - 1;
  while (index > 0 && (end ? segment.starts[index] >= offset : segment.starts[index] > offset)) index -= 1;
  return [segment.nodes[index], offset - segment.starts[index]];
}

function collectMatches(root: HTMLElement, needle: string, rowSelector?: string, rowKeyAttribute?: string): FindMatch[] {
  const matches: FindMatch[] = [];
  for (const segment of collectSegments(root)) {
    let at = segment.text.indexOf(needle);
    if (at < 0) continue;
    const row = rowSelector ? segment.nodes[0].parentElement?.closest(rowSelector) : null;
    const rowKey = row && rowKeyAttribute ? row.getAttribute(rowKeyAttribute) : null;
    while (at >= 0) {
      const range = document.createRange();
      const [startNode, startOffset] = pointAt(segment, at, false);
      const [endNode, endOffset] = pointAt(segment, at + needle.length, true);
      range.setStart(startNode, startOffset);
      range.setEnd(endNode, endOffset);
      matches.push({ range, rowKey });
      if (matches.length >= MAX_MATCHES) return matches;
      at = segment.text.indexOf(needle, at + needle.length);
    }
  }
  return matches;
}

// ---- Highlighter ------------------------------------------------------------

export function createFindHighlighter(options: FindHighlighterOptions): FindHighlighter {
  const { root, name, rowSelector, rowKeyAttribute } = options;
  const currentName = `${name}-current`;
  const paints = findHighlightsSupported();
  let needle = '';
  let current: FindCurrent = null;
  let matches: FindMatch[] = [];
  let observer: MutationObserver | null = null;
  let scheduler: RefreshScheduler | null = null;
  let rulesHeld = false;
  let disposed = false;

  function isCurrent(match: FindMatch, index: number): boolean {
    if (!current) return false;
    return 'row' in current ? match.rowKey === current.row : index === current.match;
  }

  function paint(): void {
    if (!paints) return;
    if (matches.length === 0) {
      CSS.highlights.delete(name);
      CSS.highlights.delete(currentName);
      return;
    }
    const all = new Highlight();
    const focused = new Highlight();
    focused.priority = 1;
    matches.forEach((match, index) => (isCurrent(match, index) ? focused : all).add(match.range));
    CSS.highlights.set(name, all);
    CSS.highlights.set(currentName, focused);
  }

  function recompute(): void {
    if (disposed) return;
    matches = needle ? collectMatches(root, needle, rowSelector, rowKeyAttribute) : [];
    paint();
    options.onMatches?.(matches);
  }

  function follow(): void {
    if (observer) return;
    scheduler = createRefreshScheduler({
      name: 'find highlights',
      delayMs: RECOMPUTE_DELAY_MS,
      maxWaitMs: RECOMPUTE_MAX_WAIT_MS,
      run: recompute,
    });
    const pending = scheduler;
    observer = new MutationObserver(() => pending.request());
    observer.observe(root, { childList: true, subtree: true, characterData: true });
    if (paints && !rulesHeld) {
      retainRules(name);
      rulesHeld = true;
    }
  }

  function unfollow(): void {
    observer?.disconnect();
    observer = null;
    scheduler?.dispose();
    scheduler = null;
    if (rulesHeld) {
      releaseRules(name);
      rulesHeld = false;
    }
  }

  return {
    setQuery(query: string): void {
      if (disposed) return;
      needle = findNeedle(query);
      if (needle) follow();
      else unfollow();
      recompute();
    },
    setCurrent(next: FindCurrent): void {
      if (disposed) return;
      current = next;
      paint();
    },
    get matches() {
      return matches;
    },
    dispose(): void {
      if (disposed) return;
      needle = '';
      matches = [];
      paint();
      unfollow();
      disposed = true;
    },
  };
}
