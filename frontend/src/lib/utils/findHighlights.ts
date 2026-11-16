// In-text find highlights for one pane's scroll container.
//
// Matches paint through the CSS Custom Highlight API (`CSS.highlights`,
// `::highlight(name)`), so they sit over rendered Markdown, code and diffs
// without a DOM change. Each pane registers its own names
// (`findHighlightName`) with one rule per name in a shared constructed
// stylesheet, so two open bars never paint each other's query.
//
// The walk covers only what is mounted under `root` (a virtualized list
// mounts its visible rows) and reruns when that DOM changes or a
// <details> under it opens or closes, coalesced through the refresh
// scheduler. Text is matched by the find rule (`findMatch.ts`) within a
// run: the text of one block, across inline markup. Content find skips
// (`data-find-skip`, images, scripts) is in no run and ends the run it
// interrupts, so a match never spans text the reader sees apart. With
// `rowsOnly`, only text inside a row is matched, and a row boundary ends a
// run; with `textSelector`, only text inside the elements it selects (a
// row's `data-find-text` elements, what its count reads), and the edge of
// one ends a run. Where the API is missing the matches are
// still computed, for providers that count or step through them, and
// nothing paints. An empty query or `dispose` releases every range, the
// observer and the names.

import { createRefreshScheduler, type RefreshScheduler } from './refreshScheduler';
import { findNeedle, foldFindText } from './findMatch';

/** A run of text find matches within, and whether it shows now (a run
 * inside a closed <details> does not). */
export interface FindRun {
  readonly text: string;
  readonly shown: boolean;
}

export interface FindMatch {
  readonly range: Range;
  /** The `rowKeyAttribute` of the row holding the match, when rows are keyed. */
  readonly rowKey: string | null;
}

/**
 * The current match: every match in one row, the `occurrence`-th match in
 * one row (counted in document order), or one match by index.
 */
export type FindCurrent = { row: string; occurrence?: number } | { match: number } | null;

/** A current match, or how to find it in the content at each paint (an
 * owner whose content moves under the rows derives the row from it). */
export type FindCurrentSource = FindCurrent | (() => FindCurrent);

export interface FindHighlighterOptions {
  root: HTMLElement;
  /** From `findHighlightName`; the current match paints as `${name}-current`. */
  name: string;
  rowSelector?: string;
  rowKeyAttribute?: string;
  /** Match only text inside a `rowSelector` element. */
  rowsOnly?: boolean;
  /** Match only text inside an element this selects. */
  textSelector?: string;
  /** Called after every recompute with the new matches. */
  onMatches?(matches: readonly FindMatch[]): void;
}

export interface FindHighlighter {
  setQuery(query: string): void;
  setCurrent(current: FindCurrentSource): void;
  /** Matches in document order, for the current query. */
  readonly matches: readonly FindMatch[];
  /** The range of the current match when it is mounted; the first of a row. */
  currentRange(): Range | null;
  dispose(): void;
}

/** Bounds the walk's output on a pathological query such as "e". */
const MAX_MATCHES = 5000;
const RECOMPUTE_DELAY_MS = 40;
const RECOMPUTE_MAX_WAIT_MS = 200;
const SKIP_SELECTOR = 'script,style,noscript,textarea,[data-find-skip]';
// What ends a run: skipped content, and an image, which has no text.
const BOUNDARY_SELECTOR = `${SKIP_SELECTOR},img`;
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
  row: Element | null;
  region: Element | null;
  /** In a closed <details>: kept only when asked for (`keepHidden`). */
  hidden: boolean;
}

// A closed <details> shows only its summary.
function hiddenByDetails(element: Element): boolean {
  for (let details = element.closest('details'); details; details = details.parentElement?.closest('details') ?? null) {
    if ((details as HTMLDetailsElement).open) continue;
    const summary = details.querySelector(':scope > summary');
    if (!summary || !summary.contains(element)) return true;
  }
  return false;
}

function collectSegments(root: Element, rowSelector: string | undefined, rowsOnly: boolean, laidOut = true, keepHidden = false, textSelector?: string): Segment[] {
  const visible = new Map<Element, boolean>();
  // checkVisibility fails a `display: contents` element, which has no box,
  // though its loose text renders in its parent's.
  const laidOutVisible = (element: Element): boolean => {
    if (typeof element.checkVisibility !== 'function' || element.checkVisibility()) return true;
    const parent = element.parentElement;
    return parent !== null && getComputedStyle(element).display === 'contents' && isVisible(parent);
  };
  const isVisible = (element: Element): boolean => {
    let answer = visible.get(element);
    if (answer === undefined) {
      // checkVisibility passes a closed <details> itself, whose loose
      // body text it does not show.
      answer = !hiddenByDetails(element) && (!laidOut || laidOutVisible(element));
      visible.set(element, answer);
    }
    return answer;
  };
  // Text nodes, and the elements that end a run.
  const walker = root.ownerDocument.createTreeWalker(root, NodeFilter.SHOW_TEXT | NodeFilter.SHOW_ELEMENT, {
    acceptNode: (node) => node.nodeType === Node.TEXT_NODE || (node as Element).matches(BOUNDARY_SELECTOR)
      ? NodeFilter.FILTER_ACCEPT
      : NodeFilter.FILTER_SKIP,
  });
  const segments: Segment[] = [];
  let current: Segment | null = null;
  for (let next = walker.nextNode(); next; next = walker.nextNode()) {
    if (next.nodeType !== Node.TEXT_NODE) {
      current = null;
      continue;
    }
    const node = next as Text;
    const parent = node.parentElement;
    if (!parent || node.data.length === 0) continue;
    if (parent.closest(SKIP_SELECTOR)) continue;
    const hidden = !isVisible(parent);
    if (hidden && !keepHidden) continue;
    const row = rowsOnly && rowSelector ? parent.closest(rowSelector) : null;
    if (rowsOnly && (!row || !root.contains(row))) continue;
    const region = textSelector ? parent.closest(textSelector) : null;
    if (textSelector && (!region || !root.contains(region))) continue;
    const block = parent.closest(BLOCK_SELECTOR);
    if (!current || current.block !== block || current.row !== row || current.region !== region || current.hidden !== hidden) {
      current = { nodes: [], starts: [], text: '', block, row, region, hidden };
      segments.push(current);
    }
    current.starts.push(current.text.length);
    current.nodes.push(node);
    current.text += foldFindText(node.data);
  }
  return segments;
}

