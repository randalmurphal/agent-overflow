// The terminal find provider over a scripted buffer of plain rows: what it
// counts, paints and scrolls, when it re-searches, and how it reaches the
// pane find store. Real buffers (wraps, wide characters, reflow, trimming,
// selection) run in terminalFind.browser.test.ts against a real xterm.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { IDisposable } from '@xterm/xterm';
import {
  TERMINAL_FIND_MATCH_LIMIT,
  TERMINAL_FIND_REFRESH_DELAY_MS,
  TERMINAL_FIND_REFRESH_MAX_WAIT_MS,
  createTerminalFind,
  registerTerminalFind,
  terminalFindKey,
  type TerminalFindTerminal,
} from './terminalFind.svelte';
import type { TerminalFindColors } from './terminalTheme';
import {
  closePaneFind,
  getPaneFindProvider,
  isPaneFindOpen,
  openPaneFind,
  paneOfFindKey,
  resetPaneFindForTest,
  setPaneFindQuery,
  stepPaneFind,
} from '../../stores/paneFind.svelte';

const COLS = 80;

interface Painted {
  row: number;
  x: number;
  width: number;
  background?: string;
  disposed: boolean;
}

/** A terminal of one-cell characters, `rows` high, from the top. */
function fakeTerminal(lines: string[], rows = 24) {
  const bufferListeners = new Set<() => void>();
  const screenListeners = new Set<() => void>();
  const rewriteListeners = new Set<() => void>();
  const painted: Painted[] = [];
  const scrolls: number[] = [];
  let failure: Error | null = null;
  const cell = {
    chars: '',
    getWidth: () => 1,
    getChars() { return this.chars; },
  };
  const line = (text: string) => ({
    isWrapped: false,
    length: COLS,
    getCell(x: number) {
      cell.chars = text[x] ?? '';
      return cell;
    },
    translateToString(trim?: boolean, start = 0, end = COLS) {
      const slice = text.padEnd(COLS).slice(start, end);
      return trim ? slice.trimEnd() : slice;
    },
  });
  const term = {
    rows,
    buffer: {
      active: {
        type: 'normal',
        get length() { return lines.length; },
        baseY: 0,
        cursorY: 0,
        viewportY: 0,
        getNullCell: () => cell,
        getLine: (row: number) => (row < lines.length ? line(lines[row]) : undefined),
      },
      onBufferChange(listener: () => void): IDisposable {
        screenListeners.add(listener);
        return { dispose: () => screenListeners.delete(listener) };
      },
    },
    registerMarker(offset: number) {
      const own: Painted[] = [];
      return {
        line: offset,
        isDisposed: false,
        own,
        dispose() {
          this.isDisposed = true;
          for (const decoration of own) decoration.disposed = true;
        },
      };
    },
    registerDecoration(options: { marker: { line: number; own: Painted[] }; x: number; width: number; backgroundColor?: string }) {
      if (failure) throw failure;
      const decoration: Painted = { row: options.marker.line, x: options.x, width: options.width, background: options.backgroundColor, disposed: false };
      painted.push(decoration);
      options.marker.own.push(decoration);
      return { onRender: () => ({ dispose() {} }), dispose: () => { decoration.disposed = true; } };
    },
    scrollLines(amount: number) {
      scrolls.push(amount);
      this.buffer.active.viewportY += amount;
    },
  };
  return {
    term: term as unknown as TerminalFindTerminal,
    lines,
    scrolls,
    source: {
      term: term as unknown as TerminalFindTerminal,
      onBufferChange(listener: () => void): IDisposable {
        bufferListeners.add(listener);
        return { dispose: () => bufferListeners.delete(listener) };
      },
      onBeforeReflow(listener: () => void): IDisposable {
        rewriteListeners.add(listener);
        return { dispose: () => rewriteListeners.delete(listener) };
      },
    },
    /** The live paint: [row, x, width, background]. */
    live: () => painted.filter((p) => !p.disposed).map((p) => [p.row, p.x, p.width, p.background]),
    paintedCount: () => painted.length,
    changed: () => { for (const listener of bufferListeners) listener(); },
    /** xterm switches screens (a full-screen program starts or ends). */
    switchScreen: () => { for (const listener of screenListeners) listener(); },
    /** A column-changing resize is about to reflow. */
    rewrite: () => { for (const listener of rewriteListeners) listener(); },
    fail(err: Error | null) { failure = err; },
    listenerCount: () => bufferListeners.size + screenListeners.size + rewriteListeners.size,
  };
}

