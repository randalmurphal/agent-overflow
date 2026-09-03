import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { setupEventListeners } from './events';
import {
  getExistingThreadTerminalState,
  getThreadTerminalState,
  resetThreadTerminalStatesForTest,
} from '../components/terminal/terminalStore.svelte';
import { resetPanesForTest } from './panes.svelte';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import { refreshThreads } from './threads.svelte';
import type { TerminalSessionSummary } from '../types/terminal';

// `terminal:opened` is the convergence half of the terminal surface. Before it,
// OpenTerminal answered its caller and told nobody: a second device had read
// the set once at mount, so it dropped the new terminal's output as belonging
// to an id it had never seen, and an empty surface auto-opened a SECOND shell.

function summary(terminalID: string, threadID: string): TerminalSessionSummary {
  return {
    terminalID,
    threadID,
    shell: '/bin/bash',
    cwd: '/home/me',
    rows: 24,
    cols: 80,
    pid: 1,
    startedAt: 0,
    running: true,
    exitCode: 0,
    exitReason: '',
  };
}

function fireOpened(threadID: string, terminalID: string): void {
  emitWailsEvent('terminal:opened', {
    terminalID,
    threadID,
    summary: summary(terminalID, threadID),
  });
}

let cleanupEvents: (() => void) | null = null;

beforeEach(async () => {
  resetThreadTerminalStatesForTest();
  resetPanesForTest();
  resetBindingMocks();
  setBindingMock('ListThreads', async () => []);
  await refreshThreads();
  cleanupEvents = setupEventListeners();
});

afterEach(() => {
  cleanupEvents?.();
  cleanupEvents = null;
  resetThreadTerminalStatesForTest();
});

describe('applyTerminalOpened', () => {
  it('lands the new tab on a client that already has the surface open', () => {
    const handle = getThreadTerminalState('term-1');
    expect(handle.tabs).toHaveLength(0);

    fireOpened('term-1', 't1');

    expect(handle.tabs.map((tab) => tab.terminalID)).toEqual(['t1']);
    expect(handle.activeTerminalID).toBe('t1');
  });

  // The opener applies the same summary from its own RPC answer, so its echo
  // must be a no-op rather than a duplicate tab.
  it('is idempotent, so the opening client’s own echo changes nothing', () => {
    const handle = getThreadTerminalState('term-1');
    handle.addTab(summary('t1', 'term-1'));

    fireOpened('term-1', 't1');

    expect(handle.tabs).toHaveLength(1);
  });

  it('converges a second tab beside the first without taking the active one', () => {
    const handle = getThreadTerminalState('term-1');
    handle.addTab(summary('t1', 'term-1'));

    fireOpened('term-1', 't2');

    expect(handle.tabs.map((tab) => tab.terminalID)).toEqual(['t1', 't2']);
    // Which tab this person is typing into is this client's own state: a
    // shell opened from another device lands beside it, never over it.
    expect(handle.activeTerminalID).toBe('t1');
  });

  // A client with no surface for the thread has nothing to show, and holding a
  // tab list nobody is reading is retention with no reader. Its next mount
  // reads the list anyway.
  it('creates no terminal state for a thread this client is not showing', () => {
    fireOpened('term-2', 't9');

    expect(getExistingThreadTerminalState('term-2')).toBeNull();
  });

  it('ignores a frame missing its thread or its summary', () => {
    const handle = getThreadTerminalState('term-1');
    emitWailsEvent('terminal:opened', { terminalID: 't1', threadID: '', summary: null });
    emitWailsEvent('terminal:opened', { terminalID: '', threadID: 'term-1', summary: null });

    expect(handle.tabs).toHaveLength(0);
  });
});
