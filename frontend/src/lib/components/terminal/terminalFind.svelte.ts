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
//   logical line plus its offset in that line's text. Markers follow reflow
//   and scrollback trimming, so a re-search after any change finds the same
//   match, or the nearest one after it when it is gone (the newest when none
//   follows).
// - Every buffer change while a query is set (`onBufferChange`) re-searches
//   once changes pause, and at least once a second under sustained output.
//   Until then the result is pending, so the bar marks the count it shows
//   as updating. A re-search never scrolls; a query change and a step bring
//   the active match into view.

import type { IDecoration, IDisposable, IMarker, Terminal } from '@xterm/xterm';
import {
  paneFindRegionKey,
  registerPaneFindProvider,
  type PaneFindProvider,
  type PaneFindResult,
} from '../../stores/paneFind.svelte';
import { getResolvedTheme } from '../../stores/themeMode.svelte';
import { getXtermFindColors, type TerminalFindColors } from './terminalTheme';
import { scanTerminalBuffer, type TerminalMatch, type TerminalMatchCells } from './terminalFindScan';
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
  marker: IMarker;
  offset: number;
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
  // One marker per painted row; disposing it disposes its decorations.
  let rowMarkers = new Map<number, IMarker>();
  let active: IDecoration[] = [];
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

  function unpaintActive(): void {
    for (const decoration of active) decoration.dispose();
    active = [];
  }

  function unpaint(): void {
    unpaintActive();
    for (const marker of rowMarkers.values()) marker.dispose();
    rowMarkers = new Map();
  }

  function paintActive(): void {
    unpaintActive();
    for (const cells of matches[current]?.cells ?? []) {
      const decoration = decorate(cells, colors.current, TERMINAL_FIND_ACTIVE_CLASS);
      if (decoration) active.push(decoration);
    }
  }

  function setAnchor(): void {
    const match = matches[current];
    if (!match) return;
    anchor?.marker.dispose();
    anchor = { marker: markerAt(match.line), offset: match.offset, buffer: term.buffer.active.type };
  }

  // The active match after a re-scan: the anchored one, else the nearest
  // after it, else the newest.
  function anchored(): number {
    if (matches.length === 0) return -1;
    if (!anchor || anchor.buffer !== term.buffer.active.type) return matches.length - 1;
    if (anchor.marker.isDisposed) return 0;
    const { line } = anchor.marker;
    const { offset } = anchor;
    const at = matches.findIndex((match) => match.line > line || (match.line === line && match.offset >= offset));
    return at >= 0 ? at : matches.length - 1;
  }

  function scan(): void {
    unpaint();
    colors = options.colors();
    matches = scanTerminalBuffer(term.buffer.active, needle, TERMINAL_FIND_MATCH_LIMIT);
    for (const match of matches) {
      for (const cells of match.cells) decorate(cells, colors.match, TERMINAL_FIND_MATCH_CLASS);
    }
    count = matches.length;
    pending = false;
  }

  function reveal(): void {
    const row = matches[current]?.cells[0]?.row;
    if (row === undefined) return;
    const top = term.buffer.active.viewportY;
    if (row < top || row >= top + term.rows) term.scrollLines(row - top - Math.floor(term.rows / 2));
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
  ];

  function clear(): void {
    query = '';
    needle = '';
    refresher.reset();
    unpaint();
    anchor?.marker.dispose();
    anchor = null;
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
      anchor?.marker.dispose();
      anchor = null;
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
