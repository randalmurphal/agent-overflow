import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { setupEventListeners } from './events';
import { prependThread, getThreadById, refreshThreads } from './threads.svelte';
import {
  getExistingThreadTerminalState,
  getThreadTerminalState,
  notifyTerminalFocus,
  resetTerminalFocusForTest,
  resetThreadTerminalStatesForTest,
} from '../components/terminal/terminalStore.svelte';
import { createPane, getPane, resetPanesForTest } from './panes.svelte';
import type { ThreadPane } from './thread.svelte';
import { setBindingMock, resetBindingMocks } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import type { Thread } from '../types/models';
import type { TerminalSessionSummary } from '../types/terminal';
import { noteThread } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { TransportError, transportGapChannel } from '../transport/wsClient';
import { getToasts, removeToast } from './toast.svelte';

// Exercises events.ts `applyTerminalExit`: a terminal's exit removes its tab
// and moves focus to the next one. The thread is not the client's to end: a
// terminal thread whose last shell exits is deleted by its computer, and that
// `thread:updated` deletion closes its panes on every client.

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

// Register a real pane showing `thread` and (optionally) mark its terminal as
// holding DOM focus, mirroring TerminalBody's notifyTerminalFocus on xterm focus.
// Named distinctly from the production `panesShowingThread` query it exercises.
function mountPaneShowingThread(
  paneId: string,
  thread: Thread,
  opts: { focused?: boolean } = {},
): ThreadPane {
  const pane = createPane(paneId);
  pane.replaceThread(thread);
  if (opts.focused) notifyTerminalFocus(paneId, true);
  return pane;
}

let cleanupEvents: (() => void) | null = null;
let deleteThread: ReturnType<typeof setBindingMock>;

beforeEach(async () => {
  resetThreadTerminalStatesForTest();
  resetPanesForTest();
  resetTerminalFocusForTest();
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
});

describe('applyTerminalExit removes the tab and leaves the thread to its computer', () => {
  it.each(['terminal', 'chat'] as const)('removes the last tab of a %s thread without deleting it', (mode) => {
    const thread = makeThread({ id: 'thread-1', mode });
    prependThread(thread);
    const handle = getThreadTerminalState('thread-1');
    handle.addTab(makeSummary('t1', 'thread-1'));
    const pane = mountPaneShowingThread('p1', thread);

    fireExit('thread-1', 't1');

    expect(handle.tabs).toHaveLength(0);
    expect(deleteThread).not.toHaveBeenCalled();
    expect(getThreadById('thread-1')).toBeDefined();
    expect(getPane('p1')).toBe(pane);
  });

  it('keeps the other tabs when one of several exits', () => {
    prependThread(makeThread({ id: 'term-1', mode: 'terminal' }));
    const handle = getThreadTerminalState('term-1');
    handle.addTab(makeSummary('t1', 'term-1'));
    handle.addTab(makeSummary('t2', 'term-1'));

    fireExit('term-1', 't1');

    expect(handle.tabs.map((tab) => tab.terminalID)).toEqual(['t2']);
    expect(deleteThread).not.toHaveBeenCalled();
  });
});

describe('the computer\'s deletion of a terminal thread', () => {
  it('drops the row, closes every pane showing it and releases its terminal state', () => {
    const thread = makeThread({ id: 'term-1', mode: 'terminal' });
    prependThread(thread);
    getThreadTerminalState('term-1').addTab(makeSummary('t1', 'term-1'));
    mountPaneShowingThread('p1', thread, { focused: true });
    mountPaneShowingThread('p2', thread);
    const other = mountPaneShowingThread('p3', makeThread({ id: 'chat-1', mode: 'chat' }));

    fireExit('term-1', 't1');
    emitWailsEvent('thread:updated', { action: 'deleted', id: 'term-1' });

    expect(getThreadById('term-1')).toBeUndefined();
    expect(getPane('p1')).toBeUndefined();
    expect(getPane('p2')).toBeUndefined();
    expect(getPane('p3')).toBe(other);
    expect(getExistingThreadTerminalState('term-1')).toBeNull();
  });
});

describe('a terminal thread its computer could not delete', () => {
  // The delete that ends a terminal thread has no caller, so its computer
  // tells every client, and each shows the sentence it sent.
  it('shows the computer\'s report as an error', () => {
    const message = 'The terminal "home" ended, but it could not be removed. Delete it from the sidebar to try again.';
    const before = getToasts().length;

    emitWailsEvent('terminal:end_failed', { threadID: 'term-1', message });

    const shown = getToasts().slice(before);
    for (const toast of shown) removeToast(toast.id);
    expect(shown.map(({ type, message: text }) => [type, text])).toEqual([['error', message]]);
  });
});

