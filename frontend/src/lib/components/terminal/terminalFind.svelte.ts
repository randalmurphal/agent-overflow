// The find provider of one xterm (stores/paneFind.svelte.ts). It scans the
// buffer itself (terminalFindScan.ts) and paints matches with xterm
// decorations. It never reads or writes the terminal selection: that is
// the reader's, for copying, and a drag in progress stays theirs.
//
// - The match rule is the app's (utils/findMatch.ts) over each logical line.
//   Each query change starts at the newest match (the bottom of the buffer);
//   `next` steps down toward newer output, `previous` up, and both wrap.
// - The newest TERMINAL_FIND_MATCH_LIMIT matches are counted and painted; a
//   count at it is capped.
// - The active match is remembered by a marker on the first row of its
//   logical line plus its offset in that line's text, and by a marker on the
//   first row of the next line plus its distance from the line's end.
//   Reflow keeps both; trimming that takes the line's first rows leaves the
//   second. A re-search after any change finds the same match, or the
//   nearest one after it when it is gone (the newest when none follows).
// - Find owns every decoration it registers and disposes it before its
//   marker: xterm 6 clears a marker's row before telling its decorations,
//   and a decoration whose row is out of order then stays registered for
//   the terminal's life. A reflow disposes markers out of order, so the
//   paint is dropped before a column-changing resize (`onBeforeReflow`), and
//   re-searched after.
// - Every buffer change while a query is set (`onBufferChange`) re-searches
//   once changes pause, and at least once a second under sustained output.
//   Until then the result is pending, so the bar marks the count it shows
//   as updating. A re-search never scrolls; a query change and a step bring
//   the active match into view.
// - A switch to or from a full-screen program (the alternate screen) drops
//   the paint at once: xterm paints a decoration by its row on whichever
//   screen is active.
// - A step reveals the active match below the rows the find bar covers
//   (`obscuredRows`), as far as scrolling can.

import type { IDecoration, IDisposable, IMarker, Terminal } from '@xterm/xterm';
import {
  paneFindRegionKey,
  registerPaneFindProvider,
  type PaneFindProvider,
  type PaneFindResult,
} from '../../stores/paneFind.svelte';
import { getResolvedTheme } from '../../stores/themeMode.svelte';
import { getXtermFindColors, type TerminalFindColors } from './terminalTheme';
import { scanTerminalBuffer, terminalLineAt, type TerminalMatch, type TerminalMatchCells } from './terminalFindScan';
import { findNeedle } from '../../utils/findMatch';
import { createRefreshScheduler } from '../../utils/refreshScheduler';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
import { errString } from '../../utils/errors';

/** The find key of one terminal tab in a pane. Scoped under the pane, so
 * the store drops it when the pane leaves the layout. */
export function terminalFindKey(paneId: string, terminalID: string): string {
  return paneFindRegionKey(paneId, `terminal:${terminalID}`);
}

/** Matches counted and painted; a count at it is capped. */
export const TERMINAL_FIND_MATCH_LIMIT = 1000;
/** Quiet time after a buffer change before the re-search. */
export const TERMINAL_FIND_REFRESH_DELAY_MS = 200;
/** The longest sustained output defers a re-search. */
export const TERMINAL_FIND_REFRESH_MAX_WAIT_MS = 1000;
/** The find bar's place over a terminal surface's grid: top right on
 * desktop, the full width under compact. Empty while the bar is closed, so
 * it takes no pointer events then. */
export const TERMINAL_FIND_OVERLAY_CLASS = 'absolute top-1 right-2 z-10 w-[min(26rem,calc(100%-1rem))] compact:left-1 compact:right-1 compact:w-auto';
/** Class names on the rendered match decorations. */
export const TERMINAL_FIND_MATCH_CLASS = 'terminal-find-match';
export const TERMINAL_FIND_ACTIVE_CLASS = 'terminal-find-active';

/** The slice of the xterm the provider uses. No selection member: find
 * leaves the selection alone. */
export type TerminalFindTerminal = Pick<Terminal, 'buffer' | 'rows' | 'registerMarker' | 'registerDecoration' | 'scrollLines'>;

/** What a surface's built terminal hands its find (`BuiltTerminal`). */
export interface TerminalFindSource {
  term: TerminalFindTerminal;
  /** Every change to the searched buffer. */
  onBufferChange(listener: () => void): IDisposable;
  /** Just before a column-changing resize, whose reflow disposes markers. */
  onBeforeReflow(listener: () => void): IDisposable;
  /** Rows at the top of the viewport the find bar covers. */
  obscuredRows?(): number;
}

export interface TerminalFindOptions extends TerminalFindSource {
  /** The match grounds for the current theme. */
  colors(): TerminalFindColors;
}

