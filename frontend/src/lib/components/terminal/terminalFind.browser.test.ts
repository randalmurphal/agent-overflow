// Terminal find against a real xterm, built the way every terminal surface
// builds it (terminalXterm.buildTerminal), in real Chromium: what the scan
// matches in a real buffer (wraps, wide and combining characters, the
// alternate screen, trimming, reflow), where the matches are painted, on
// the cells as the WebGL renderer draws them too, that the active match
// follows its text through every buffer change, that the reader's selection
// and a drag in progress are never touched, and the work a re-search does
// at full scrollback.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { IBuffer, IBufferLine, IDisposable, IMarker, Terminal } from '@xterm/xterm';
import { page } from 'vitest/browser';
import '../../../app.css';
import { applyThemeClass } from '../../utils/theme';
import { applyTheme, resetThemeApplyForTest } from '../../theme/themeApply.svelte';
import { buildTerminal, type BuiltTerminal } from './terminalXterm';
import { getXtermFindColors, getXtermTheme, resetXtermThemeCache } from './terminalTheme';
import { TERMINAL_FIND_ACTIVE_CLASS, TERMINAL_FIND_MATCH_CLASS } from './terminalFindLayer';
import {
  TERMINAL_FIND_MATCH_LIMIT,
  createTerminalFind,
  registerTerminalFind,
  terminalFindKey,
  type TerminalFind,
  type TerminalFindRegistration,
  type TerminalFindTerminal,
} from './terminalFind.svelte';
import {
  closePaneFind,
  getPaneFindProvider,
  openPaneFind,
  resetPaneFindForTest,
  setPaneFindQuery,
  stepPaneFind,
} from '../../stores/paneFind.svelte';

const KEY = terminalFindKey('pane-browser', 't1');
const NEEDLE_ROWS = [10, 250];
const LINES = 300;

let mount: HTMLDivElement;
let built: BuiltTerminal;
let term: Terminal;
let registration: TerminalFindRegistration | null = null;
let priorClass = '';

function applyMode(mode: 'dark' | 'light'): void {
  applyThemeClass(mode);
  applyTheme({ mode, appearance: { uiTheme: 'default', codeTheme: 'github' }, themes: [], revision: 1 });
}

