// The take-control mirror's find bar under the compact layout, over a real
// xterm in real Chromium at a phone viewport: the bar covers the mirror's
// header row, not the grid, so no row is hidden and nothing resizes; the
// covered row takes no taps or focus until the bar closes. Desktop
// placement: TakeControlTerminal.find.browser.test.ts.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import { page } from 'vitest/browser';
import type { Terminal } from '@xterm/xterm';
import '../../../app.css';

const captured = vi.hoisted(() => ({ term: null as Terminal | null }));

vi.mock('../terminal/terminalXterm', async (importOriginal) => {
  const original = await importOriginal<typeof import('../terminal/terminalXterm')>();
  return {
    ...original,
    buildTerminal: (...args: Parameters<typeof original.buildTerminal>) => {
      const built = original.buildTerminal(...args);
      captured.term = built.term;
      return built;
    },
  };
});

import TakeControlTerminal from './TakeControlTerminal.svelte';
import { TERMINAL_FIND_ACTIVE_CLASS } from '../terminal/terminalFindLayer';
import { closePaneFind, getPaneFindProvider, resetPaneFindForTest, setPaneFindQuery } from '../../stores/paneFind.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';

const PANE = 'tc-compact';

let component: ReturnType<typeof mount> | null = null;

function base64(text: string): string {
  return btoa(String.fromCharCode(...new TextEncoder().encode(text)));
}

async function frames(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
}

function rect(selector: string): DOMRect {
  return document.querySelector<HTMLElement>(selector)!.getBoundingClientRect();
}

function intersects(a: DOMRect, b: DOMRect): boolean {
  return a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;
}

beforeEach(async () => {
  await page.viewport(412, 839);
  setCompactLayoutForTest(true);
  resetPaneFindForTest();
  captured.term = null;
  setBindingMock('ProviderTerminalAttach', async () => ({ terminalID: 'term-compact', threadID: 'thread-compact', summary: { cols: 0, rows: 0 } }));
  // A fresh session: a match on its first line.
  setBindingMock('ProviderTerminalReplay', async () => ({ data: base64('first needle line\r\ntwo\r\nthree'), fromSequence: 0, throughSequence: 1 }));
  setBindingMock('ProviderTerminalResize', async () => {});
  setBindingMock('ProviderTerminalInput', async () => {});
  setBindingMock('ProviderTerminalSetControl', async () => {});
  setBindingMock('ProviderTerminalDetach', async () => {});
  const pane = document.createElement('section');
  pane.dataset.paneId = PANE;
  pane.style.cssText = 'display:flex;flex-direction:column;width:412px;height:700px';
  document.body.appendChild(pane);
  component = mount(TakeControlTerminal, { target: pane, props: { paneId: PANE, threadId: 'thread-compact' } });
  await vi.waitFor(() => expect(getPaneFindProvider(PANE)).not.toBeNull(), { timeout: 3000 });
  await frames();
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  captured.term = null;
  resetPaneFindForTest();
  setCompactLayoutForTest(false);
  document.body.replaceChildren();
});

it('covers the header row, not the grid: no row hidden, nothing resized, and the row back on close', async () => {
  const xterm = captured.term!;
  const resizes = getBindingMock('ProviderTerminalResize')!;
  const before = { rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length };
  const header = document.querySelector<HTMLElement>('[data-testid="take-control-header"]')!;
  const headerBox = header.getBoundingClientRect();
  expect(header.inert).toBe(false);

  document.querySelector<HTMLElement>('[data-testid="take-control-find"]')!.click();
  await tick();
  await frames();
  const bar = rect('[data-testid="pane-find-bar"]');
  expect(bar.height).toBeGreaterThan(0);
  expect(bar.top).toBeGreaterThanOrEqual(headerBox.top);
  expect(bar.bottom).toBeLessThanOrEqual(headerBox.bottom);
  expect(intersects(bar, rect('.xterm-screen'))).toBe(false);
  expect({ rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length }).toEqual(before);
  expect(header.inert).toBe(true);

  setPaneFindQuery(PANE, 'needle');
  await frames();
  expect(getPaneFindProvider(PANE)!.result).toMatchObject({ count: 1, current: 0 });
  expect(xterm.buffer.active.viewportY).toBe(0);
  expect(intersects(rect(`.${TERMINAL_FIND_ACTIVE_CLASS}`), bar)).toBe(false);

  closePaneFind(PANE);
  await tick();
  await frames();
  expect(header.inert).toBe(false);
  expect({ rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length }).toEqual(before);
});
