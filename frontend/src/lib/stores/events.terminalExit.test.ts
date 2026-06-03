import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { setupEventListeners } from './events';
import { prependThread, getThreadById, refreshThreads } from './threads.svelte';
import { getToasts } from './toast.svelte';
import {
  getThreadTerminalState,
  resetThreadTerminalStatesForTest,
} from '../components/terminal/terminalStore.svelte';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import type { Thread } from '../types/models';
import type { TerminalSessionSummary } from '../types/terminal';

// Exercises events.ts `applyTerminalExit` — the single seam for issues #2 (the
// pane must close) and #3 (the sidebar must clear) when a terminal's last shell
// exits. The delete decision lives here; the pane teardown delegates to
// closePanesShowingThread (covered by panes.svelte tests). The most important
// case is the negative one: a CHAT thread's drawer terminal exiting must not
// delete the chat thread.

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'term-1',
    title: 'home',
    provider: 'claude',
    workspacePath: '/home/me',
    projectPath: '',
    mode: 'terminal',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

function makeSummary(terminalID: string, threadID: string): TerminalSessionSummary {
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

function fireExit(threadID: string, terminalID: string): void {
  emitWailsEvent('terminal:exit', { terminalID, threadID, code: 0, reason: '' });
}

let cleanupEvents: (() => void) | null = null;
let deleteThread: ReturnType<typeof setBindingMock>;
// Set only by the failure-path test, which expects a console.error from the
// rejected delete; restored in afterEach so it never leaks to other tests.
let errorSpy: ReturnType<typeof vi.spyOn> | null = null;

beforeEach(async () => {
  resetThreadTerminalStatesForTest();
  resetBindingMocks();
  // Reset the threads store to a known-empty baseline.
  setBindingMock('ListThreads', async () => []);
  await refreshThreads();
  deleteThread = setBindingMock('DeleteThread', async () => undefined);
  cleanupEvents = setupEventListeners();
});

afterEach(() => {
  cleanupEvents?.();
  cleanupEvents = null;
  errorSpy?.mockRestore();
  errorSpy = null;
});

describe('applyTerminalExit — terminal-thread lifecycle (#2/#3)', () => {
  it('removes the terminal thread when its last shell exits', async () => {
    prependThread(makeThread({ id: 'term-1', mode: 'terminal' }));
    const handle = getThreadTerminalState('term-1');
    handle.addTab(makeSummary('t1', 'term-1'));

    fireExit('term-1', 't1');

    // Tab handle drained and the backend delete invoked, both synchronously
    // (removeTab and the DeleteThread call run before the await suspends).
    expect(handle.tabs).toHaveLength(0);
    expect(deleteThread).toHaveBeenCalledTimes(1);
    expect(deleteThread).toHaveBeenCalledWith('term-1');
    // The sidebar row is dropped only AFTER DeleteThread resolves, so the
    // store removal is async now — a failed RPC must leave the row.
    await vi.waitFor(() => expect(getThreadById('term-1')).toBeUndefined());
  });

  it('keeps the row and toasts when the backend delete fails (no ghost on refresh)', async () => {
    errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    prependThread(makeThread({ id: 'term-1', mode: 'terminal' }));
    const handle = getThreadTerminalState('term-1');
    handle.addTab(makeSummary('t1', 'term-1'));
    setBindingMock('DeleteThread', async () => {
      throw new Error('db is locked');
    });

    fireExit('term-1', 't1');

    // The delete was attempted and the tab drained, but because it rejected the
    // row must stay (removing it then re-listing would resurrect a shell-less
    // ghost) and the failure surfaces as a toast, not a silent console.error.
    await vi.waitFor(() => {
      expect(getThreadById('term-1')).toBeDefined();
      expect(getToasts().some((t) => t.message.includes('Could not remove terminal'))).toBe(true);
    });
    expect(handle.tabs).toHaveLength(0);
  });

  it('does NOT delete a chat thread whose drawer terminal exits (mode guard)', () => {
    prependThread(makeThread({ id: 'chat-1', mode: 'chat' }));
    const handle = getThreadTerminalState('chat-1');
    handle.addTab(makeSummary('t1', 'chat-1'));

    fireExit('chat-1', 't1');

    // The chat thread survives — only its terminal tab is removed.
    expect(getThreadById('chat-1')).toBeDefined();
    expect(deleteThread).not.toHaveBeenCalled();
    expect(handle.tabs).toHaveLength(0);
  });

  it('keeps the terminal thread while other tabs remain (only the last exit deletes)', async () => {
    prependThread(makeThread({ id: 'term-1', mode: 'terminal' }));
    const handle = getThreadTerminalState('term-1');
    handle.addTab(makeSummary('t1', 'term-1'));
    handle.addTab(makeSummary('t2', 'term-1'));

    // First of two shells exits → thread stays (synchronous early-return,
    // tabs.length still > 0 so the await is never reached).
    fireExit('term-1', 't1');
    expect(getThreadById('term-1')).toBeDefined();
    expect(deleteThread).not.toHaveBeenCalled();

    // Last shell exits → thread is removed once DeleteThread resolves.
    fireExit('term-1', 't2');
    expect(deleteThread).toHaveBeenCalledTimes(1);
    expect(deleteThread).toHaveBeenCalledWith('term-1');
    await vi.waitFor(() => expect(getThreadById('term-1')).toBeUndefined());
  });

  it('is a no-op when the terminal thread is already gone (no double delete)', () => {
    // No thread seeded — mimics an exit arriving after an explicit delete
    // already removed the row from the store.
    const handle = getThreadTerminalState('term-gone');
    handle.addTab(makeSummary('t1', 'term-gone'));

    fireExit('term-gone', 't1');

    expect(deleteThread).not.toHaveBeenCalled();
    expect(handle.tabs).toHaveLength(0);
  });
});
