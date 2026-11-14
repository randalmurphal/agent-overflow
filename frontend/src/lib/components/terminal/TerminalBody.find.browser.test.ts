// A kept query restored into a remounted TerminalBody, in real Chromium with
// a real xterm: the replay parses asynchronously, and the restored search
// must end as a fresh one does, its active match painted and in view.
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import type { Terminal } from '@xterm/xterm';
import '../../../app.css';
import TerminalBody from './TerminalBody.svelte';
import { createThreadTerminalState } from './terminalStore.svelte';
import { terminalFindKey } from './terminalFind.svelte';
import { TERMINAL_FIND_ACTIVE_CLASS } from './terminalFindLayer';
import {
  getPaneFindProvider,
  openPaneFind,
  registerPaneFindProvider,
  resetPaneFindForTest,
  setPaneFindQuery,
} from '../../stores/paneFind.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import type { TerminalSessionSummary } from '../../types/terminal';

const TERMINAL = 't-restore';
const KEY = terminalFindKey('pane-restore', TERMINAL);
const NEEDLE_ROWS = [10, 250];
const LINES = 300;

let component: ReturnType<typeof mount> | null = null;

function summary(): TerminalSessionSummary {
  return {
    terminalID: TERMINAL,
    threadID: 'thread-1',
    shell: '/bin/bash',
    cwd: '/tmp',
    // Taller than the mount, so the body's fit after the restored search
    // changes the rows.
    rows: 40,
    cols: 80,
    pid: 1,
    startedAt: 0,
    running: true,
    exitCode: 0,
    exitReason: '',
  };
}

function base64(text: string): string {
  const bytes = new TextEncoder().encode(text);
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary);
}

function replayLines(): string[] {
  return Array.from({ length: LINES }, (_, i) => (NEEDLE_ROWS.includes(i) ? `row ${i} has the Needle in it` : `row ${i}`));
}

beforeEach(() => {
  resetPaneFindForTest();
  setBindingMock('WriteTerminal', async () => {});
  setBindingMock('ResizeTerminal', async () => {});
});

afterEach(() => {
  if (component) unmount(component);
  component = null;
  resetPaneFindForTest();
  document.body.replaceChildren();
});

it('restores a kept query after the replay is parsed, with its newest match painted and in view', async () => {
  // The bar was open with a query when the tab's previous body went.
  const release = registerPaneFindProvider(KEY, { noun: 'terminal', result: { count: 0, current: -1, pending: false }, search: () => {}, clear: () => {} });
  openPaneFind(KEY);
  setPaneFindQuery(KEY, 'needle');
  release();

  const handle = createThreadTerminalState();
  handle.addTab(summary());
  let term: Terminal | null = null;
  const attach = handle.attachXterm.bind(handle);
  handle.attachXterm = (terminalID, xterm) => {
    term = xterm as Terminal;
    return attach(terminalID, xterm);
  };
  const target = document.createElement('div');
  target.style.cssText = 'display:flex;flex-direction:column;width:640px;height:200px';
  document.body.appendChild(target);
  // The replay answers just after a frame, as an RPC can: it parses well
  // before the next frame, so the body's own scheduled fit comes after the
  // restored search.
  setBindingMock('GetTerminalReplay', async () => {
    await new Promise((resolve) => requestAnimationFrame(resolve));
    await new Promise((resolve) => requestAnimationFrame(resolve));
    return { data: base64(replayLines().join('\r\n')), fromSequence: 0, throughSequence: 1 };
  });
  component = mount(TerminalBody, { target, props: { handle, terminalID: TERMINAL, paneId: 'pane-restore', findKey: KEY } });
  await vi.waitFor(() => expect(getPaneFindProvider(KEY)?.noun).toBe('terminal'), { timeout: 2000 });
  const restored = term as Terminal | null;
  expect(restored).not.toBeNull();
  const xterm = restored!;
  // The parse that registered find also re-counts it once; that re-search
  // keeps the match and the view.
  await vi.waitFor(() => expect(getPaneFindProvider(KEY)!.result).toMatchObject({ count: 2, current: 1, pending: false }), { timeout: 2000 });
  expect(xterm.rows).toBeLessThan(40);
  const top = xterm.buffer.active.viewportY;
  expect(NEEDLE_ROWS[1] >= top && NEEDLE_ROWS[1] < top + xterm.rows).toBe(true);
  // Painted on its row, which is on screen.
  await vi.waitFor(() => {
    const active = [...target.querySelectorAll<HTMLElement>(`.${TERMINAL_FIND_ACTIVE_CLASS}`)].filter((el) => el.style.display !== 'none');
    expect(active.length).toBe(1);
    const height = parseFloat(active[0].style.height);
    expect(Math.round(parseFloat(active[0].style.top) / height)).toBe(NEEDLE_ROWS[1] - top);
  });
  expect(xterm.hasSelection()).toBe(false);
});