/**
 * The folded text of each run the highlighter matches within, in document
 * order. `laidOut: false` reads a tree with no layout (a parsed fragment):
 * every element is visible but the body of a closed <details>. With
 * `textSelector`, only the runs inside the elements it selects.
 */
export function findTextSegments(root: Element, laidOut = true, textSelector?: string): string[] {
  return collectSegments(root, undefined, false, laidOut, false, textSelector).map((segment) => segment.text);
}

/**
 * Every run of a tree with no layout (a parsed fragment), in walk order:
 * the runs `findTextSegments(root, false)` gives, and the body of each
 * closed <details> as runs of its own, not shown. Numbered this way a run
 * keeps its place whichever <details> are open.
 */
export function findTextRuns(root: Element): FindRun[] {
  return collectSegments(root, undefined, false, false, true).map((segment) => ({ text: segment.text, shown: !segment.hidden }));
}

function pointAt(segment: Segment, offset: number, end: boolean): [Text, number] {
  let index = segment.starts.length - 1;
  while (index > 0 && (end ? segment.starts[index] >= offset : segment.starts[index] > offset)) index -= 1;
  return [segment.nodes[index], offset - segment.starts[index]];
}

function collectMatches(root: HTMLElement, needle: string, rowSelector: string | undefined, rowKeyAttribute: string | undefined, rowsOnly: boolean, textSelector: string | undefined): FindMatch[] {
  const matches: FindMatch[] = [];
  for (const segment of collectSegments(root, rowSelector, rowsOnly, true, false, textSelector)) {
    let at = segment.text.indexOf(needle);
    if (at < 0) continue;
    const row = segment.row ?? (rowSelector ? segment.nodes[0].parentElement?.closest(rowSelector) : null);
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
  const { root, name, rowSelector, rowKeyAttribute, textSelector } = options;
  const rowsOnly = options.rowsOnly === true;
  const currentName = `${name}-current`;
  const paints = findHighlightsSupported();
  let needle = '';
  let current: FindCurrentSource = null;
  let matches: FindMatch[] = [];
  let observer: MutationObserver | null = null;
  let onToggle: (() => void) | null = null;
  let scheduler: RefreshScheduler | null = null;
  let rulesHeld = false;
  let disposed = false;

  // Indexes of the current matches.
  function currentIndexes(): number[] {
    const now = typeof current === 'function' ? current() : current;
    if (!now) return [];
    if ('match' in now) return now.match >= 0 && now.match < matches.length ? [now.match] : [];
    const { row, occurrence } = now;
    const out: number[] = [];
    let seen = 0;
    for (let index = 0; index < matches.length; index += 1) {
      if (matches[index].rowKey !== row) continue;
      if (occurrence === undefined || seen === occurrence) out.push(index);
      seen += 1;
      if (occurrence !== undefined && seen > occurrence) break;
    }
    return out;
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
    const currentSet = new Set(currentIndexes());
    matches.forEach((match, index) => (currentSet.has(index) ? focused : all).add(match.range));
    CSS.highlights.set(name, all);
    CSS.highlights.set(currentName, focused);
  }

  function recompute(): void {
    if (disposed) return;
    matches = needle ? collectMatches(root, needle, rowSelector, rowKeyAttribute, rowsOnly, textSelector) : [];
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
    // `toggle` does not bubble; a capture listener sees every <details>.
    onToggle = () => pending.request();
    root.addEventListener('toggle', onToggle, true);
    if (paints && !rulesHeld) {
      retainRules(name);
      rulesHeld = true;
    }
  }

  function unfollow(): void {
    observer?.disconnect();
    observer = null;
    if (onToggle) root.removeEventListener('toggle', onToggle, true);
    onToggle = null;
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
    setCurrent(next: FindCurrentSource): void {
      if (disposed) return;
      current = next;
      paint();
    },
    get matches() {
      return matches;
    },
    currentRange(): Range | null {
      const first = currentIndexes()[0];
      return first === undefined ? null : matches[first].range;
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
