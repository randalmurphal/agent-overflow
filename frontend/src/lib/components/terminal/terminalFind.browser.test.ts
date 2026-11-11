// Terminal find against a real xterm and its real search addon, built the
// way every terminal surface builds them (terminalXterm.buildTerminal), in
// real Chromium: the active match is selected, scrolled into view and
// decorated with the theme's match grounds; steps wrap; the count follows
// new output; a close removes it all; a full-screen program's screen is all
// there is to search.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Terminal } from '@xterm/xterm';
import '../../../app.css';
import { applyThemeClass } from '../../utils/theme';
import { applyTheme, resetThemeApplyForTest } from '../../theme/themeApply.svelte';
import { buildTerminal } from './terminalXterm';
import { getXtermFindDecorations, resetXtermThemeCache } from './terminalTheme';
import { registerTerminalFind, terminalFindKey, type TerminalFindRegistration } from './terminalFind.svelte';
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

function decorations(): HTMLElement[] {
  return [...mount.querySelectorAll<HTMLElement>('.xterm-find-result-decoration')].filter((el) => el.style.display !== 'none');
}

/** The rendered find decorations on `row`, which must be on screen. */
function decorationsOnRow(row: number): HTMLElement[] {
  const offset = row - term.buffer.active.viewportY;
  return decorations().filter((el) => Math.round(parseFloat(el.style.top) / parseFloat(el.style.height)) === offset);
}

beforeEach(async () => {
  priorClass = document.documentElement.className;
  resetXtermThemeCache();
  resetThemeApplyForTest();
  resetPaneFindForTest();
  applyMode('dark');
  mount = document.createElement('div');
  mount.style.cssText = 'width:640px;height:200px';
  document.body.appendChild(mount);
  const built = buildTerminal(mount, { onInput: () => {} });
  term = built.term;
  const lines = Array.from({ length: LINES }, (_, i) => (NEEDLE_ROWS.includes(i) ? `row ${i} has the Needle in it` : `row ${i}`));
  await write(lines.join('\r\n'));
  registration = registerTerminalFind(KEY, term, built.search);
  expect(openPaneFind(KEY)).toBe(true);
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
  it('starts at the newest match, selected, scrolled into view and decorated', async () => {
    expect(inView(NEEDLE_ROWS[1])).toBe(false);
    setPaneFindQuery(KEY, '  needle ');
    expect(result()).toMatchObject({ count: 2, current: 1, pending: false, error: null });
    expect(inView(NEEDLE_ROWS[1])).toBe(true);
    expect(term.getSelectionPosition()).toMatchObject({ start: { x: 16, y: NEEDLE_ROWS[1] } });
    expect(term.getSelection()).toBe('Needle');
    await frames();
    // The match ground, and the current match's over it.
    expect(decorationsOnRow(NEEDLE_ROWS[1]).length).toBe(2);
  });

  it('steps down and up, wrapping, and scrolls each match into view', async () => {
    setPaneFindQuery(KEY, 'needle');
    stepPaneFind(KEY, 1);
    expect(result().current).toBe(0);
    expect(inView(NEEDLE_ROWS[0])).toBe(true);
    expect(term.getSelectionPosition()?.start.y).toBe(NEEDLE_ROWS[0]);
    await frames();
    expect(decorationsOnRow(NEEDLE_ROWS[0]).length).toBe(2);
    stepPaneFind(KEY, 1);
    expect(result().current).toBe(1);
    expect(inView(NEEDLE_ROWS[1])).toBe(true);
    stepPaneFind(KEY, -1);
    expect(result().current).toBe(0);
    stepPaneFind(KEY, -1);
    expect(result().current).toBe(1);
  });

  it('reports no match and selects nothing', () => {
    setPaneFindQuery(KEY, 'absent');
    expect(result()).toMatchObject({ count: 0, current: -1 });
    expect(term.hasSelection()).toBe(false);
  });

  it('the count follows new output, pending until the addon searches again', async () => {
    setPaneFindQuery(KEY, 'needle');
    expect(result().count).toBe(2);
    await write('\r\nnew output with a needle');
    expect(result()).toMatchObject({ count: 2, pending: true });
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 3, pending: false }), { timeout: 2000 });
  });

  it('a close clears the decorations and the selection, and later output searches nothing', async () => {
    setPaneFindQuery(KEY, 'needle');
    await frames();
    expect(decorations().length).toBeGreaterThan(0);
    closePaneFind(KEY);
    expect(term.hasSelection()).toBe(false);
    await frames();
    expect(decorations().length).toBe(0);
    await write('\r\nanother needle');
    await new Promise((resolve) => setTimeout(resolve, 400));
    await frames();
    expect(result()).toMatchObject({ count: 0, current: -1, pending: false });
    expect(decorations().length).toBe(0);
  });

  it('in a full-screen program searches only the visible screen', async () => {
    await write('\x1b[?1049h\x1b[Hscreen needle');
    setPaneFindQuery(KEY, 'needle');
    expect(result()).toMatchObject({ count: 1, current: 0 });
    await write('\x1b[?1049l');
    await vi.waitFor(() => expect(result()).toMatchObject({ count: 2, pending: false }), { timeout: 2000 });
  });

  it('paints with the theme\'s match grounds in both modes', () => {
    const dark = getXtermFindDecorations('dark');
    applyMode('light');
    const light = getXtermFindDecorations('light');
    for (const grounds of [dark, light]) {
      expect(grounds.matchBackground).toMatch(/^#[0-9a-f]{6}$/);
      expect(grounds.activeMatchBackground).toMatch(/^#[0-9a-f]{6}$/);
      expect(grounds.activeMatchBackground).not.toBe(grounds.matchBackground);
    }
    expect(light.matchBackground).not.toBe(dark.matchBackground);
  });
});