const COLORS: TerminalFindColors = { match: '#111111', current: '#222222' };
const ROWS = ['zero', 'one needle', 'two', 'three NEEDLE needle', 'four'];

function setup(lines = [...ROWS], rows = 24) {
  const fake = fakeTerminal(lines, rows);
  const find = createTerminalFind({ ...fake.source, colors: () => COLORS });
  return { fake, find };
}

beforeEach(() => {
  resetPaneFindForTest();
});

afterEach(() => {
  resetPaneFindForTest();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('createTerminalFind', () => {
  it('matches the trimmed query by the app\'s fold rule and starts at the newest match, painted', () => {
    const { fake, find } = setup();
    find.search('  Needle ');
    expect(find.result).toMatchObject({ count: 3, current: 2, pending: false, capped: false, error: null });
    // The active match carries only its own ground.
    expect(fake.live()).toEqual([
      [1, 4, 6, '#111111'],
      [3, 6, 6, '#111111'],
      [3, 13, 6, '#222222'],
    ]);
  });

  it('scrolls a match out of view to the middle of the screen, and one in view not at all', () => {
    const { fake, find } = setup(['needle', ...Array.from({ length: 40 }, () => 'filler'), 'needle'], 10);
    find.search('needle');
    expect(fake.scrolls).toEqual([41 - 5]);
    find.next();
    expect(find.result.current).toBe(0);
    expect(fake.scrolls).toEqual([36, -41]);
  });

  it('does not search again for a query that trims to the current one', () => {
    const { fake, find } = setup();
    find.search('needle');
    const painted = fake.paintedCount();
    find.search('needle ');
    expect(fake.paintedCount()).toBe(painted);
  });

  it('steps down with next and up with previous, wrapping, moving the active paint', () => {
    const { fake, find } = setup();
    find.search('needle');
    find.next();
    expect(find.result.current).toBe(0);
    // The stepped-from match has its match ground back.
    expect(fake.live().filter((p) => p[0] === 1 || p[1] === 13)).toEqual([[3, 13, 6, '#111111'], [1, 4, 6, '#222222']]);
    find.previous();
    expect(find.result.current).toBe(2);
    find.previous();
    expect(find.result.current).toBe(1);
    expect(fake.live().filter((p) => p[3] === '#222222')).toEqual([[3, 6, 6, '#222222']]);
  });

  it('reports no match, and steps nothing without a query', () => {
    const { fake, find } = setup();
    find.next();
    find.previous();
    expect(fake.paintedCount()).toBe(0);
    find.search('absent');
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false });
    find.next();
    expect(find.result.current).toBe(-1);
    expect(fake.scrolls).toEqual([]);
  });

  it('counts and paints the newest matches up to the limit, reported as capped', () => {
    // The limit falls inside the line of ten.
    const singles = TERMINAL_FIND_MATCH_LIMIT - 5;
    const { fake, find } = setup(['e', 'e'.repeat(10), ...Array.from({ length: singles }, () => 'e')]);
    find.search('e');
    expect(find.result).toMatchObject({ count: TERMINAL_FIND_MATCH_LIMIT, current: TERMINAL_FIND_MATCH_LIMIT - 1, capped: true });
    const live = fake.live();
    expect(live.length).toBe(TERMINAL_FIND_MATCH_LIMIT);
    // The newest five of the line of ten, and nothing above it.
    expect(live.filter((p) => p[0] === 1).map((p) => p[1])).toEqual([5, 6, 7, 8, 9]);
    expect(live.filter((p) => p[0] === 0)).toEqual([]);
  });

  it('clear removes every paint, and again is a no-op', () => {
    const { fake, find } = setup();
    find.search('needle');
    find.clear();
    expect(fake.live()).toEqual([]);
    expect(find.result).toMatchObject({ count: 0, current: -1, pending: false, error: null });
    find.clear();
    expect(fake.live()).toEqual([]);
  });

  it('an empty query clears', () => {
    const { fake, find } = setup();
    find.search('needle');
    find.search('   ');
    expect(fake.live()).toEqual([]);
    expect(find.result.count).toBe(0);
  });

  it('shows a failed search as its error and reports it, until a later call succeeds', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const { fake, find } = setup();
    fake.fail(new Error('decoration refused'));
    find.search('needle');
    expect(find.result).toMatchObject({ count: 0, current: -1, error: 'decoration refused' });
    expect(warn).toHaveBeenCalledWith('terminal find failed', 'decoration refused');
    // A buffer change while failed does not read as a pending re-search.
    fake.changed();
    expect(find.result.pending).toBe(false);
    fake.fail(null);
    find.next();
    expect(find.result).toMatchObject({ count: 3, current: 0, error: null });
  });

  it('restyle repaints the current query with the current colors, keeping its match', () => {
    const fake = fakeTerminal([...ROWS]);
    let colors = COLORS;
    const find = createTerminalFind({ ...fake.source, colors: () => colors });
    find.restyle();
    expect(fake.paintedCount()).toBe(0);
    find.search('needle');
    find.next();
    colors = { match: '#333333', current: '#444444' };
    find.restyle();
    expect(find.result).toMatchObject({ current: 0, pending: false });
    expect(fake.live()).toEqual([
      [3, 6, 6, '#333333'],
      [3, 13, 6, '#333333'],
      [1, 4, 6, '#444444'],
    ]);
  });

  it('dispose drops its listener and its paint', () => {
    const { fake, find } = setup();
    find.search('needle');
    expect(fake.listenerCount()).toBe(3);
    find.dispose();
    expect(fake.listenerCount()).toBe(0);
    expect(fake.live()).toEqual([]);
  });
});

