// The thread terminal surface's find bar under the compact layout, over a
// real TerminalBody and xterm in real Chromium at a phone viewport: the bar
// covers the tab strip, not the grid, so no row is hidden and nothing
// resizes; the covered strip takes no taps or focus until the bar closes or
// its tab goes. Desktop placement: TerminalSurface.find.browser.test.ts.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import { page } from 'vitest/browser';
import type { Terminal } from '@xterm/xterm';
import '../../../app.css';
import TerminalSurface from './TerminalSurface.svelte';
import {
  getThreadTerminalState,
  resetThreadTerminalStatesForTest,
  terminalStateKeyForPane,
  type ThreadTerminalStateHandle,
} from './terminalStore.svelte';
import { terminalFindKey } from './terminalFind.svelte';
import { TERMINAL_FIND_ACTIVE_CLASS } from './terminalFindLayer';
import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';
import { endTerminalTab } from '../../stores/eventsTerminal';
import {
  closePaneFind,
  getPaneFindProvider,
  isPaneFindOpen,
  resetPaneFindForTest,
  setPaneFindQuery,
} from '../../stores/paneFind.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from '../../stores/paneLayout.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';

const PANE = 'pane-compact';
const THREAD = 'thread-compact';
const FIRST = 't-compact-1';
const SECOND = 't-compact-2';

let component: ReturnType<typeof mount> | null = null;
let handle: ThreadTerminalStateHandle;
const terms = new Map<string, Terminal>();

function base64(text: string): string {
  return btoa(String.fromCharCode(...new TextEncoder().encode(text)));
}

async function frames(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
}

function surface(): ThreadTerminalSurfaceContext {
  return {
    paneId: PANE,
    threadId: THREAD,
    workspacePath: undefined,
    setVisible: () => {},
    acquireResizeLease: () => null,
    consumeFocusRequest: () => false,
  };
}

function tab(terminalID: string) {
  return { terminalID, threadID: THREAD, shell: '/bin/sh', cwd: '/tmp', rows: 24, cols: 80, pid: 1, startedAt: 0, running: true, exitCode: 0, exitReason: '' };
}

function rect(selector: string): DOMRect {
  return document.querySelector<HTMLElement>(selector)!.getBoundingClientRect();
}

function strip(): HTMLElement {
  return document.querySelector<HTMLElement>('[data-testid="terminal-surface"] > .contents')!;
}

function intersects(a: DOMRect, b: DOMRect): boolean {
  return a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom;
}

async function mountTabs(ids: string[]): Promise<void> {
  handle = getThreadTerminalState(terminalStateKeyForPane(THREAD, PANE));
  for (const id of ids) handle.addTab(tab(id));
  handle.setActive(ids.at(-1)!);
  const attach = handle.attachXterm.bind(handle);
  handle.attachXterm = (terminalID, xterm) => {
    terms.set(terminalID, xterm as Terminal);
    return attach(terminalID, xterm);
  };
  const pane = document.createElement('section');
  pane.dataset.paneId = PANE;
  pane.style.cssText = 'display:flex;flex-direction:column;width:412px;height:700px';
  document.body.appendChild(pane);
  component = mount(TerminalSurface, { target: pane, props: { surface: surface(), manual: true } });
  await vi.waitFor(() => expect(getPaneFindProvider(terminalFindKey(PANE, ids.at(-1)!))).not.toBeNull(), { timeout: 3000 });
  await frames();
}

async function openFind(): Promise<HTMLElement> {
  document.querySelector<HTMLElement>('[data-testid="terminal-find"]')!.click();
  await tick();
  await frames();
  return document.querySelector<HTMLElement>('[data-testid="pane-find-bar"]')!;
}