function write(data: string): Promise<void> {
  return new Promise((resolve) => term.write(data, resolve));
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function frames(): Promise<void> {
  for (let i = 0; i < 2; i += 1) await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
}

function result() {
  return getPaneFindProvider(KEY)!.result;
}

function inView(row: number): boolean {
  const top = term.buffer.active.viewportY;
  return row >= top && row < top + term.rows;
}

function cellSize(): { width: number; height: number } {
  const screen = mount.querySelector<HTMLElement>('.xterm-screen')!.getBoundingClientRect();
  return { width: screen.width / term.cols, height: screen.height / term.rows };
}

function rectangles(): HTMLElement[] {
  return [...mount.querySelectorAll<HTMLElement>(`.${TERMINAL_FIND_MATCH_CLASS}, .${TERMINAL_FIND_ACTIVE_CLASS}`)];
}

/** The rectangles of `kind` on `row`, which must be on screen, in cells.
 * Each must sit exactly on cell edges. */
function painted(row: number, kind: string): Array<{ x: number; width: number }> {
  const cell = cellSize();
  const grid = mount.querySelector<HTMLElement>('.xterm-screen')!.getBoundingClientRect();
  const offset = row - term.buffer.active.viewportY;
  const found: Array<{ x: number; width: number }> = [];
  for (const el of mount.querySelectorAll<HTMLElement>(`.${kind}`)) {
    const box = el.getBoundingClientRect();
    const y = (box.top - grid.top) / cell.height;
    const x = (box.left - grid.left) / cell.width;
    const width = box.width / cell.width;
    for (const value of [y, x, width]) expect(Math.abs(value - Math.round(value))).toBeLessThan(0.01);
    expect(box.height).toBeCloseTo(cell.height, 2);
    if (Math.round(y) === offset) found.push({ x: Math.round(x), width: Math.round(width) });
  }
  return found.sort((a, b) => a.x - b.x);
}

function paintedCount(): number {
  return rectangles().length;
}

/** Waits for xterm's next render, which redraws the layer. */
function rendered(): Promise<void> {
  return new Promise((resolve) => {
    const once = term.onRender(() => {
      once.dispose();
      resolve();
    });
  });
}

/** Scrolls `row` into view and waits for it to render. */
async function show(row: number): Promise<void> {
  term.scrollToLine(Math.max(0, row - 2));
  await frames();
}

function selection() {
  return { text: term.getSelection(), at: term.getSelectionPosition() };
}

function needleLines(): string {
  return Array.from({ length: LINES }, (_, i) => (NEEDLE_ROWS.includes(i) ? `row ${i} has the Needle in it` : `row ${i}`)).join('\r\n');
}

async function build(width = 640): Promise<void> {
  mount = document.createElement('div');
  // As the surfaces' mounts: the layer fills it.
  mount.style.cssText = `position:relative;width:${width}px;height:200px`;
  document.body.appendChild(mount);
  built = buildTerminal(mount, { onInput: () => {} });
  term = built.term;
}

function register(): void {
  registration = registerTerminalFind(KEY, built);
  expect(openPaneFind(KEY)).toBe(true);
}

beforeEach(() => {
  priorClass = document.documentElement.className;
  resetXtermThemeCache();
  resetThemeApplyForTest();
  resetPaneFindForTest();
  applyMode('dark');
});

afterEach(() => {
  registration?.release();
  registration = null;
  term.dispose();
  mount.remove();
  resetPaneFindForTest();
  document.documentElement.className = priorClass;
  resetXtermThemeCache();
  resetThemeApplyForTest();
});

describe('terminal find on a real xterm', () => {
  beforeEach(async () => {
    await build();
    await write(needleLines());
    register();
  });

  it('starts at the newest match, scrolled into view and painted, selecting nothing', async () => {
    expect(inView(NEEDLE_ROWS[1])).toBe(false);
    setPaneFindQuery(KEY, '  needle ');
    expect(result()).toMatchObject({ count: 2, current: 1, pending: false, error: null });
    expect(inView(NEEDLE_ROWS[1])).toBe(true);
    expect(term.hasSelection()).toBe(false);
    await frames();
    expect(painted(NEEDLE_ROWS[1], TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 16, width: 6 }]);
    expect(painted(NEEDLE_ROWS[1], TERMINAL_FIND_MATCH_CLASS)).toEqual([]);
  });

  it('steps down and up, wrapping, scrolling each match into view and moving the active paint', async () => {
    setPaneFindQuery(KEY, 'needle');
    stepPaneFind(KEY, 1);
    expect(result().current).toBe(0);
    expect(inView(NEEDLE_ROWS[0])).toBe(true);
    await frames();
    expect(painted(NEEDLE_ROWS[0], TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 15, width: 6 }]);
    stepPaneFind(KEY, 1);
    expect(result().current).toBe(1);
    expect(inView(NEEDLE_ROWS[1])).toBe(true);
    await frames();
    expect(painted(NEEDLE_ROWS[1], TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 16, width: 6 }]);
    await show(NEEDLE_ROWS[0]);
    expect(painted(NEEDLE_ROWS[0], TERMINAL_FIND_ACTIVE_CLASS)).toEqual([]);
    expect(painted(NEEDLE_ROWS[0], TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 15, width: 6 }]);
    stepPaneFind(KEY, -1);
    expect(result().current).toBe(0);
    stepPaneFind(KEY, -1);
    expect(result().current).toBe(1);
  });

  it('a changed query starts again at the newest match, not at the one stepped to', () => {
    setPaneFindQuery(KEY, 'needle');
    stepPaneFind(KEY, 1);
    expect(inView(NEEDLE_ROWS[0])).toBe(true);
    setPaneFindQuery(KEY, 'needl');
    expect(result()).toMatchObject({ count: 2, current: 1 });
    expect(inView(NEEDLE_ROWS[1])).toBe(true);
  });

  it('reports no match', () => {
    setPaneFindQuery(KEY, 'absent');
    expect(result()).toMatchObject({ count: 0, current: -1 });
  });

  it('the count follows new output, pending until the re-search, which keeps its match and does not scroll', async () => {
    setPaneFindQuery(KEY, 'needle');
    expect(result().count).toBe(2);
    // The reader follows the output, away from the match.
    term.scrollToBottom();
    await write('\r\nnew output with a needle');
    expect(result()).toMatchObject({ count: 2, pending: true });
    const top = term.buffer.active.viewportY;
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 3, pending: false }), { timeout: 2000 });
    expect(result().current).toBe(1);
    expect(term.buffer.active.viewportY).toBe(top);
    await show(NEEDLE_ROWS[1]);
    expect(painted(NEEDLE_ROWS[1], TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 16, width: 6 }]);
  });

  it('a step while output is pending steps over the buffer as it is now', async () => {
    setPaneFindQuery(KEY, 'needle');
    await write('\r\nnew output with a needle');
    expect(result().pending).toBe(true);
    stepPaneFind(KEY, 1);
    expect(result()).toMatchObject({ count: 3, current: 2, pending: false });
  });

  it('never changes the reader\'s selection, made before, during or after a search', async () => {
    term.select(0, 5, 5);
    const before = selection();
    expect(before.text).toBe('row 5');
    setPaneFindQuery(KEY, 'needle');
    expect(selection()).toEqual(before);
    stepPaneFind(KEY, 1);
    expect(selection()).toEqual(before);
    // Made during the search.
    term.select(0, 6, 5);
    const during = selection();
    stepPaneFind(KEY, -1);
    expect(selection()).toEqual(during);
    await write('\r\nnew output with a needle');
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 3, pending: false }), { timeout: 2000 });
    expect(selection()).toEqual(during);
    getPaneFindProvider(KEY)!.search('needle in');
    expect(selection()).toEqual(during);
    registration!.restyle();
    expect(selection()).toEqual(during);
    closePaneFind(KEY);
    expect(selection()).toEqual(during);
    // Made after: a later open, query and close leave it too.
    term.select(0, 7, 5);
    const after = selection();
    openPaneFind(KEY);
    setPaneFindQuery(KEY, 'row');
    stepPaneFind(KEY, 1);
    closePaneFind(KEY);
    expect(selection()).toEqual(after);
  });

  it('a drag in progress survives re-searches during streaming output that auto-scrolls', async () => {
    setPaneFindQuery(KEY, 'needle');
    term.scrollToBottom();
    await frames();
    const screen = mount.querySelector<HTMLElement>('.xterm-screen')!;
    const rect = screen.getBoundingClientRect();
    const cell = cellSize();
    const at = (col: number, row: number) => ({ clientX: rect.left + (col + 0.5) * cell.width, clientY: rect.top + (row + 0.5) * cell.height });
    const startRow = term.buffer.active.viewportY + 2;
    screen.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0, detail: 1, ...at(1, 2) }));
    document.dispatchEvent(new MouseEvent('mousemove', { bubbles: true, buttons: 1, ...at(3, 4) }));
    // Output with matches streams past a quiet period and the longest wait.
    for (let i = 0; i < 12; i += 1) {
      await write(`\r\nstreamed ${i} needle`);
      await sleep(120);
    }
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 14, pending: false }), { timeout: 2000 });
    expect(term.buffer.active.viewportY).toBe(term.buffer.active.baseY);
    const endRow = term.buffer.active.viewportY + 5;
    document.dispatchEvent(new MouseEvent('mousemove', { bubbles: true, buttons: 1, ...at(4, 5) }));
    document.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, button: 0, ...at(4, 5) }));
    expect(term.getSelectionPosition()).toEqual({ start: { x: 1, y: startRow }, end: { x: 4, y: endRow } });
  });

  // Every way the buffer changes re-counts: the ones that are not output
  // (Terminal: Clear's clear(), reset(), a resize) and the ones output
  // carries (a full reset, scrollback trimming).
  for (const [name, change, expected] of [
    ['clear()', async () => term.clear(), 0],
    ['reset()', async () => term.reset(), 0],
    ['a full reset (RIS)', () => write('\x1bc'), 0],
    ['scrollback trimming', () => write('\r\nfiller'.repeat(4100)), 0],
    ['a resize', async () => term.resize(30, 10), 2],
  ] as const) {
    it(`re-counts after ${name}`, async () => {
      setPaneFindQuery(KEY, 'needle');
      expect(result().count).toBe(2);
      await change();
      expect(result().pending).toBe(true);
      await vi.waitFor(() => expect(result()).toMatchObject({ count: expected, pending: false }), { timeout: 3000 });
      if (expected === 0) {
        await frames();
        expect(paintedCount()).toBe(0);
      }
    });
  }

  it('when trimming drops the active match, the next one after it becomes active', async () => {
    setPaneFindQuery(KEY, 'needle');
    stepPaneFind(KEY, 1);
    expect(result().current).toBe(0);
    // Enough to trim row 10 and keep row 250, then a later match.
    const keep = 4000 + term.rows - LINES;
    await write(`${'\r\nfiller'.repeat(keep + 99)}\r\nlate needle`);
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, current: 0, pending: false }), { timeout: 3000 });
    const row = NEEDLE_ROWS[1] - (LINES + keep + 100 - term.buffer.active.length);
    expect(term.buffer.active.getLine(row)?.translateToString(true)).toBe('row 250 has the Needle in it');
    await show(row);
    expect(painted(row, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 16, width: 6 }]);
  });

  it('a close clears the paint, and later output searches nothing', async () => {
    setPaneFindQuery(KEY, 'needle');
    await frames();
    expect(paintedCount()).toBeGreaterThan(0);
    closePaneFind(KEY);
    await frames();
    expect(paintedCount()).toBe(0);
    await write('\r\nanother needle');
    await sleep(400);
    await frames();
    expect(result()).toMatchObject({ count: 0, current: -1, pending: false });
    expect(paintedCount()).toBe(0);
  });

  it('in a full-screen program searches only the visible screen', async () => {
    await write('\x1b[?1049h\x1b[Hscreen needle');
    setPaneFindQuery(KEY, 'needle');
    expect(result()).toMatchObject({ count: 1, current: 0 });
    await write('\x1b[?1049l');
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, current: 1, pending: false }), { timeout: 2000 });
  });

  it('paints with the theme\'s match grounds in both modes, translucent over the text', () => {
    const dark = getXtermFindColors('dark');
    applyMode('light');
    const light = getXtermFindColors('light');
    for (const grounds of [dark, light]) {
      for (const color of [grounds.match, grounds.current]) {
        const alpha = Number(/^rgba\(\d+, \d+, \d+, ([\d.]+)\)$/.exec(color ?? '')?.[1]);
        expect(alpha).toBeGreaterThan(0);
        expect(alpha).toBeLessThanOrEqual(0.45);
      }
      expect(grounds.current).not.toBe(grounds.match);
    }
    expect(light.match).not.toBe(dark.match);
  });
});