describe('a thread its computer deleted while this client was away', () => {
  // A restart ends every terminal thread before any client connects, so a
  // reconnecting client gets no deletion frame for it. The resync that
  // follows the reconnect asks for each shown thread the rows do not name.
  it('closes the panes showing it once its computer answers not_found', async () => {
    const gone = makeThread({ id: 'term-gone', mode: 'terminal' });
    const archived = makeThread({ id: 'chat-archived', mode: 'chat', archived: true });
    const unreachable = makeThread({ id: 'term-unreachable', mode: 'terminal' });
    const listed = makeThread({ id: 'term-listed', mode: 'terminal' });
    for (const thread of [gone, archived, unreachable, listed]) noteThread(thread.id, HOME_BACKEND);
    mountPaneShowingThread('p1', gone);
    const archivedPane = mountPaneShowingThread('p2', archived);
    const unreachablePane = mountPaneShowingThread('p3', unreachable);
    const listedPane = mountPaneShowingThread('p4', listed);
    setBindingMock('ListThreads', async () => [listed]);
    const getThread = setBindingMock('GetThread', async (id: string) => {
      if (id === 'term-gone') throw new TransportError('not_found', 'The requested item no longer exists.');
      if (id === 'term-unreachable') throw new TransportError('disconnected', 'Connection lost.');
      return archived;
    });

    emitWailsEvent(transportGapChannel, { channel: 'thread:updated', seq: 3 });

    await vi.waitFor(() => expect(getPane('p1')).toBeUndefined());
    expect(getThread.mock.calls.map(([id]) => id).sort()).toEqual(['chat-archived', 'term-gone', 'term-unreachable']);
    expect(getPane('p2')).toBe(archivedPane);
    expect(getPane('p3')).toBe(unreachablePane);
    expect(getPane('p4')).toBe(listedPane);
  });
});

// These assert the INTENT half: applyTerminalExit latches pane.requestTerminalFocus
// when (and only when) the user's focused active terminal exits with a sibling.
// The OUTCOME half — that a latched intent actually lands focus() on the
// remounted TerminalBody — is covered in TerminalSurface.focus.test.ts.
describe('applyTerminalExit — focus follows the active tab into a promoted sibling', () => {
  it('re-latches focus on a pane whose FOCUSED active terminal exits with a sibling', () => {
    const thread = makeThread({ id: 'term-1', mode: 'terminal' });
    prependThread(thread);
    const handle = getThreadTerminalState('term-1');
    handle.addTab(makeSummary('t1', 'term-1'));
    handle.addTab(makeSummary('t2', 'term-1'));
    handle.setActive('t1'); // t1 is the active (focused) tab about to exit
    const pane = mountPaneShowingThread('p1', thread, { focused: true });

    fireExit('term-1', 't1');

    // t1 removed, t2 promoted to active (a remount); because the user was
    // focused in the terminal, the cursor follows into the promoted sibling.
    expect(handle.tabs.map((tab) => tab.terminalID)).toEqual(['t2']);
    expect(handle.activeTerminalID).toBe('t2');
    expect(pane.consumeTerminalFocusRequest()).toBe(true);
  });

  it('does NOT steal focus when a BACKGROUNDED active terminal exits (composer focused)', () => {
    const thread = makeThread({ id: 'term-1', mode: 'terminal' });
    prependThread(thread);
    const handle = getThreadTerminalState('term-1');
    handle.addTab(makeSummary('t1', 'term-1'));
    handle.addTab(makeSummary('t2', 'term-1'));
    handle.setActive('t1');
    // Pane shows the thread but the terminal does NOT hold focus — the user is in
    // the composer / another pane when the backgrounded `sleep; exit` shell dies.
    const pane = mountPaneShowingThread('p1', thread);

    fireExit('term-1', 't1');

    expect(handle.activeTerminalID).toBe('t2'); // sibling still promoted
    expect(pane.consumeTerminalFocusRequest()).toBe(false); // but no focus steal
  });

  it('does NOT request focus when a NON-active tab exits (no remount, nothing moved)', () => {
    const thread = makeThread({ id: 'term-1', mode: 'terminal' });
    prependThread(thread);
    const handle = getThreadTerminalState('term-1');
    handle.addTab(makeSummary('t1', 'term-1'));
    handle.addTab(makeSummary('t2', 'term-1')); // addTab activates t2
    const pane = mountPaneShowingThread('p1', thread, { focused: true });

    fireExit('term-1', 't1'); // a background tab exits; active stays t2

    expect(handle.activeTerminalID).toBe('t2');
    expect(pane.consumeTerminalFocusRequest()).toBe(false);
  });

  it('does NOT request focus when the LAST tab exits (no sibling to focus)', () => {
    const thread = makeThread({ id: 'chat-1', mode: 'chat' });
    prependThread(thread);
    const handle = getThreadTerminalState('chat-1');
    handle.addTab(makeSummary('t1', 'chat-1')); // sole drawer terminal, active
    const pane = mountPaneShowingThread('p1', thread, { focused: true });

    fireExit('chat-1', 't1');

    expect(handle.tabs).toHaveLength(0);
    expect(pane.consumeTerminalFocusRequest()).toBe(false);
  });

  it('re-latches ONLY the focused pane when two panes show the same terminal thread', () => {
    // Split view: one terminal thread (one shared handle) shown in two panes.
    // The user is focused in pane A's terminal; pane B shows the same tabs but
    // isn't focused. When the active tab's shell exits, only A follows focus —
    // the plural panesShowingThread(...).filter(getTerminalFocused) must not
    // re-latch B and steal the cursor from wherever the user is in that pane.
    const thread = makeThread({ id: 'term-1', mode: 'terminal' });
    prependThread(thread);
    const handle = getThreadTerminalState('term-1');
    handle.addTab(makeSummary('t1', 'term-1'));
    handle.addTab(makeSummary('t2', 'term-1'));
    handle.setActive('t1');
    const focusedPane = mountPaneShowingThread('paneA', thread, { focused: true });
    const otherPane = mountPaneShowingThread('paneB', thread);

    fireExit('term-1', 't1');

    expect(handle.activeTerminalID).toBe('t2'); // both panes remount to t2
    expect(focusedPane.consumeTerminalFocusRequest()).toBe(true);
    expect(otherPane.consumeTerminalFocusRequest()).toBe(false);
  });
});