describe('createTerminalFind re-search', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  it('re-searches once buffer changes pause, keeping its match, without scrolling', async () => {
    const { fake, find } = setup();
    fake.changed();
    expect(find.result.pending).toBe(false);
    find.search('needle');
    find.next();
    const scrolls = [...fake.scrolls];
    fake.lines.push('five needle');
    fake.changed();
    // Progressive: the bar keeps showing the last count beside the spinner.
    expect(find.result).toMatchObject({ count: 3, pending: true, progressive: true });
    await vi.advanceTimersByTimeAsync(TERMINAL_FIND_REFRESH_DELAY_MS - 1);
    expect(find.result.count).toBe(3);
    await vi.advanceTimersByTimeAsync(1);
    expect(find.result).toMatchObject({ count: 4, current: 0, pending: false });
    expect(fake.scrolls).toEqual(scrolls);
  });

  it('a re-search keeps the active match by its offset in its line', async () => {
    const { fake, find } = setup();
    // The second match on its line.
    find.search('needle');
    expect(find.result.current).toBe(2);
    fake.lines.push('five needle');
    fake.changed();
    await vi.advanceTimersByTimeAsync(TERMINAL_FIND_REFRESH_DELAY_MS);
    expect(find.result).toMatchObject({ count: 4, current: 2, pending: false });
    expect(fake.live().filter((p) => p[3] === '#222222')).toEqual([[3, 13, 6, '#222222']]);
  });

  it('a switch of screens clears the paint at once and re-searches the new one', async () => {
    const { fake, find } = setup();
    find.search('needle');
    expect(fake.live().length).toBe(3);
    fake.switchScreen();
    expect(fake.live()).toEqual([]);
    expect(find.result).toMatchObject({ count: 3, pending: true });
    await vi.advanceTimersByTimeAsync(TERMINAL_FIND_REFRESH_DELAY_MS);
    expect(find.result).toMatchObject({ count: 3, pending: false });
    expect(fake.live().length).toBe(3);
  });

  it('drops the paint before a reflow, and re-searches after', async () => {
    const { fake, find } = setup();
    find.search('needle');
    fake.rewrite();
    expect(fake.live()).toEqual([]);
    expect(find.result).toMatchObject({ count: 3, pending: true });
    await vi.advanceTimersByTimeAsync(TERMINAL_FIND_REFRESH_DELAY_MS);
    expect(fake.live().length).toBe(3);
  });

  it('re-searches at least once a second under sustained buffer changes', async () => {
    const { fake, find } = setup();
    find.search('needle');
    fake.lines.push('five needle');
    for (let at = 0; at < TERMINAL_FIND_REFRESH_MAX_WAIT_MS; at += 50) {
      fake.changed();
      await vi.advanceTimersByTimeAsync(50);
    }
    expect(find.result.count).toBe(4);
  });

  it('a clear cancels a scheduled re-search', async () => {
    const { fake, find } = setup();
    find.search('needle');
    fake.changed();
    find.clear();
    const painted = fake.paintedCount();
    await vi.advanceTimersByTimeAsync(TERMINAL_FIND_REFRESH_MAX_WAIT_MS);
    expect(fake.paintedCount()).toBe(painted);
  });

  it('dispose cancels a scheduled re-search', async () => {
    const { fake, find } = setup();
    find.search('needle');
    fake.changed();
    find.dispose();
    const painted = fake.paintedCount();
    await vi.advanceTimersByTimeAsync(TERMINAL_FIND_REFRESH_MAX_WAIT_MS);
    expect(fake.paintedCount()).toBe(painted);
  });
});