describe('terminal find text on a real xterm', () => {
  beforeEach(async () => {
    await build();
    term.resize(80, 10);
  });

  async function findIn(text: string, query: string): Promise<number> {
    await write(text);
    register();
    setPaneFindQuery(KEY, query);
    await frames();
    return term.buffer.active.length;
  }

  it('matches across a wrap and paints both rows', async () => {
    await findIn(`${'x'.repeat(78)}needle`, 'needle');
    expect(result().count).toBe(1);
    expect(term.buffer.active.getLine(1)?.isWrapped).toBe(true);
    expect(painted(0, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 78, width: 2 }]);
    expect(painted(1, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 0, width: 4 }]);
  });

  it('places matches after wide, combining and emoji characters in their cells', async () => {
    await findIn(['漢字 needle', 'é needle', '😀 needle', 'a漢字 n'].join('\r\n'), 'needle');
    expect(result().count).toBe(3);
    expect(painted(0, TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 5, width: 6 }]);
    expect(painted(1, TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 2, width: 6 }]);
    expect(painted(2, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 3, width: 6 }]);
    setPaneFindQuery(KEY, '字 n');
    await frames();
    expect(result().count).toBe(2);
    expect(painted(3, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 3, width: 4 }]);
  });

  it('a wide character wrapped to the next row leaves no gap in the text', async () => {
    await findIn(`${'x'.repeat(79)}漢needle`, 'x漢');
    expect(term.buffer.active.getLine(1)?.isWrapped).toBe(true);
    expect(result().count).toBe(1);
    expect(painted(0, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 78, width: 1 }]);
    expect(painted(1, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 0, width: 2 }]);
  });

  it('folds case by the app\'s match rule', async () => {
    await findIn('ÄRGER and the NEEDLE', 'ärger');
    expect(result().count).toBe(1);
  });

  it('keeps the active match through a column reflow, both ways', async () => {
    // Codex's sequence, with a match after the active one: a row number
    // kept from before the reflow would land on the wrong match.
    const needles = [10, 150, 250];
    const line = (i: number) => (needles.includes(i) ? `${String(i).padStart(4, '0')} ${'.'.repeat(44)} the needle` : `${String(i).padStart(4, '0')} ${'.'.repeat(55)}`);
    await write(Array.from({ length: LINES }, (_, i) => line(i)).join('\r\n'));
    register();
    setPaneFindQuery(KEY, 'needle');
    stepPaneFind(KEY, -1);
    expect(result()).toMatchObject({ count: 3, current: 1 });
    term.resize(40, 10);
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 3, current: 1, pending: false }), { timeout: 2000 });
    // Every 60-character line is two rows at 40 columns; the needle starts
    // at offset 54, on the second.
    const row = needles[1] * 2 + 1;
    expect(term.buffer.active.getLine(row)?.translateToString(true)).toBe('......... the needle');
    await show(row);
    expect(painted(row, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 14, width: 6 }]);
    term.resize(80, 10);
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 3, current: 1, pending: false }), { timeout: 2000 });
    await show(needles[1]);
    expect(painted(needles[1], TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 54, width: 6 }]);
  });
});

