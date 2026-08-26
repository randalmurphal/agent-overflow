// The semantic snapshot: what an agent reads INSTEAD of a screenshot
// (§4 of docs/specs/testing-harness.md).
//
// The whole design constraint is that the output is read in a terminal
// and diffed between two moments. So: stable field order, text first,
// numbers rounded to whole pixels, every free-form string capped, and no
// key that changes shape depending on what is on screen.
//
// It reads the REAL DOM through the attributes the components already
// declare, never through class names or renderer internals:
//
//   [data-pane-id] / -kind / -focused      panes/PaneHost.svelte
//   [data-ui-surface="chat"][data-thread-id]  chat/ChatView.svelte
//   [data-testid="message-timeline-scroll"]   chat/MessageTimeline.svelte
//   [data-row-index]                          chat/MessageTimeline.svelte
//   [data-item-id] / -kind / -role / -status  chat/TimelineLeaf.svelte
//   [data-testid="indicator"][data-state]     chat/Indicator.svelte
//   [role="dialog"], [data-popover]           primitives/{Modal,OverlayShell,Popover}
//
// Only the three item-* attributes were ADDED for this; everything else
// already existed. Prefer extending that list over pattern-matching a
// class, and keep additions on the element that owns the concept.
//
// "Visible rows" means the rows the virtualizer has MOUNTED, each flagged
// with whether its box currently intersects the scroller. That is the
// honest reading for a virtualized list: the mounted window is what the
// DOM contains, the intersecting subset is what a human would see, and a
// reader debugging a windowing bug needs both. Filtering to the
// intersecting subset here would have hidden the overscan that most
// timeline bugs live in.
//
// Layout reads only. Nothing in this file writes to the DOM, and it must
// stay that way: a probe that mutates is a probe that changes the thing
// it is measuring.