describe('registerTerminalFind', () => {
  const tab1 = terminalFindKey('pane-1', 't1');
  const tab2 = terminalFindKey('pane-1', 't2');

  it('keys each terminal under its pane, apart from its siblings', () => {
    expect(paneOfFindKey(tab1)).toBe('pane-1');
    expect(tab1).not.toBe(tab2);
    expect(terminalFindKey('pane-2', 't1')).not.toBe(tab1);
  });

  it('is driven through the pane find store, and release clears it', () => {
    const fake = fakeTerminal([...ROWS]);
    const registration = registerTerminalFind(tab1, fake.source);
    expect(openPaneFind(tab1)).toBe(true);
    setPaneFindQuery(tab1, 'needle');
    stepPaneFind(tab1, 1);
    expect(getPaneFindProvider(tab1)?.result).toMatchObject({ count: 3, current: 0 });
    registration.release();
    expect(fake.live()).toEqual([]);
    expect(getPaneFindProvider(tab1)).toBeNull();
    expect(fake.listenerCount()).toBe(0);
  });

  it('a tab switch leaves each terminal its own provider, bar state and matches', () => {
    const first = fakeTerminal([...ROWS]);
    const releaseFirst = registerTerminalFind(tab1, first.source);
    openPaneFind(tab1);
    setPaneFindQuery(tab1, 'needle');
    // Switching away unmounts the first body; the second mounts.
    releaseFirst.release();
    const second = fakeTerminal([...ROWS]);
    registerTerminalFind(tab2, second.source);
    expect(isPaneFindOpen(tab2)).toBe(false);
    expect(second.paintedCount()).toBe(0);
    expect(first.live()).toEqual([]);
  });

  it('a re-created terminal searches its open query again, and the old release leaves it', () => {
    const before = fakeTerminal([...ROWS]);
    const old = registerTerminalFind(tab1, before.source);
    openPaneFind(tab1);
    setPaneFindQuery(tab1, 'needle');
    const after = fakeTerminal(['needle']);
    registerTerminalFind(tab1, after.source);
    expect(getPaneFindProvider(tab1)?.result.count).toBe(1);
    old.release();
    expect(getPaneFindProvider(tab1)?.result.count).toBe(1);
    closePaneFind(tab1);
    expect(after.live()).toEqual([]);
  });
});