/** `color` (`rgba()`) over the dark terminal ground, as the page shows it. */
function tinted(color: string | undefined): string {
  const [r, g, b, a] = /^rgba\((\d+), (\d+), (\d+), ([\d.]+)\)$/.exec(color ?? '')!.slice(1).map(Number);
  const under = groundHex().slice(1).match(/../g)!.map((part) => parseInt(part, 16));
  return `#${[r, g, b].map((v, i) => Math.round(v * a + under[i] * (1 - a)).toString(16).padStart(2, '0')).join('')}`;
}

/** Whether two `#rrggbb` colors differ by at most the compositor's rounding. */
function near(shown: string, expected: string): boolean {
  const channels = (hex: string) => hex.slice(1).match(/../g)!.map((part) => parseInt(part, 16));
  const [a, b] = [channels(shown), channels(expected)];
  return a.every((v, i) => Math.abs(v - b[i]) <= 2);
}

/** Each cell's most common color as the page shows it, from a screenshot
 * of the mount: the ground under the glyph, tinted by find's layer. */
async function groundsShown(): Promise<(col: number, row: number) => string> {
  await frames();
  const image = new Image();
  image.src = `data:image/png;base64,${await page.screenshot({ element: mount, save: false })}`;
  await image.decode();
  const canvas = document.createElement('canvas');
  canvas.width = image.width;
  canvas.height = image.height;
  const context = canvas.getContext('2d')!;
  context.drawImage(image, 0, 0);
  const box = mount.getBoundingClientRect();
  const screen = mount.querySelector('.xterm-screen')!.getBoundingClientRect();
  const scale = image.width / box.width;
  const cell = cellSize();
  return (col, row) => {
    const x = Math.round((screen.left - box.left + col * cell.width) * scale);
    const y = Math.round((screen.top - box.top + row * cell.height) * scale);
    const { data } = context.getImageData(x, y, Math.max(1, Math.floor(cell.width * scale)), Math.max(1, Math.floor(cell.height * scale)));
    const counts = new Map<string, number>();
    for (let i = 0; i < data.length; i += 4) {
      const hex = `#${[data[i], data[i + 1], data[i + 2]].map((v) => v.toString(16).padStart(2, '0')).join('')}`;
      counts.set(hex, (counts.get(hex) ?? 0) + 1);
    }
    return [...counts].sort((a, b) => b[1] - a[1])[0][0];
  };
}

/** Each cell's ground as the page shows it, named: `current` and `match`
 * for find's two tints over the dark ground, `ground` for none, else the
 * hex read. */
async function shadesShown(): Promise<(col: number, row: number) => string> {
  const shown = await groundsShown();
  const colors = getXtermFindColors('dark');
  const named: Array<[string, string]> = [['current', tinted(colors.current)], ['match', tinted(colors.match)], ['ground', groundHex()]];
  return (col, row) => {
    const hex = shown(col, row);
    return named.find(([, expected]) => near(hex, expected))?.[0] ?? hex;
  };
}

