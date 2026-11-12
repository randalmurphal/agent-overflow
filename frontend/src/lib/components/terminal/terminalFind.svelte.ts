// The find provider of one xterm (stores/paneFind.svelte.ts). It scans the
// buffer itself (terminalFindScan.ts) and paints matches on the surface's
// layer over the grid (terminalFindLayer.ts), never with xterm decorations.
// It never reads or writes the terminal selection: that is the reader's,
// for copying, and a drag in progress stays theirs.
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
// - Each painted row has a marker, so the paint follows its rows as output
//   moves them, until the re-search.
// - Every buffer change while a query is set (`onBufferChange`) re-searches
//   once changes pause, and at least once a second under sustained output.
//   Until then the result is pending, so the bar marks the count it shows
//   as updating. A re-search never scrolls; a query change and a step bring
//   the active match into view.
// - A switch to or from a full-screen program (the alternate screen) drops
//   the paint at once: the layer draws by row on whichever screen is active.
// - A query change and a step reveal the active match clear of the cells
//   the find bar covers (`obscured`), as far as scrolling can, and so does
//   a change to the bar's size (`onObscuredChange`) for a match it covers:
//   the bar mounts after a reopen's search.

import type { IDisposable, IMarker, Terminal } from '@xterm/xterm';
import {
  paneFindRegionKey,
  registerPaneFindProvider,
  type PaneFindProvider,
  type PaneFindResult,
} from '../../stores/paneFind.svelte';
import { getResolvedTheme } from '../../stores/themeMode.svelte';
import { getXtermFindColors, type TerminalFindColors } from './terminalTheme';
import { scanTerminalBuffer, terminalLineAt, type TerminalMatch } from './terminalFindScan';
import type { TerminalFindLayer, TerminalFindRun } from './terminalFindLayer';
import type { TerminalObscured } from './terminalXterm';
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
/** The slice of the xterm the provider uses. No selection member: find
 * leaves the selection alone; no decoration member: find paints on its
 * own layer. */
export type TerminalFindTerminal = Pick<Terminal, 'buffer' | 'rows' | 'registerMarker' | 'scrollLines'>;

/** What a surface's built terminal hands its find (`BuiltTerminal`). */
export interface TerminalFindSource {
  term: TerminalFindTerminal;
  /** Every change to the searched buffer. */
  onBufferChange(listener: () => void): IDisposable;
  /** Draws the matches over the grid. */
  findLayer: TerminalFindLayer;
  /** The viewport cells the find bar covers. */
  obscured?(): TerminalObscured;
  /** Fires when the find bar's size changes. */
  onObscuredChange?(listener: () => void): IDisposable;
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
    findLayer: source.findLayer,
    obscured: source.obscured,
    onObscuredChange: source.onObscuredChange,
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
  // One marker per painted row.
  let rowMarkers = new Map<number, IMarker>();
  // Each match's runs, by index.
  let runs: TerminalFindRun[][] = [];
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

  function runsOf(match: TerminalMatch): TerminalFindRun[] {
    return match.cells.map((cells) => {
      let marker = rowMarkers.get(cells.row);
      if (!marker) {
        marker = markerAt(cells.row);
        rowMarkers.set(cells.row, marker);
      }
      return { marker, x: cells.x, width: cells.width };
    });
  }

  function unpaint(): void {
    options.findLayer.clear();
    for (const marker of rowMarkers.values()) marker.dispose();
    rowMarkers = new Map();
    runs = [];
  }

  function paint(): void {
    options.findLayer.paint(runs, current, colors);
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
    runs = matches.map(runsOf);
    count = matches.length;
    pending = false;
  }

  // Scrolls the active match into view below the find bar's rows when it is
  // out of view or (`coveredOnly`) when the bar covers any of its cells.
  function reveal(coveredOnly = false): void {
    const live = (runs[current] ?? []).filter((run) => !run.marker.isDisposed);
    if (live.length === 0) return;
    const top = term.buffer.active.viewportY;
    const bar = options.obscured?.() ?? { rows: 0, from: 0, to: -1 };
    const covered = Math.min(bar.rows, term.rows - 1);
    const row = live[0].marker.line;
    const outside = row < top || row >= top + term.rows;
    const hidden = live.some((run) => run.marker.line - top >= 0 && run.marker.line - top < covered
      && run.x <= bar.to && run.x + run.width - 1 >= bar.from);
    if (hidden || (outside && !coveredOnly)) term.scrollLines(row - top - Math.floor((term.rows + covered) / 2));
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
      paint();
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
      paint();
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
    // The layer draws by row on whichever screen is active, so a switch to
    // or from a full-screen program would show the other screen's matches
    // on unrelated text until the re-search.
    term.buffer.onBufferChange(() => {
      if (!query || error !== null) return;
      unpaint();
      pending = true;
      refresher.request();
    }),
  ];
  // The bar mounts after the search a reopen runs, and changes size with
  // the layout: a match it now covers moves clear of it.
  if (options.onObscuredChange) {
    listeners.push(options.onObscuredChange(() => {
      if (query && error === null) reveal(true);
    }));
  }

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
        paint();
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
