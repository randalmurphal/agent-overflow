// Terminal find against a real xterm, built the way every terminal surface
// builds it (terminalXterm.buildTerminal), in real Chromium: what the scan
// matches in a real buffer (wraps, wide and combining characters, the
// alternate screen, trimming, reflow), where the matches are painted, on
// the cells as the WebGL renderer draws them too, that the active match
// follows its text through every buffer change, that the reader's selection
// and a drag in progress are never touched, and the work a re-search does
// at full scrollback.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { IBuffer, IBufferLine, IDecoration, IMarker, Terminal } from '@xterm/xterm';
import { page } from 'vitest/browser';
import '../../../app.css';
import { applyThemeClass } from '../../utils/theme';
import { applyTheme, resetThemeApplyForTest } from '../../theme/themeApply.svelte';
import { buildTerminal, type BuiltTerminal } from './terminalXterm';
import { getXtermFindColors, getXtermTheme, resetXtermThemeCache } from './terminalTheme';
import {
  TERMINAL_FIND_ACTIVE_CLASS,
  TERMINAL_FIND_MATCH_CLASS,
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

/** The painted decorations of `kind` on `row`, which must be on screen, in cells. */
function painted(row: number, kind: string): Array<{ x: number; width: number }> {
  const cell = cellSize();
  const offset = row - term.buffer.active.viewportY;
  return [...mount.querySelectorAll<HTMLElement>(`.${kind}`)]
    .filter((el) => el.style.display !== 'none' && Math.round(parseFloat(el.style.top) / cell.height) === offset)
    .map((el) => ({ x: Math.round((parseFloat(el.style.left) || 0) / cell.width), width: Math.round(parseFloat(el.style.width) / cell.width) }))
    .sort((a, b) => a.x - b.x);
}

function paintedCount(): number {
  return [...mount.querySelectorAll<HTMLElement>(`.${TERMINAL_FIND_MATCH_CLASS}, .${TERMINAL_FIND_ACTIVE_CLASS}`)].filter((el) => el.style.display !== 'none').length;
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
  mount.style.cssText = `width:${width}px;height:200px`;
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

  it('paints with the theme\'s match grounds in both modes', () => {
    const dark = getXtermFindColors('dark');
    applyMode('light');
    const light = getXtermFindColors('light');
    for (const grounds of [dark, light]) {
      expect(grounds.match).toMatch(/^#[0-9a-f]{6}$/);
      expect(grounds.current).toMatch(/^#[0-9a-f]{6}$/);
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

/** Each cell's most common color as the page shows it, from a screenshot
 * of the mount: the ground the renderer painted under the glyph. */
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

describe('terminal find on the alternate screen, as the renderer paints it', () => {
  beforeEach(async () => {
    await build();
    term.resize(80, 10);
  });

  it('paints the match grounds on a full-screen program\'s cells, and neither screen\'s highlights on the other', async () => {
    // The production renderer: WebGL draws the cells; there are no DOM rows.
    expect(mount.querySelector('.xterm-screen canvas')).not.toBeNull();
    expect(mount.querySelector('.xterm-rows')).toBeNull();
    const colors = getXtermFindColors('dark');
    // The theme names the ground as rgb(); the screenshot reads hex.
    const [r, g, b] = getXtermTheme('dark').background!.match(/\d+/g)!.map(Number);
    const ground = `#${[r, g, b].map((v) => v.toString(16).padStart(2, '0')).join('')}`;
    expect(colors.match).toMatch(/^#[0-9a-f]{6}$/);

    await write('normal row\r\n   normal needle');
    register();
    setPaneFindQuery(KEY, 'needle');
    let shown = await groundsShown();
    expect(shown(10, 1)).toBe(colors.current);
    expect(shown(0, 1)).toBe(ground);

    // A full-screen program draws two matches; the normal screen's match
    // cell (row 1, column 10) is plain text there.
    await write('\x1b[?1049h\x1b[H\x1b[2Jalt screen\r\nneedle one is here\r\nand needle two');
    shown = await groundsShown();
    expect(shown(10, 1)).toBe(ground);
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, current: 1, pending: false }), { timeout: 2000 });
    shown = await groundsShown();
    expect(shown(0, 1)).toBe(colors.match);
    expect(shown(5, 1)).toBe(colors.match);
    expect(shown(4, 2)).toBe(colors.current);
    expect(shown(9, 2)).toBe(colors.current);
    expect(shown(10, 1)).toBe(ground);
    // Stepping moves the active ground on the program's screen.
    stepPaneFind(KEY, 1);
    shown = await groundsShown();
    expect(shown(0, 1)).toBe(colors.current);
    expect(shown(4, 2)).toBe(colors.match);

    // Leaving it, none of its highlights stays on the normal screen.
    await write('\x1b[?1049l');
    shown = await groundsShown();
    expect(shown(0, 1)).toBe(ground);
    expect(shown(4, 2)).toBe(ground);
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 1, current: 0, pending: false }), { timeout: 2000 });
    shown = await groundsShown();
    expect(shown(10, 1)).toBe(colors.current);
    expect(shown(0, 1)).toBe(ground);
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
    const colors = getXtermFindColors('dark');
    // Three matches on screen: rows 1, 3 and 5 at column 2.
    await write(['zero', '  needle a', 'two', '  needle b', 'four', '  needle c'].join('\r\n'));
    register();
    setPaneFindQuery(KEY, 'needle');
    const expectActive = async (active: number) => {
      const shown = await groundsShown();
      for (const [index, row] of [1, 3, 5].entries()) {
        for (const col of [2, 7]) expect(shown(col, row)).toBe(index === active ? colors.current : colors.match);
      }
      expect(shown(0, 1)).toBe(groundHex());
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

/** Entries in xterm's decoration service, ours included. A test reaches
 * xterm internals here: nothing public counts registered decorations. */
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

  it('leaves no decoration registered in xterm across reflows, re-searches, trimming, clear, reset and screen switches', async () => {
    const baseline = serviceDecorations();
    // A match on each line's first row and one on its continuation row:
    // each 120-character line wraps at 80.
    await write(Array.from({ length: 30 }, (_, i) => `${String(i).padStart(3, '0')} needle ${'.'.repeat(80)} needle ${'.'.repeat(15)}`).join('\r\n'));
    register();
    setPaneFindQuery(KEY, 'needle');
    expect(result().count).toBe(60);
    expect(serviceDecorations() - baseline).toBe(60);
    const settled = () => vi.waitFor(() => expect(result().pending).toBe(false), { timeout: 2000 });
    for (let cycle = 0; cycle < 3; cycle += 1) {
      term.resize(140, 10);
      await settled();
      expect(serviceDecorations() - baseline).toBe(60);
      term.resize(60, 10);
      await settled();
      expect(serviceDecorations() - baseline).toBe(60);
      registration!.restyle();
      expect(serviceDecorations() - baseline).toBe(60);
    }
    term.resize(80, 10);
    await settled();
    // Trimming every match away.
    await write('\r\nfiller'.repeat(4100));
    await settled();
    expect(result().count).toBe(0);
    expect(serviceDecorations()).toBe(baseline);
    await write('\r\nneedle\r\n');
    await settled();
    expect(serviceDecorations() - baseline).toBe(1);
    term.clear();
    await settled();
    expect(serviceDecorations()).toBe(baseline);
    await write('\r\nneedle');
    await settled();
    term.reset();
    await settled();
    expect(serviceDecorations()).toBe(baseline);
    await write('\r\nneedle\x1b[?1049h\x1b[Halt needle');
    await settled();
    expect(result().count).toBe(1);
    expect(serviceDecorations() - baseline).toBe(1);
    await write('\x1b[?1049l');
    await settled();
    expect(result().count).toBe(1);
    expect(serviceDecorations() - baseline).toBe(1);
    closePaneFind(KEY);
    expect(serviceDecorations()).toBe(baseline);
  });
});

/** `term` as the provider sees it, counting what the scan reads and
 * tracking every marker and decoration it makes until disposed. */
function countedTerminal(real: Terminal) {
  const reads = { lines: 0, texts: 0 };
  const made = { markers: 0, decorations: 0 };
  const live = { markers: new Set<IMarker>(), decorations: new Set<IDecoration>() };
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
    registerDecoration(options) {
      const decoration = real.registerDecoration(options);
      if (decoration) {
        made.decorations += 1;
        live.decorations.add(decoration);
        decoration.onDispose(() => live.decorations.delete(decoration));
      }
      return decoration;
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
      made.decorations = 0;
    },
  };
}

// What a re-search costs is the rows it reads and the markers and
// decorations it makes; both are bounded here, at full scrollback.
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
    find = createTerminalFind({ term: counted.term, onBufferChange: built.onBufferChange, onBeforeReflow: built.onBeforeReflow, colors: () => getXtermFindColors('dark') });
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

  it('paints at most the limit plus the active match, a marker a row, and leaves nothing of the previous scan', () => {
    find.search('needle');
    expect(find.result).toMatchObject({ count: TERMINAL_FIND_MATCH_LIMIT, capped: true });
    const first = { markers: new Set(counted.live.markers), decorations: new Set(counted.live.decorations) };
    for (let i = 0; i < 5; i += 1) {
      counted.reset();
      find.restyle();
      expect(counted.made.decorations).toBe(TERMINAL_FIND_MATCH_LIMIT + 1);
      // One marker per painted row, and the active match's two anchors.
      expect(counted.made.markers).toBe(TERMINAL_FIND_MATCH_LIMIT + 2);
      // The active match's own match ground is disposed for its active one.
      expect(counted.live.decorations.size).toBe(TERMINAL_FIND_MATCH_LIMIT);
      expect(counted.live.markers.size).toBe(TERMINAL_FIND_MATCH_LIMIT + 2);
    }
    for (const marker of first.markers) expect(counted.live.markers.has(marker)).toBe(false);
    for (const decoration of first.decorations) expect(counted.live.decorations.has(decoration)).toBe(false);
    find.clear();
    expect(counted.live.markers.size).toBe(0);
    expect(counted.live.decorations.size).toBe(0);
  });
});