describe('terminal find on the alternate screen, as the renderer paints it', () => {
  beforeEach(async () => {
    await build();
    term.resize(80, 10);
  });

  it('paints the match grounds on a full-screen program\'s cells, and neither screen\'s highlights on the other', async () => {
    // The production renderer: WebGL draws the cells; there are no DOM rows.
    expect(mount.querySelector('.xterm-screen canvas')).not.toBeNull();
    expect(mount.querySelector('.xterm-rows')).toBeNull();

    await write('normal row\r\n   normal needle');
    register();
    setPaneFindQuery(KEY, 'needle');
    let shown = await shadesShown();
    expect(shown(10, 1)).toBe('current');
    expect(shown(0, 1)).toBe('ground');

    // A full-screen program draws two matches; the normal screen's match
    // cell (row 1, column 10) is plain text there.
    await write('\x1b[?1049h\x1b[H\x1b[2Jalt screen\r\nneedle one is here\r\nand needle two');
    shown = await shadesShown();
    expect(shown(10, 1)).toBe('ground');
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, current: 1, pending: false }), { timeout: 2000 });
    shown = await shadesShown();
    expect(shown(0, 1)).toBe('match');
    expect(shown(5, 1)).toBe('match');
    expect(shown(4, 2)).toBe('current');
    expect(shown(9, 2)).toBe('current');
    expect(shown(10, 1)).toBe('ground');
    // Stepping moves the active ground on the program's screen.
    stepPaneFind(KEY, 1);
    shown = await shadesShown();
    expect(shown(0, 1)).toBe('current');
    expect(shown(4, 2)).toBe('match');

    // Leaving it, none of its highlights stays on the normal screen.
    await write('\x1b[?1049l');
    shown = await shadesShown();
    expect(shown(0, 1)).toBe('ground');
    expect(shown(4, 2)).toBe('ground');
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 1, current: 0, pending: false }), { timeout: 2000 });
    shown = await shadesShown();
    expect(shown(10, 1)).toBe('current');
    expect(shown(0, 1)).toBe('ground');
  });
});

/** The theme's terminal ground, as the screenshot reads it. */
function groundHex(): string {
  const [r, g, b] = getXtermTheme('dark').background!.match(/\d+/g)!.map(Number);
  return `#${[r, g, b].map((v) => v.toString(16).padStart(2, '0')).join('')}`;
}

describe('terminal find grounds through steps, as the renderer paints them', () => {
  beforeEach(async () => {
    await build();
    term.resize(80, 10);
  });

  it('paints only the active match in the current ground, after the search, every step either way and a re-search', async () => {
    // Three matches on screen: rows 1, 3 and 5 at column 2.
    await write(['zero', '  needle a', 'two', '  needle b', 'four', '  needle c'].join('\r\n'));
    register();
    setPaneFindQuery(KEY, 'needle');
    const expectActive = async (active: number) => {
      const shown = await shadesShown();
      for (const [index, row] of [1, 3, 5].entries()) {
        for (const col of [2, 7]) expect(shown(col, row)).toBe(index === active ? 'current' : 'match');
      }
      expect(shown(0, 1)).toBe('ground');
      expect(shown(8, 1)).toBe('ground');
    };
    await expectActive(2);
    for (const [delta, active] of [[1, 0], [1, 1], [1, 2], [-1, 1], [-1, 0], [-1, 2]] as const) {
      stepPaneFind(KEY, delta);
      expect(result().current).toBe(active);
      await expectActive(active);
    }
    stepPaneFind(KEY, 1);
    await write('\r\nfive');
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 3, current: 0, pending: false }), { timeout: 2000 });
    await expectActive(0);
  });
});

/** Entries in xterm's decoration service, which find must never add to. A
 * test reaches xterm internals here: nothing public counts them. */
function serviceDecorations(): number {
  const core = (term as unknown as { _core: { _decorationService: { decorations: Iterable<unknown> } } })._core;
  return [...core._decorationService.decorations].length;
}