/** Whole-pixel box. Fractional CSS pixels are noise in a diff. */
export interface HarnessRect {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface HarnessRow {
  itemId: string;
  kind: string;
  role: string;
  status: string;
  streaming: boolean;
  badge: string;
  rowIndex: number;
  inViewport: boolean;
  rect: HarnessRect;
  textHead: string;
}

export interface HarnessScroll {
  top: number;
  height: number;
  client: number;
  distanceFromBottom: number;
  atBottom: boolean;
}

export interface HarnessPane {
  paneId: string;
  paneKind: string;
  focused: boolean;
  threadId: string;
  rect: HarnessRect;
  scroll: HarnessScroll | null;
  mountedRows: number;
  rows: HarnessRow[];
}

export interface HarnessOverlay {
  name: string;
  kind: 'dialog' | 'popover';
  rect: HarnessRect;
}

export interface HarnessViewport {
  v: 1;
  settled: boolean;
  sinceMutationMs: number;
  activeThreadId: string;
  domNodes: number;
  panes: HarnessPane[];
  overlays: HarnessOverlay[];
}

export interface ViewportOptions {
  /** How long the document must have been mutation-free to read settled. */
  settledMs?: number;
  /** Milliseconds since the last observed DOM mutation. */
  sinceMutationMs: number;
  /** Cap for each row's textHead. Absent or 0 means DEFAULT_TEXT_HEAD. */
  textHead?: number;
}

export const DEFAULT_TEXT_HEAD = 120;
export const DEFAULT_SETTLED_MS = 300;
export const DEFAULT_ELEMENT_TEXT_CAP = 500;
/** A pane cannot plausibly mount more than this; a runaway is a finding, not a dump. */
export const MAX_ROWS_PER_PANE = 400;

// ---------------------------------------------------------------------------
// Pure helpers. Exported for their own unit tests: these are the parts
// that decide what a reader sees, and they need no browser to check.

/**
 * First `cap` characters of a single-line rendering of `raw`. All
 * whitespace runs (including the newlines a markdown row is full of)
 * collapse to one space so a row occupies one terminal line, and the
 * truncation marker is a single ellipsis character so the cap is exact.
 *
 * Counting is by CODE POINT, not UTF-16 unit: cutting a surrogate pair in
 * half produces a lone surrogate, which JSON.stringify happily emits and
 * some readers render as a replacement char that looks like real content.
 *
 * The point array is built over a BOUNDED prefix, never the whole string.
 * A row's textContent is the row's entire rendered text — a long tool
 * output or a markdown answer is tens of kilobytes — and `Array.from` over
 * it allocates one string per character to keep ~120 of them. A code point
 * is at most two UTF-16 units, so `cap * 2` units always contain at least
 * `cap` points; the slice can only ever orphan a surrogate PAST the cap,
 * which the point slice then drops.
 */
export function textHead(raw: string, cap = DEFAULT_TEXT_HEAD): string {
  if (cap <= 0) return '';
  const collapsed = raw.replace(/\s+/g, ' ').trim();
  const bounded = collapsed.length > cap * 2 ? collapsed.slice(0, cap * 2) : collapsed;
  const points = Array.from(bounded);
  if (points.length <= cap && bounded.length === collapsed.length) return collapsed;
  return points.slice(0, cap).join('') + '…';
}

export function roundRect(rect: {
  left: number;
  top: number;
  width: number;
  height: number;
}): HarnessRect {
  return {
    x: Math.round(rect.left),
    y: Math.round(rect.top),
    w: Math.round(rect.width),
    h: Math.round(rect.height),
  };
}

/**
 * Vertical intersection only. A timeline row always spans the scroller's
 * width, so a horizontal test would answer the same question twice — and
 * would go WRONG during a pane-divider drag, where a row is briefly wider
 * than the box it is settling into.
 */
export function rectsOverlapVertically(row: HarnessRect, viewport: HarnessRect): boolean {
  return row.y < viewport.y + viewport.h && row.y + row.h > viewport.y;
}

/**
 * A scroll position is "at bottom" within one CSS pixel. Sub-pixel
 * scrollTop is normal on a fractional-DPI display, so an exact compare
 * reports "not at bottom" on a pane that visibly is.
 */
export function readScroll(el: {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}): HarnessScroll {
  const distance = el.scrollHeight - el.clientHeight - el.scrollTop;
  return {
    top: Math.round(el.scrollTop),
    height: Math.round(el.scrollHeight),
    client: Math.round(el.clientHeight),
    distanceFromBottom: Math.round(distance),
    atBottom: distance <= 1,
  };
}

// ---------------------------------------------------------------------------
// DOM walk.

function attr(el: Element, name: string): string {
  return el.getAttribute(name) ?? '';
}

function rectOf(el: Element): HarnessRect {
  return roundRect(el.getBoundingClientRect());
}

function readRow(rowEl: Element, viewport: HarnessRect | null, textCap: number): HarnessRow | null {
  const leaf = rowEl.querySelector('[data-item-id]');
  if (!leaf) return null;
  const rect = rectOf(leaf);
  const badge = leaf.querySelector('[data-testid="indicator"]');
  const status = attr(leaf, 'data-item-status');
  return {
    itemId: attr(leaf, 'data-item-id'),
    kind: attr(leaf, 'data-item-kind'),
    role: attr(leaf, 'data-item-role'),
    status,
    streaming: status === 'streaming' || status === 'running',
    badge: badge ? attr(badge, 'data-state') : '',
    rowIndex: Number.parseInt(attr(rowEl, 'data-row-index'), 10) || 0,
    inViewport: viewport === null ? false : rectsOverlapVertically(rect, viewport),
    rect,
    textHead: textHead(leaf.textContent ?? '', textCap),
  };
}

function readPane(paneEl: Element, textCap: number): HarnessPane {
  const chat = paneEl.querySelector('[data-ui-surface="chat"]');
  const scroller = paneEl.querySelector('[data-testid="message-timeline-scroll"]');
  const scroll = scroller ? readScroll(scroller as HTMLElement) : null;
  const viewport = scroller ? rectOf(scroller) : null;
  const rowEls = paneEl.querySelectorAll('[data-row-index]');
  const rows: HarnessRow[] = [];
  for (const rowEl of rowEls) {
    if (rows.length >= MAX_ROWS_PER_PANE) break;
    const row = readRow(rowEl, viewport, textCap);
    if (row) rows.push(row);
  }
  return {
    paneId: attr(paneEl, 'data-pane-id'),
    paneKind: attr(paneEl, 'data-pane-kind'),
    focused: attr(paneEl, 'data-pane-focused') === 'true',
    threadId: chat ? attr(chat, 'data-thread-id') : '',
    rect: rectOf(paneEl),
    scroll,
    mountedRows: rowEls.length,
    rows,
  };
}

/**
 * A dialog's own accessible name, falling back to its heading text and
 * then its testid. Overlays are identified by NAME because that is what
 * the person reading the snapshot typed into the UI spec; a generated id
 * would diff cleanly and mean nothing.
 */
function overlayName(el: Element): string {
  const aria = attr(el, 'aria-label');
  if (aria) return textHead(aria, 60);
  const labelledBy = attr(el, 'aria-labelledby');
  if (labelledBy) {
    const label = el.ownerDocument.getElementById(labelledBy);
    if (label?.textContent) return textHead(label.textContent, 60);
  }
  const testId = attr(el, 'data-testid');
  if (testId) return testId;
  return el.tagName.toLowerCase();
}

function readOverlays(doc: Document): HarnessOverlay[] {
  const overlays: HarnessOverlay[] = [];
  for (const el of doc.querySelectorAll('[role="dialog"]')) {
    overlays.push({ name: overlayName(el), kind: 'dialog', rect: rectOf(el) });
  }
  for (const el of doc.querySelectorAll('[data-popover]')) {
    overlays.push({ name: overlayName(el), kind: 'popover', rect: rectOf(el) });
  }
  return overlays;
}

/**
 * The whole visible surface, in document order. `settled` is derived from
 * a mutation timestamp the CALLER owns (bridge.ts holds the observer):
 * this function must stay a pure read of the current DOM, so that the
 * same snapshot code can be driven from a test with a synthetic clock.
 */
export function readViewport(doc: Document, opts: ViewportOptions): HarnessViewport {
  const settledMs = opts.settledMs ?? DEFAULT_SETTLED_MS;
  // A caller asking for more (or less) text per row is asking about EVERY
  // row, so the cap rides the walk down rather than being re-read at the
  // leaf. 0 and absent both mean "the default" — the CLI's `--text-head`
  // is an int flag whose unset value is 0.
  const textCap = opts.textHead && opts.textHead > 0 ? opts.textHead : DEFAULT_TEXT_HEAD;
  const panes: HarnessPane[] = [];
  for (const paneEl of doc.querySelectorAll('[data-pane-id]')) {
    panes.push(readPane(paneEl, textCap));
  }
  const focused = panes.find((pane) => pane.focused) ?? panes[0];
  return {
    v: 1,
    settled: opts.sinceMutationMs >= settledMs,
    sinceMutationMs: Math.round(opts.sinceMutationMs),
    activeThreadId: focused?.threadId ?? '',
    domNodes: doc.getElementsByTagName('*').length,
    panes,
    overlays: readOverlays(doc),
  };
}

// ---------------------------------------------------------------------------
// element query.

export interface HarnessElementMatch {
  tag: string;
  rect: HarnessRect;
  visible: boolean;
  clipped: boolean;
  text: string;
  role: string;
  ariaLabel: string;
  testId: string;
}

export interface HarnessElement {
  v: 1;
  selector: string;
  count: number;
  first: HarnessElementMatch | null;
}

/**
 * Nearest ancestor that can actually clip. `overflow: visible` cannot, so
 * walking to the first ancestor with a different scrollHeight would pick
 * up any wrapper that happens to be shorter than its content.
 */
function clippingAncestor(el: Element): Element | null {
  let node = el.parentElement;
  while (node) {
    const style = node.ownerDocument.defaultView?.getComputedStyle(node);
    const overflow = `${style?.overflowX ?? ''} ${style?.overflowY ?? ''}`;
    if (/auto|scroll|hidden|clip/.test(overflow)) return node;
    node = node.parentElement;
  }
  return null;
}

function isVisible(el: Element, rect: HarnessRect): boolean {
  const style = el.ownerDocument.defaultView?.getComputedStyle(el);
  if (!style) return rect.w > 0 && rect.h > 0;
  if (style.display === 'none' || style.visibility === 'hidden') return false;
  if (Number.parseFloat(style.opacity || '1') === 0) return false;
  return rect.w > 0 && rect.h > 0;
}

export function readElement(
  doc: Document,
  selector: string,
  textCap = DEFAULT_ELEMENT_TEXT_CAP,
): HarnessElement {
  // An invalid selector is the caller's typo, and it must read as one
  // rather than as "no such element" — those get debugged very
  // differently.
  const matches = doc.querySelectorAll(selector);
  const el = matches[0];
  if (!el) return { v: 1, selector, count: matches.length, first: null };
  const rect = rectOf(el);
  const clipper = clippingAncestor(el);
  return {
    v: 1,
    selector,
    count: matches.length,
    first: {
      tag: el.tagName.toLowerCase(),
      rect,
      visible: isVisible(el, rect),
      clipped: clipper ? !rectsOverlapVertically(rect, rectOf(clipper)) : false,
      text: textHead(el.textContent ?? '', textCap),
      role: attr(el, 'role'),
      ariaLabel: attr(el, 'aria-label'),
      testId: attr(el, 'data-testid'),
    },
  };
}