beforeEach(async () => {
  await page.viewport(412, 839);
  setCompactLayoutForTest(true);
  resetThreadTerminalStatesForTest();
  resetPaneFindForTest();
  terms.clear();
  setPaneLayoutItemsForTest([{ id: PANE, paneId: PANE, kind: 'thread', widthPx: 412 }]);
  setBindingMock('WriteTerminal', async () => {});
  setBindingMock('ResizeTerminal', async () => {});
  // A fresh shell: a match on its first line, a few lines under it.
  setBindingMock('GetTerminalReplay', async () => ({ data: base64('first needle line\r\ntwo\r\nthree\r\n$ '), fromSequence: 0, throughSequence: 1 }));
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  resetPaneFindForTest();
  resetPaneLayoutForTest();
  resetThreadTerminalStatesForTest();
  setCompactLayoutForTest(false);
  document.body.replaceChildren();
});

it('covers the tab strip, not the grid: no row hidden, nothing resized, and the strip back on close', async () => {
  await mountTabs([FIRST]);
  const xterm = terms.get(FIRST)!;
  const resizes = getBindingMock('ResizeTerminal')!;
  const before = { rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length };
  const stripBox = rect('[data-testid="terminal-surface"] > .contents > *');
  expect(strip().inert).toBe(false);

  const bar = (await openFind()).getBoundingClientRect();
  expect(bar.height).toBeGreaterThan(0);
  expect(bar.top).toBeGreaterThanOrEqual(stripBox.top);
  expect(bar.bottom).toBeLessThanOrEqual(stripBox.bottom);
  expect(intersects(bar, rect('.xterm-screen'))).toBe(false);
  expect({ rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length }).toEqual(before);
  expect(strip().inert).toBe(true);

  // The first line's match is the active one, and fully in view.
  setPaneFindQuery(terminalFindKey(PANE, FIRST), 'needle');
  await frames();
  expect(getPaneFindProvider(terminalFindKey(PANE, FIRST))!.result).toMatchObject({ count: 1, current: 0 });
  expect(xterm.buffer.active.viewportY).toBe(0);
  const match = rect(`.${TERMINAL_FIND_ACTIVE_CLASS}`);
  expect(match.top).toBeGreaterThanOrEqual(rect('.xterm-screen').top);
  expect(intersects(match, bar)).toBe(false);

  closePaneFind(terminalFindKey(PANE, FIRST));
  await tick();
  await frames();
  expect(document.querySelector('[data-testid="pane-find-bar"]')).toBeNull();
  expect(strip().inert).toBe(false);
  expect({ rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length }).toEqual(before);
});

it('the covered strip takes no tap and no focus', async () => {
  await mountTabs([FIRST, SECOND]);
  const bar = await openFind();
  const tabs = [...document.querySelectorAll<HTMLElement>('[data-testid="terminal-surface"] [role="tab"]')];
  expect(tabs.length).toBe(2);
  // Every point of the strip, the slivers beside and under the bar
  // included, hit-tests to the bar or to nothing of the strip.
  const stripBox = rect('[data-testid="terminal-surface"] > .contents > *');
  for (const y of [stripBox.top + 1, stripBox.top + stripBox.height / 2, stripBox.bottom - 1]) {
    for (const x of [stripBox.left + 1, stripBox.left + 20, stripBox.right - 2]) {
      const hit = document.elementFromPoint(x, y);
      if (hit && strip().contains(hit)) {
        hit.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      }
    }
  }
  await tick();
  expect(handle.activeTerminalID).toBe(SECOND);
  expect(handle.tabs.map((t) => t.terminalID)).toEqual([FIRST, SECOND]);
  // Tab from the query does not land on a strip control.
  for (const control of strip().querySelectorAll<HTMLElement>('button, [tabindex]')) {
    control.focus();
    expect(strip().contains(document.activeElement)).toBe(false);
  }
  expect(bar.isConnected).toBe(true);
});

it('when the active tab goes with the bar open, its find goes with it and the strip is usable again', async () => {
  await mountTabs([FIRST, SECOND]);
  await openFind();
  expect(strip().inert).toBe(true);
  // The shell exits: the tab ends.
  endTerminalTab(handle, THREAD, SECOND);
  await tick();
  await frames();
  expect(handle.activeTerminalID).toBe(FIRST);
  expect(isPaneFindOpen(terminalFindKey(PANE, SECOND))).toBe(false);
  expect(document.querySelector('[data-testid="pane-find-bar"]')).toBeNull();
  expect(strip().inert).toBe(false);
});