describe('terminal find anchors and paint lifetime on a real xterm', () => {
  beforeEach(async () => {
    await build();
    term.resize(80, 10);
  });

  it('keeps the active match when trimming takes only the first row of its wrapped line', async () => {
    // Codex's sequence: a four-row line at the top of a full buffer, with
    // matches on its rows 2 and 3, the later active; one more line trims
    // only its first row.
    const top = `${'a'.repeat(160)}needle${'b'.repeat(74)}needle`;
    const max = 4000 + term.rows;
    await write(`${top}${'\r\nf'.repeat(max - 4)}`);
    expect(term.buffer.active.length).toBe(max);
    expect(term.buffer.active.getLine(3)?.isWrapped).toBe(true);
    expect(term.buffer.active.getLine(4)?.isWrapped).toBe(false);
    register();
    setPaneFindQuery(KEY, 'needle');
    expect(result()).toMatchObject({ count: 2, current: 1 });
    await write('\r\nmore');
    expect(term.buffer.active.getLine(0)?.isWrapped).toBe(true);
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, current: 1, pending: false }), { timeout: 2000 });
    // Then a reflow both ways keeps it too.
    term.resize(40, 10);
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, current: 1, pending: false }), { timeout: 2000 });
    term.resize(80, 10);
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, current: 1, pending: false }), { timeout: 2000 });
  });

  it('keeps the active match when its first rows are trimmed between a reflow and the re-search', async () => {
    const top = `${'a'.repeat(160)}needle${'b'.repeat(74)}needle`;
    const max = 4000 + term.rows;
    await write(`${top}${'\r\nf'.repeat(max - 4)}`);
    register();
    setPaneFindQuery(KEY, 'needle');
    expect(result()).toMatchObject({ count: 2, current: 1 });
    // Narrower, the line is seven rows: the reflow itself trims its first
    // three, and one more line its fourth, before any re-search runs.
    term.resize(40, 10);
    await write('\r\nmore');
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, current: 1, pending: false }), { timeout: 2000 });
  });

  // Codex's sequences: a program clears and redraws its screen, or deletes
  // a middle line, while find is open on matches over several rows; xterm
  // disposes those rows' markers inside the parser.
  for (const [name, change] of [
    ['a clear and redraw (CSI 2 J)', `\x1b[2J\x1b[H${Array.from({ length: 8 }, (_, i) => `redrawn ${i} needle`).join('\r\n')}`],
    ['a deleted middle line (CSI M)', '\x1b[4;1H\x1b[M\x1b[6;1H\x1b[2M'],
  ] as const) {
    for (const screen of ['normal', 'alternate'] as const) {
      it(`leaves no decoration registered in xterm after ${name} on the ${screen} screen`, async () => {
        const baseline = serviceDecorations();
        if (screen === 'alternate') await write('\x1b[?1049h\x1b[H');
        await write(Array.from({ length: 8 }, (_, i) => `line ${i} needle ${'.'.repeat(i)} needle`).join('\r\n'));
        register();
        setPaneFindQuery(KEY, 'needle');
        expect(result().count).toBe(16);
        stepPaneFind(KEY, -1);
        await write(change);
        expect(serviceDecorations()).toBe(baseline);
        await vi.waitFor(() => expect(result().pending).toBe(false), { timeout: 2000 });
        stepPaneFind(KEY, 1);
        expect(paintedCount()).toBeGreaterThan(0);
        closePaneFind(KEY);
        expect(serviceDecorations()).toBe(baseline);
      });
    }
  }

  it('registers no decoration in xterm for the terminal\'s life: searches, steps, re-searches, resizes, trimming, clear, reset, redraws, deleted lines, screen switches and a close', async () => {
    // Nothing else in the terminal registers one.
    expect(serviceDecorations()).toBe(0);
    // A match on each line's first row and one on its continuation row:
    // each 120-character line wraps at 80.
    await write(Array.from({ length: 30 }, (_, i) => `${String(i).padStart(3, '0')} needle ${'.'.repeat(80)} needle ${'.'.repeat(15)}`).join('\r\n'));
    register();
    setPaneFindQuery(KEY, 'needle');
    expect(result().count).toBe(60);
    await frames();
    expect(paintedCount()).toBeGreaterThan(0);
    const none = () => expect(serviceDecorations()).toBe(0);
    none();
    const settled = () => vi.waitFor(() => expect(result().pending).toBe(false), { timeout: 2000 });
    stepPaneFind(KEY, 1);
    stepPaneFind(KEY, -1);
    none();
    for (let cycle = 0; cycle < 3; cycle += 1) {
      term.resize(140, 10);
      await settled();
      none();
      term.resize(60, 10);
      await settled();
      none();
      registration!.restyle();
      none();
    }
    term.resize(80, 10);
    await settled();
    await write('\x1b[2J\x1b[Hneedle\r\nneedle\r\nneedle\x1b[2;1H\x1b[M');
    none();
    await settled();
    none();
    // Trimming every match away.
    await write('\r\nfiller'.repeat(4100));
    await settled();
    expect(result().count).toBe(0);
    none();
    await write('\r\nneedle\r\n');
    await settled();
    term.clear();
    await settled();
    none();
    await write('\r\nneedle');
    await settled();
    term.reset();
    await settled();
    none();
    await write('\r\nneedle\x1b[?1049h\x1b[Halt needle\r\nneedle\x1b[1;1H\x1b[M');
    await settled();
    none();
    await write('\x1b[?1049l');
    await settled();
    expect(result().count).toBe(1);
    none();
    closePaneFind(KEY);
    none();
  });
});

