// The take-control mirror's find bar over a real xterm in real Chromium: the
// bar floats over the grid, so opening and closing it changes neither the
// mirror's rows and columns nor the provider PTY's size, and leaves the
// reader's selection alone.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';
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
import { closePaneFind, getPaneFindProvider, resetPaneFindForTest } from '../../stores/paneFind.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';

const PANE = 'tc-overlay';

let component: ReturnType<typeof mount> | null = null;

function base64(text: string): string {
  return btoa(String.fromCharCode(...new TextEncoder().encode(text)));
}

async function frames(): Promise<void> {
  for (let i = 0; i < 3; i += 1) await new Promise((resolve) => requestAnimationFrame(() => resolve(null)));
}

beforeEach(async () => {
  resetPaneFindForTest();
  captured.term = null;
  setBindingMock('ProviderTerminalAttach', async () => ({
    terminalID: 'term-overlay',
    threadID: 'thread-overlay',
    summary: { cols: 80, rows: 24 },
  }));
  const lines = Array.from({ length: 60 }, (_, i) => `row ${i} of the provider`);
  setBindingMock('ProviderTerminalReplay', async () => ({ data: base64(lines.join('\r\n')), fromSequence: 0, throughSequence: 1 }));
  setBindingMock('ProviderTerminalResize', async () => {});
  setBindingMock('ProviderTerminalInput', async () => {});
  setBindingMock('ProviderTerminalSetControl', async () => {});
  setBindingMock('ProviderTerminalDetach', async () => {});
  const pane = document.createElement('section');
  pane.dataset.paneId = PANE;
  pane.style.cssText = 'display:flex;flex-direction:column;width:640px;height:300px';
  document.body.appendChild(pane);
  component = mount(TakeControlTerminal, { target: pane, props: { paneId: PANE, threadId: 'thread-overlay' } });
  await vi.waitFor(() => expect(getPaneFindProvider(PANE)).not.toBeNull(), { timeout: 3000 });
  await frames();
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  captured.term = null;
  resetPaneFindForTest();
  document.body.replaceChildren();
});

it('opening and closing find keeps the grid, the provider PTY size and the reader\'s selection', async () => {
  const xterm = captured.term!;
  const resizes = getBindingMock('ProviderTerminalResize')!;
  const before = { rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length };
  expect(before.rows).toBeGreaterThan(5);
  expect(before.calls).toBeGreaterThan(0);
  xterm.select(0, xterm.buffer.active.viewportY + 1, 5);
  const selected = { text: xterm.getSelection(), at: xterm.getSelectionPosition() };
  expect(selected.text).toHaveLength(5);

  document.querySelector<HTMLElement>('[data-testid="take-control-find"]')!.click();
  await tick();
  const bar = document.querySelector<HTMLElement>('[data-testid="pane-find-bar"]')!;
  expect(bar).not.toBeNull();
  expect(document.activeElement).toBe(bar.querySelector('input'));
  await frames();
  expect({ rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length }).toEqual(before);
  expect({ text: xterm.getSelection(), at: xterm.getSelectionPosition() }).toEqual(selected);
  // It floats over the mirror's top.
  const grid = document.querySelector<HTMLElement>('[data-testid="take-control-mount"]')!.getBoundingClientRect();
  const box = bar.getBoundingClientRect();
  expect(box.top).toBeGreaterThanOrEqual(grid.top);
  expect(box.bottom).toBeLessThan(grid.bottom);

  closePaneFind(PANE);
  await tick();
  await frames();
  expect(document.querySelector('[data-testid="pane-find-bar"]')).toBeNull();
  expect({ rows: xterm.rows, cols: xterm.cols, calls: resizes.mock.calls.length }).toEqual(before);
  expect({ text: xterm.getSelection(), at: xterm.getSelectionPosition() }).toEqual(selected);
});
