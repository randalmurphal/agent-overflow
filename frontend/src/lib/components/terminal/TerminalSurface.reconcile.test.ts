import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render } from '@testing-library/svelte';
import TerminalSurface from './TerminalSurface.svelte';
import {
  getThreadTerminalState,
  notifyTerminalFocus,
  resetTerminalFocusForTest,
  resetThreadTerminalStatesForTest,
} from './terminalStore.svelte';
import type { ThreadTerminalSurfaceContext } from './terminalDrawerTypes';
import type { TerminalSessionSummary } from '../../types/terminal';
import { setupEventListeners } from '../../stores/events';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../../test/mocks/wailsio-runtime';
import { REMOTE_BACKEND_UUID, resetStagedBackends, stageBackend, type StagedBackend } from '../../../test/helpers/backends';
import { __resetEntityIndexForTest, noteTerminal, noteThread, terminalBackend } from '../../transport/entityIndex';
import { HOME_BACKEND } from '../../transport/backendKey';
import { transportGapChannel } from '../../transport/wsClient';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
import { createPane, resetPanesForTest } from '../../stores/panes.svelte';
import type { Thread } from '../../types/models';

// A mounted surface re-reads its thread's terminals once the thread's computer
// has replayed what this client missed, or has lost terminal frames, so a tab
// for a terminal that is gone does not outlive the reconnect.