describe('terminal find\'s layer on a real xterm', () => {
  beforeEach(async () => {
    await build();
    term.resize(80, 10);
  });

  it('a line inserted or deleted above moves the rectangles with their rows before the re-search, and a deleted row\'s go', async () => {
    await write(['zero', 'one needle', 'two', 'three needle', 'four needle'].join('\r\n'));
    register();
    setPaneFindQuery(KEY, 'needle');
    await frames();
    expect(painted(1, TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 4, width: 6 }]);
    expect(painted(3, TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 6, width: 6 }]);
    expect(painted(4, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 5, width: 6 }]);
    // A line inserted at row 0 (CSI L) moves every row down one.
    const drawn = rendered();
    await write('\x1b[1;1H\x1b[L');
    await drawn;
    expect(result().pending).toBe(true);
    expect(painted(1, TERMINAL_FIND_MATCH_CLASS)).toEqual([]);
    expect(painted(2, TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 4, width: 6 }]);
    expect(painted(4, TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 6, width: 6 }]);
    expect(painted(5, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 5, width: 6 }]);
    // Deleting row 2 (CSI M) disposes its marker: its rectangle goes, and
    // the rows below move up.
    const again = rendered();
    await write('\x1b[3;1H\x1b[M');
    await again;
    expect(result().pending).toBe(true);
    expect(paintedCount()).toBe(2);
    expect(painted(2, TERMINAL_FIND_MATCH_CLASS)).toEqual([]);
    expect(painted(3, TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 6, width: 6 }]);
    expect(painted(4, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 5, width: 6 }]);
  });

  it('a scroll moves the rectangles in the render that shows it', async () => {
    await write(needleLines());
    register();
    setPaneFindQuery(KEY, 'needle');
    await frames();
    const row = NEEDLE_ROWS[1];
    expect(painted(row, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 16, width: 6 }]);
    for (const amount of [-3, 2, 1]) {
      const drawn = rendered();
      term.scrollLines(amount);
      await drawn;
      expect(inView(row)).toBe(true);
      expect(painted(row, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 16, width: 6 }]);
    }
    // Scrolled out of view, it is not drawn.
    const away = rendered();
    term.scrollToLine(NEEDLE_ROWS[0] - 5);
    await away;
    expect(paintedCount()).toBe(1);
    expect(painted(NEEDLE_ROWS[0], TERMINAL_FIND_MATCH_CLASS)).toEqual([{ x: 15, width: 6 }]);
  });

  it('a column change draws nothing until the re-search paints the reflowed rows', async () => {
    await write('zero\r\none needle');
    register();
    setPaneFindQuery(KEY, 'needle');
    await frames();
    expect(paintedCount()).toBe(1);
    const drawn = rendered();
    term.resize(60, 10);
    await drawn;
    expect(result().pending).toBe(true);
    expect(paintedCount()).toBe(0);
    await vi.waitFor(() => expect(result().pending).toBe(false), { timeout: 2000 });
    expect(painted(1, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 4, width: 6 }]);
  });

  it('a close and a release remove the layer and stop listening to renders', async () => {
    const subscriptions = new Set<IDisposable>();
    const onRender = term.onRender;
    Object.defineProperty(term, 'onRender', {
      configurable: true,
      value: (listener: Parameters<Terminal['onRender']>[0]) => {
        const subscription = onRender(listener);
        const tracked = { dispose: () => { subscriptions.delete(tracked); subscription.dispose(); } };
        subscriptions.add(tracked);
        return tracked;
      },
    });
    await write(needleLines());
    register();
    setPaneFindQuery(KEY, 'needle');
    expect(mount.querySelectorAll('[data-terminal-find-layer]')).toHaveLength(1);
    expect(subscriptions.size).toBe(1);
    // Steps and re-searches keep one layer and one listener.
    stepPaneFind(KEY, 1);
    registration!.restyle();
    expect(mount.querySelectorAll('[data-terminal-find-layer]')).toHaveLength(1);
    expect(subscriptions.size).toBe(1);
    closePaneFind(KEY);
    expect(mount.querySelector('[data-terminal-find-layer]')).toBeNull();
    expect(subscriptions.size).toBe(0);
    openPaneFind(KEY);
    setPaneFindQuery(KEY, 'needle');
    expect(subscriptions.size).toBe(1);
    registration!.release();
    registration = null;
    expect(mount.querySelector('[data-terminal-find-layer]')).toBeNull();
    expect(subscriptions.size).toBe(0);
  });

  for (const renderer of ['WebGL', 'DOM'] as const) {
    it(`a drag that starts on a highlighted cell selects, and typing reaches the terminal, under the ${renderer} renderer`, async () => {
      const typed: string[] = [];
      term.onData((data) => typed.push(data));
      await write(['zero', 'one needle here', 'two'].join('\r\n'));
      register();
      setPaneFindQuery(KEY, 'needle');
      if (renderer === 'DOM') {
        // A lost WebGL context falls back to the DOM renderer
        // (buildTerminal), whose rows sit under the layer.
        const canvas = [...mount.querySelectorAll('canvas')].find((c) => c.getContext('webgl2'))!;
        canvas.getContext('webgl2')!.getExtension('WEBGL_lose_context')!.loseContext();
        await vi.waitFor(() => expect(mount.querySelector('.xterm-rows')).not.toBeNull(), { timeout: 6000 });
      }
      await frames();
      expect(painted(1, TERMINAL_FIND_ACTIVE_CLASS)).toEqual([{ x: 4, width: 6 }]);
      await dragAcrossTheActiveMatch();
      expect(term.getSelection()).toBe('needle');
      term.input('x');
      expect(typed).toEqual(['x']);
    });
  }

  async function dragAcrossTheActiveMatch(): Promise<void> {
    const box = mount.querySelector<HTMLElement>(`.${TERMINAL_FIND_ACTIVE_CLASS}`)!.getBoundingClientRect();
    const cell = cellSize();
    const start = { clientX: box.left + cell.width / 2, clientY: box.top + cell.height / 2 };
    const end = { clientX: box.left + 6.5 * cell.width, clientY: start.clientY };
    // What the page hit-tests at the rectangle is xterm's, not the layer's.
    const target = document.elementFromPoint(start.clientX, start.clientY)!;
    expect(target.closest('[data-terminal-find-layer]')).toBeNull();
    expect(mount.querySelector('.xterm')!.contains(target)).toBe(true);
    target.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, button: 0, detail: 1, ...start }));
    document.dispatchEvent(new MouseEvent('mousemove', { bubbles: true, buttons: 1, ...end }));
    document.dispatchEvent(new MouseEvent('mouseup', { bubbles: true, button: 0, ...end }));
  }
});

/** `term` as the provider sees it, counting what the scan reads and
 * tracking every marker it makes until disposed. */