export interface TerminalFind extends PaneFindProvider {
  next(): void;
  previous(): void;
  /** Repaints the matches with the current `colors()`. */
  restyle(): void;
  /** Clears the matches and drops the timers and listeners. */
  dispose(): void;
}

/** A terminal surface's registered find. */
export interface TerminalFindRegistration {
  /** Repaints the matches after a theme change. */
  restyle(): void;
  /** Unregisters, clears the matches and drops the listeners. Call before
   * the xterm is disposed. */
  release(): void;
}

/**
 * Builds the find of a surface's xterm, painted from the app theme, and
 * registers it under `key` (a layout pane id or `terminalFindKey`).
 * Register once the replay is parsed (its write's callback), so an open
 * bar's query searches the restored buffer and brings its match into view.
 */
export function registerTerminalFind(key: string, source: TerminalFindSource): TerminalFindRegistration {
  const find = createTerminalFind({
    term: source.term,
    onBufferChange: source.onBufferChange,
    onBeforeReflow: source.onBeforeReflow,
    obscuredRows: source.obscuredRows,
    colors: () => getXtermFindColors(getResolvedTheme()),
  });
  const unregister = registerPaneFindProvider(key, find);
  return {
    restyle: () => find.restyle(),
    release() {
      unregister();
      find.dispose();
    },
  };
}

interface Anchor {
  /** On the first row of the match's logical line, with the match's
   * offset in the line's text. */
  line: IMarker;
  offset: number;
  /** On the first row of the next logical line, with the match's distance
   * from the end of its line's text; null for the buffer's last line. */
  next: IMarker | null;
  tail: number;
  buffer: string;
}