vi.mock('./TerminalBody.svelte', async () => ({
  default: (await import('../../../test/mocks/StubTerminalBody.svelte')).default,
}));
vi.mock('../../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

function summary(terminalID: string, threadID: string): TerminalSessionSummary {
  return {
    terminalID, threadID, shell: '/bin/bash', cwd: '/tmp', rows: 24, cols: 80,
    pid: 1, startedAt: 0, running: true, exitCode: 0, exitReason: '',
  };
}

function surfaceFor(threadId: string, setVisible: (visible: boolean) => void = () => {}): ThreadTerminalSurfaceContext {
  return {
    paneId: `pane-${threadId}`,
    threadId,
    workspacePath: '/tmp',
    setVisible,
    acquireResizeLease: () => null,
    consumeFocusRequest: () => false,
  };
}

const ids = (threadId: string) => getThreadTerminalState(threadId).tabs.map((tab) => tab.terminalID);

let running: Record<string, string[]>;
let listTerminals: ReturnType<typeof setBindingMock>;
let laptop: StagedBackend;
let cleanupEvents: (() => void) | null = null;

async function mount(threadId: string, setVisible?: (visible: boolean) => void): Promise<void> {
  render(TerminalSurface, { surface: surfaceFor(threadId, setVisible) as never });
  await vi.waitFor(() => expect(ids(threadId)).toEqual(running[threadId]));
}

function fromLaptop(channel: string, data: unknown): void {
  emitWailsEvent(channel, data, REMOTE_BACKEND_UUID, { replayed: true });
}

beforeEach(() => {
  resetThreadTerminalStatesForTest();
  resetPanesForTest();
  resetTerminalFocusForTest();
  resetBindingMocks();
  __resetEntityIndexForTest();
  running = { 'thread-L': ['a', 'b'], 'thread-H': ['h'] };
  listTerminals = setBindingMock('ListTerminals', async (threadId: string) =>
    (running[threadId] ?? []).map((id) => summary(id, threadId)));
  laptop = stageBackend();
  noteThread('thread-L', 'laptop');
  noteThread('thread-H', HOME_BACKEND);
  cleanupEvents = setupEventListeners();
});

afterEach(() => {
  cleanup();
  cleanupEvents?.();
  cleanupEvents = null;
  resetStagedBackends();
  resetThreadTerminalStatesForTest();
  resetPanesForTest();
  resetTerminalFocusForTest();
  vi.mocked(reportFrontendDiagnostic).mockClear();
});

describe('terminal surface reconciliation', () => {
  it('drops a terminal that opened and exited while away, replayed exit first', async () => {
    await mount('thread-L');

    laptop.replay('start');
    fromLaptop('terminal:exit', { terminalID: 'c', threadID: 'thread-L', code: 0 });
    fromLaptop('terminal:opened', { terminalID: 'c', threadID: 'thread-L', summary: summary('c', 'thread-L') });
    expect(ids('thread-L')).toEqual(['a', 'b', 'c']);

    laptop.replay('complete');

    await vi.waitFor(() => expect(ids('thread-L')).toEqual(['a', 'b']));
    expect(terminalBackend('c')).toBeUndefined();
  });

  it('prunes a terminal whose exit a gap lost', async () => {
    await mount('thread-L');
    noteTerminal('a', 'laptop');
    noteTerminal('b', 'laptop');
    running['thread-L'] = ['a'];

    emitWailsEvent(transportGapChannel, { channel: 'terminal:exit', seq: 9 }, REMOTE_BACKEND_UUID);

    await vi.waitFor(() => expect(ids('thread-L')).toEqual(['a']));
    expect(terminalBackend('b')).toBeUndefined();
    expect(terminalBackend('a')).toBe('laptop');
  });

  // A terminal the re-read no longer names ended while this client was away,
  // and goes the way its exit would have (events.terminalExit.test.ts).
  it('moves focus to the next tab when the focused active terminal is gone', async () => {
    await mount('thread-L');
    const handle = getThreadTerminalState('thread-L');
    handle.setActive('b');
    const pane = createPane('pane-thread-L');
    pane.replaceThread({ id: 'thread-L', title: 'term', mode: 'terminal' } as Thread);
    notifyTerminalFocus('pane-thread-L', true);
    running['thread-L'] = ['a'];

    emitWailsEvent(transportGapChannel, { channel: 'terminal:exit', seq: 9 }, REMOTE_BACKEND_UUID);

    await vi.waitFor(() => expect(ids('thread-L')).toEqual(['a']));
    expect(handle.activeTerminalID).toBe('a');
    expect(pane.consumeTerminalFocusRequest()).toBe(true);
  });

  it('collapses the drawer and opens nothing when the re-read finds every terminal gone', async () => {
    const setVisible = vi.fn();
    await mount('thread-L', setVisible);
    const openTerminal = setBindingMock('OpenTerminal', async () => {
      throw new Error('unexpected open');
    });
    noteTerminal('a', 'laptop');
    running['thread-L'] = [];

    emitWailsEvent(transportGapChannel, { channel: 'terminal:exit', seq: 9 }, REMOTE_BACKEND_UUID);

    await vi.waitFor(() => expect(ids('thread-L')).toEqual([]));
    await vi.waitFor(() => expect(setVisible).toHaveBeenCalledWith(false));
    expect(openTerminal).not.toHaveBeenCalled();
    expect(terminalBackend('a')).toBeUndefined();
  });

  it('adds a terminal whose open a gap lost', async () => {
    await mount('thread-L');
    running['thread-L'] = ['a', 'b', 'd'];

    emitWailsEvent(transportGapChannel, { channel: 'terminal:opened', seq: 4 }, REMOTE_BACKEND_UUID);

    await vi.waitFor(() => expect(ids('thread-L')).toEqual(['a', 'b', 'd']));
    expect(getThreadTerminalState('thread-L').activeTerminalID).toBe('b');
  });

  it('keeps the tabs and reports the failure when the re-read fails', async () => {
    await mount('thread-L');
    listTerminals.mockImplementation(async () => { throw new Error('connection lost'); });

    emitWailsEvent(transportGapChannel, { channel: 'terminal:exit', seq: 9 }, REMOTE_BACKEND_UUID);

    await vi.waitFor(() => expect(reportFrontendDiagnostic).toHaveBeenCalledWith('terminal list failed', 'connection lost'));
    expect(ids('thread-L')).toEqual(['a', 'b']);
  });

  it('re-reads only the surfaces on the computer that reconnected', async () => {
    await mount('thread-L');
    await mount('thread-H');
    listTerminals.mockClear();

    laptop.replay('start');
    laptop.replay('complete');

    await vi.waitFor(() => expect(listTerminals).toHaveBeenCalled());
    expect(listTerminals.mock.calls.map(([threadId]) => threadId)).toEqual(['thread-L']);
  });

  it('leaves a gap inside a replay to the replay\'s completion', async () => {
    await mount('thread-L');
    listTerminals.mockClear();

    laptop.replay('start');
    emitWailsEvent(transportGapChannel, { channel: 'terminal:exit', seq: 9 }, REMOTE_BACKEND_UUID);
    await Promise.resolve();
    expect(listTerminals).not.toHaveBeenCalled();

    laptop.replay('complete');
    await vi.waitFor(() => expect(listTerminals).toHaveBeenCalledTimes(1));
  });

  it('asks nothing when no surface is mounted', async () => {
    await mount('thread-L');
    cleanup();
    listTerminals.mockClear();

    laptop.replay('start');
    laptop.replay('complete');
    emitWailsEvent(transportGapChannel, { channel: 'terminal:exit', seq: 9 }, REMOTE_BACKEND_UUID);
    await Promise.resolve();

    expect(listTerminals).not.toHaveBeenCalled();
  });
});
