// The thread terminal surface's find bar over a real TerminalBody and xterm
// in real Chromium: the bar floats over the grid, so opening and closing it
// changes neither the terminal's rows and columns nor the PTY's size, and
// leaves the reader's selection alone; a match under the bar is scrolled
// clear of it.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import type { Terminal } from '@xterm/xterm';
import '../../../app.css';
import TerminalSurface from './TerminalSurface.svelte';
import {
  getThreadTerminalState,
  resetThreadTerminalStatesForTest,
  terminalStateKeyForPane,
} from './terminalStore.svelte';
import { terminalFindKey } from './terminalFind.svelte';
import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';
import {
  closePaneFind,
  getPaneFindProvider,
  resetPaneFindForTest,
  setPaneFindQuery,
} from '../../stores/paneFind.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';

const PANE = 'pane-overlay';
const TERMINAL = 't-overlay';
const KEY = terminalFindKey(PANE, TERMINAL);
const NEEDLE_ROW = 250;

let component: ReturnType<typeof mount> | null = null;
let term: Terminal | null = null;

function base64(text: string): string {
  return btoa(String.fromCharCode(...new TextEncoder().encode(text)));
}

async function frames(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
}

function surface(): ThreadTerminalSurfaceContext {
  return {
    paneId: PANE,
    threadId: 'thread-overlay',
    workspacePath: undefined,
    setVisible: () => {},
    acquireResizeLease: () => null,
    consumeFocusRequest: () => false,
  };
}

beforeEach(async () => {
  resetThreadTerminalStatesForTest();
  resetPaneFindForTest();
  setPaneLayoutItemsForTest([{ id: PANE, paneId: PANE, kind: 'thread', widthPx: 640 }]);
  setBindingMock('WriteTerminal', async () => {});
  setBindingMock('ResizeTerminal', async () => {});
  const lines = Array.from({ length: 300 }, (_, i) => (i === NEEDLE_ROW ? `row ${i} has the needle` : `row ${i}`));
  setBindingMock('GetTerminalReplay', async () => ({ data: base64(lines.join('\r\n')), fromSequence: 0, throughSequence: 1 }));
  const handle = getThreadTerminalState(terminalStateKeyForPane('thread-overlay', PANE));
  handle.addTab({ terminalID: TERMINAL, threadID: 'thread-overlay', shell: '/bin/bash', cwd: '/tmp', rows: 24, cols: 80, pid: 1, startedAt: 0, running: true, exitCode: 0, exitReason: '' });
  handle.setActive(TERMINAL);
  const attach = handle.attachXterm.bind(handle);
  handle.attachXterm = (terminalID, xterm) => {
    term = xterm as Terminal;
    return attach(terminalID, xterm);
  };
  const pane = document.createElement('section');
  pane.dataset.paneId = PANE;
  pane.style.cssText = 'display:flex;flex-direction:column;width:640px;height:300px';
  document.body.appendChild(pane);
  component = mount(TerminalSurface, { target: pane, props: { surface: surface(), manual: true } });
  await vi.waitFor(() => expect(getPaneFindProvider(KEY)).not.toBeNull(), { timeout: 3000 });
  await frames();
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  term = null;
  resetPaneFindForTest();
  resetPaneLayoutForTest();
  resetThreadTerminalStatesForTest();
  document.body.replaceChildren();
});

function openFind(): void {
  document.querySelector<HTMLElement>('[data-testid="terminal-find"]')!.click();
}

it('opening and closing find keeps the grid, the PTY size and the reader\'s selection', async () => {
  const xterm = term!;
  const resizes = getBindingMock('ResizeTerminal')!;
  const before = { rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length };
  expect(before.rows).toBeGreaterThan(5);
  xterm.select(0, xterm.buffer.active.viewportY + 1, 5);
  const selected = { text: xterm.getSelection(), at: xterm.getSelectionPosition() };
  expect(selected.text).toHaveLength(5);

  openFind();
  await tick();
  const bar = document.querySelector<HTMLElement>('[data-testid="pane-find-bar"]')!;
  expect(bar).not.toBeNull();
  expect(document.activeElement).toBe(bar.querySelector('input'));
  await frames();
  expect({ rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length }).toEqual(before);
  expect({ text: xterm.getSelection(), at: xterm.getSelectionPosition() }).toEqual(selected);
  // It floats over the terminal's top right.
  const host = document.querySelector<HTMLElement>('[data-terminal-find-host]')!.getBoundingClientRect();
  const box = bar.getBoundingClientRect();
  expect(box.top).toBeGreaterThanOrEqual(host.top);
  expect(box.right).toBeLessThanOrEqual(host.right);
  expect(box.width).toBeLessThan(host.width / 2 + 100);

  closePaneFind(KEY);
  await tick();
  await frames();
  expect(document.querySelector('[data-testid="pane-find-bar"]')).toBeNull();
  expect({ rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length }).toEqual(before);
  expect({ text: xterm.getSelection(), at: xterm.getSelectionPosition() }).toEqual(selected);
});

it('a match on the rows under the bar is scrolled clear of it', async () => {
  const xterm = term!;
  // The match on the viewport's top row, where the bar sits.
  xterm.scrollToLine(NEEDLE_ROW);
  await frames();
  openFind();
  await tick();
  await frames();
  setPaneFindQuery(KEY, 'needle');
  expect(getPaneFindProvider(KEY)!.result).toMatchObject({ count: 1, current: 0 });
  const grid = document.querySelector<HTMLElement>('.xterm-screen')!.getBoundingClientRect();
  const bar = document.querySelector<HTMLElement>('[data-testid="pane-find-bar"]')!.getBoundingClientRect();
  const rowHeight = grid.height / xterm.rows;
  const matchTop = grid.top + (NEEDLE_ROW - xterm.buffer.active.viewportY) * rowHeight;
  expect(NEEDLE_ROW - xterm.buffer.active.viewportY).toBeLessThan(xterm.rows);
  expect(matchTop).toBeGreaterThanOrEqual(bar.bottom);
});