export function createTerminalFind(options: TerminalFindOptions): TerminalFind {
  const { term } = options;
  let query = '';
  let needle = '';
  let count = $state(0);
  let current = $state(-1);
  let pending = $state(false);
  let error = $state<string | null>(null);
  let matches: TerminalMatch[] = [];
  // One marker per painted row, disposed after its decorations.
  let rowMarkers = new Map<number, IMarker>();
  // Each match's paint, by index; the active match has only its own.
  let matchPaint: IDecoration[][] = [];
  let active: IDecoration[] = [];
  let activeAt = -1;
  let colors: TerminalFindColors = {};
  let anchor: Anchor | null = null;

  const result: PaneFindResult = {
    get count() { return count; },
    get current() { return current; },
    get pending() { return pending; },
    get capped() { return count >= TERMINAL_FIND_MATCH_LIMIT; },
    // The last count stays beside the spinner while output streams, rather
    // than blinking out on every write.
    progressive: true,
    get error() { return error; },
  };

  // A marker on buffer row `row` of the active buffer.
  function markerAt(row: number): IMarker {
    const buffer = term.buffer.active;
    return term.registerMarker(row - buffer.baseY - buffer.cursorY);
  }

  function decorate(cells: TerminalMatchCells, background: string | undefined, className: string): IDecoration | undefined {
    let marker = rowMarkers.get(cells.row);
    if (!marker) {
      marker = markerAt(cells.row);
      rowMarkers.set(cells.row, marker);
    }
    const decoration = term.registerDecoration({ marker, x: cells.x, width: cells.width, backgroundColor: background, layer: 'bottom' });
    decoration?.onRender((element) => element.classList.add(className));
    return decoration;
  }

  function paint(cells: readonly TerminalMatchCells[], background: string | undefined, className: string): IDecoration[] {
    const painted: IDecoration[] = [];
    for (const part of cells) {
      const decoration = decorate(part, background, className);
      if (decoration) painted.push(decoration);
    }
    return painted;
  }

  function unpaint(): void {
    for (const decorations of matchPaint) for (const decoration of decorations) decoration.dispose();
    for (const decoration of active) decoration.dispose();
    for (const marker of rowMarkers.values()) marker.dispose();
    rowMarkers = new Map();
    matchPaint = [];
    active = [];
    activeAt = -1;
  }

  // xterm lets a later decoration on a row lose to an earlier one, so the
  // active match never sits over its own match paint: the two swap.
  function paintActive(): void {
    for (const decoration of active) decoration.dispose();
    active = [];
    if (matches[activeAt]) matchPaint[activeAt] = paint(matches[activeAt].cells, colors.match, TERMINAL_FIND_MATCH_CLASS);
    activeAt = -1;
    const match = matches[current];
    if (!match) return;
    for (const decoration of matchPaint[current] ?? []) decoration.dispose();
    matchPaint[current] = [];
    active = paint(match.cells, colors.current, TERMINAL_FIND_ACTIVE_CLASS);
    activeAt = current;
  }

  function dropAnchor(): void {
    anchor?.line.dispose();
    anchor?.next?.dispose();
    anchor = null;
  }

  function setAnchor(): void {
    const match = matches[current];
    if (!match) return;
    dropAnchor();
    const buffer = term.buffer.active;
    const line = terminalLineAt(buffer, match.line);
    anchor = {
      line: markerAt(match.line),
      offset: match.offset,
      next: line.end < buffer.length ? markerAt(line.end) : null,
      tail: line.length - match.offset,
      buffer: buffer.type,
    };
  }

  // Where the anchored match is now: its line's first row and its offset
  // there, or null when its text is gone.
  function anchorPlace(held: Anchor): { line: number; offset: number } | null {
    if (!held.line.isDisposed) return { line: held.line.line, offset: held.offset };
    if (!held.next || held.next.isDisposed || held.next.line < 1) return null;
    // Trimming took the line's first rows: count back from its end.
    const line = terminalLineAt(term.buffer.active, held.next.line - 1);
    const offset = line.length - held.tail;
    return offset >= 0 ? { line: line.start, offset } : null;
  }

  // The active match after a re-scan: the anchored one, else the nearest
  // after it, else the newest.
  function anchored(): number {
    if (matches.length === 0) return -1;
    if (!anchor || anchor.buffer !== term.buffer.active.type) return matches.length - 1;
    const place = anchorPlace(anchor);
    if (!place) return 0;
    const at = matches.findIndex((match) => match.line > place.line || (match.line === place.line && match.offset >= place.offset));
    return at >= 0 ? at : matches.length - 1;
  }

  function scan(): void {
    unpaint();
    colors = options.colors();
    matches = scanTerminalBuffer(term.buffer.active, needle, TERMINAL_FIND_MATCH_LIMIT);
    matchPaint = matches.map((match) => paint(match.cells, colors.match, TERMINAL_FIND_MATCH_CLASS));
    count = matches.length;
    pending = false;
  }

  function reveal(): void {
    const row = matches[current]?.cells[0]?.row;
    if (row === undefined) return;
    const top = term.buffer.active.viewportY;
    const covered = Math.min(Math.max(0, options.obscuredRows?.() ?? 0), term.rows - 1);
    if (row < top + covered || row >= top + term.rows) term.scrollLines(row - top - Math.floor((term.rows + covered) / 2));
  }

  // A throw is the bar's error until a later call succeeds.
  function guarded(work: () => void): void {
    try {
      work();
      error = null;
    } catch (err) {
      unpaint();
      matches = [];
      count = 0;
      current = -1;
      pending = false;
      error = errString(err);
      console.warn('terminal find failed', error);
      reportFrontendDiagnostic('terminal find failed', error);
    }
  }

  function refresh(): void {
    if (!query || error !== null) return;
    guarded(() => {
      scan();
      current = anchored();
      setAnchor();
      paintActive();
    });
  }

  function step(delta: number): void {
    if (!query) return;
    guarded(() => {
      // Rows move under output; step over what the buffer holds now.
      if (pending || error !== null) {
        scan();
        current = anchored();
      }
      if (matches.length === 0) return;
      current = current < 0 ? (delta > 0 ? 0 : matches.length - 1) : (current + delta + matches.length) % matches.length;
      setAnchor();
      paintActive();
      reveal();
    });
  }

  const refresher = createRefreshScheduler({
    name: 'terminal find',
    delayMs: TERMINAL_FIND_REFRESH_DELAY_MS,
    maxWaitMs: TERMINAL_FIND_REFRESH_MAX_WAIT_MS,
    run: () => refresh(),
  });

  const listeners: IDisposable[] = [
    options.onBufferChange(() => {
      if (!query || error !== null) return;
      pending = true;
      refresher.request();
    }),
    // xterm paints a decoration by its marker's row on whichever screen is
    // active, so a switch to or from a full-screen program would show the
    // other screen's matches on unrelated text until the re-search.
    term.buffer.onBufferChange(() => {
      if (!query || error !== null) return;
      unpaint();
      pending = true;
      refresher.request();
    }),
    options.onBeforeReflow(() => {
      if (!query || error !== null) return;
      unpaint();
      pending = true;
      refresher.request();
    }),
  ];

  function clear(): void {
    query = '';
    needle = '';
    refresher.reset();
    unpaint();
    dropAnchor();
    matches = [];
    count = 0;
    current = -1;
    pending = false;
    error = null;
  }

  return {
    noun: 'terminal',
    result,
    search(raw: string): void {
      const next = raw.trim();
      if (next === query) return;
      if (!next) {
        clear();
        return;
      }
      query = next;
      needle = findNeedle(next);
      refresher.reset();
      dropAnchor();
      guarded(() => {
        scan();
        current = matches.length - 1;
        setAnchor();
        paintActive();
        reveal();
      });
    },
    next: () => step(1),
    previous: () => step(-1),
    restyle(): void {
      refresher.reset();
      refresh();
    },
    clear,
    dispose(): void {
      clear();
      refresher.dispose();
      for (const listener of listeners) listener.dispose();
      listeners.length = 0;
    },
  };
}