function countedTerminal(real: Terminal) {
  const reads = { lines: 0, texts: 0 };
  const made = { markers: 0 };
  const live = { markers: new Set<IMarker>() };
  const bound = <T extends object>(target: T, prop: string | symbol): unknown => {
    const value = Reflect.get(target, prop);
    return typeof value === 'function' ? value.bind(target) : value;
  };
  const line = (real: IBufferLine): IBufferLine => new Proxy(real, {
    get(target, prop) {
      if (prop !== 'translateToString') return bound(target, prop);
      return (...args: Parameters<IBufferLine['translateToString']>) => {
        reads.texts += 1;
        return target.translateToString(...args);
      };
    },
  });
  const buffer = (real: IBuffer): IBuffer => new Proxy(real, {
    get(target, prop) {
      if (prop !== 'getLine') return bound(target, prop);
      return (y: number) => {
        reads.lines += 1;
        const found = target.getLine(y);
        return found && line(found);
      };
    },
  });
  const term: TerminalFindTerminal = {
    get rows() { return real.rows; },
    buffer: {
      get active() { return buffer(real.buffer.active); },
      get normal() { return real.buffer.normal; },
      get alternate() { return real.buffer.alternate; },
      onBufferChange: real.buffer.onBufferChange,
    },
    registerMarker(offset) {
      const marker = real.registerMarker(offset);
      made.markers += 1;
      live.markers.add(marker);
      marker.onDispose(() => live.markers.delete(marker));
      return marker;
    },
    scrollLines: (amount) => real.scrollLines(amount),
  };
  return {
    term,
    reads,
    made,
    live,
    reset() {
      reads.lines = 0;
      reads.texts = 0;
      made.markers = 0;
    },
  };
}

// What a re-search costs is the rows it reads and the markers it makes;
// what a draw costs is a node per rectangle in view. All are bounded here,
// at full scrollback.
describe('terminal find work at full scrollback', () => {
  let find: TerminalFind;
  let counted: ReturnType<typeof countedTerminal>;

  beforeEach(async () => {
    await build();
    term.resize(80, 24);
    // A full buffer (4000 + 24 lines) whose matches, on every fourth line,
    // reach the limit only near its top; every line has sixty dashes.
    await write(Array.from({ length: 4100 }, (_, i) => (i % 4 === 0 ? `${i} needle ${'-'.repeat(60)}` : `${i} ${'-'.repeat(68)}`)).join('\r\n'));
    counted = countedTerminal(term);
    find = createTerminalFind({ term: counted.term, onBufferChange: built.onBufferChange, findLayer: built.findLayer, colors: () => getXtermFindColors('dark') });
  });

  afterEach(() => {
    find.dispose();
  });

  it('with the limit reached by dense matches, reads only the rows it needs from the bottom', () => {
    expect(term.buffer.active.length).toBe(4024);
    find.search('-');
    expect(find.result).toMatchObject({ count: TERMINAL_FIND_MATCH_LIMIT, capped: true });
    counted.reset();
    find.restyle();
    // Sixty matches a line: the limit is met in the bottom 17 lines.
    const rows = Math.ceil(TERMINAL_FIND_MATCH_LIMIT / 60);
    expect(counted.reads.texts).toBeLessThanOrEqual(rows);
    expect(counted.reads.lines).toBeLessThanOrEqual(rows * 5);
  });

  it('with no match, reads each row of the buffer once', () => {
    find.search('absent');
    counted.reset();
    find.restyle();
    expect(counted.reads.texts).toBe(term.buffer.active.length);
  });

  it('makes a marker a painted row and the two anchors, and leaves nothing of the previous scan', () => {
    find.search('needle');
    expect(find.result).toMatchObject({ count: TERMINAL_FIND_MATCH_LIMIT, capped: true });
    const first = new Set(counted.live.markers);
    for (let i = 0; i < 5; i += 1) {
      counted.reset();
      find.restyle();
      expect(counted.made.markers).toBe(TERMINAL_FIND_MATCH_LIMIT + 2);
      expect(counted.live.markers.size).toBe(TERMINAL_FIND_MATCH_LIMIT + 2);
    }
    for (const marker of first) expect(counted.live.markers.has(marker)).toBe(false);
    find.clear();
    expect(counted.live.markers.size).toBe(0);
  });

  // The painted dashes on rows in view: the newest thousand, from the bottom.
  function dashesInView(): number {
    const buffer = term.buffer.active;
    let left = TERMINAL_FIND_MATCH_LIMIT;
    let shown = 0;
    for (let row = buffer.length - 1; row >= 0 && left > 0; row -= 1) {
      const here = Math.min(left, (buffer.getLine(row)!.translateToString(true).match(/-/g) ?? []).length);
      left -= here;
      if (inView(row)) shown += here;
    }
    return shown;
  }

  it('draws a node only for each rectangle on a row in view, at the limit', async () => {
    // A thousand one-cell matches, all on the bottom 17 rows, in view.
    find.search('-');
    expect(find.result.count).toBe(TERMINAL_FIND_MATCH_LIMIT);
    await frames();
    expect(dashesInView()).toBe(TERMINAL_FIND_MATCH_LIMIT);
    expect(paintedCount()).toBe(TERMINAL_FIND_MATCH_LIMIT);
    // Six rows up, six of those rows leave the view, and their nodes go.
    const drawn = rendered();
    term.scrollLines(-6);
    await drawn;
    expect(dashesInView()).toBeLessThan(TERMINAL_FIND_MATCH_LIMIT - 300);
    expect(paintedCount()).toBe(dashesInView());
    // Every fourth line's needle, at the limit: the view shows six.
    find.search('needle');
    expect(find.result.count).toBe(TERMINAL_FIND_MATCH_LIMIT);
    const top = rendered();
    term.scrollToTop();
    await top;
    // The top of the buffer is above the newest thousand needles.
    expect(paintedCount()).toBe(0);
    const bottom = rendered();
    term.scrollToBottom();
    await bottom;
    expect(paintedCount()).toBe(6);
  });
});
