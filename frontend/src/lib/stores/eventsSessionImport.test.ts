import { __setTransportStatusForTest } from './transportStatus.svelte';
// session-import:progress ingestion. A frame that reaches the store drives a
// progress bar, closes the modal and toasts "imported N sessions" — and the
// channel reaches any client granted `threads:operate`, this device or
// another — so this handler is the place a malformed frame has to die,
// whole. Every case below asserts BOTH halves: the frame is rejected, and
// the run state is untouched.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { emitWailsEvent } from '../../test/mocks/wailsio-runtime';
import type { TransportStatusSnapshot } from '../transport/wsClient';
import { applySessionImportProgress, setupSessionImportEvents } from './eventsSessionImport';
import { applyTransportGap } from './eventsTransportGap';
import {
  getSessionImportRun,
  isSessionImportOpen as isRunSurfaceOpen,
  openSessionImport,
  resetSessionImportForTest,
  startImport,
} from './sessionImport.svelte';
import { getToasts, removeToast } from './toast.svelte';

function frame(extra: Record<string, unknown> = {}): Record<string, unknown> {
  return { importId: 'run-1', completed: 1, total: 2, ...extra };
}

async function startRun(): Promise<void> {
  setBindingMock('ImportSessions', (async () => ({ importId: 'run-1', total: 2 })) as never);
  openSessionImport();
  await startImport(['claude:a', 'codex:b']);
}

/**
 * Capture the status handler `setupSessionImportEvents` registers so a test
 * can drive transport transitions without a socket. Returns a setter that
 * plays a snapshot through the wiring.
 */
function driveTransportStatus(initial: TransportStatusSnapshot['status']) {
  __setTransportStatusForTest({ status: initial, nextAttemptAt: null });
  return (status: TransportStatusSnapshot['status']) => {
    __setTransportStatusForTest({ status, nextAttemptAt: null });
  };
}

function toastMessages(): string[] {
  return getToasts().map((t) => t.message);
}

beforeEach(() => {
  resetSessionImportForTest();
  for (const toast of [...getToasts()]) removeToast(toast.id);
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListProjects', async () => []);
});

afterEach(() => {
  resetSessionImportForTest();
  for (const toast of [...getToasts()]) removeToast(toast.id);
  vi.restoreAllMocks();
});

describe('frame validation', () => {
  it('accepts a well-formed per-row frame and folds it', async () => {
    await startRun();

    expect(applySessionImportProgress(frame({ id: 'claude:a', status: 'imported', threadIds: ['t1'] })))
      .toBe(true);
    expect(getSessionImportRun()?.results.get('claude:a')).toMatchObject({
      status: 'imported',
      threadIds: ['t1'],
    });
  });

  it('accepts the terminal frame', async () => {
    await startRun();
    expect(applySessionImportProgress(frame({ completed: 2, done: true }))).toBe(true);
    // A clean full run settles itself: the store closes the surface and
    // drops the run, so "no run left" is what acceptance looks like here.
    expect(getSessionImportRun()).toBeNull();
  });

  it.each([
    ['a non-object payload', 'nope' as unknown],
    ['no payload at all', null],
    ['a missing import id', frame({ importId: undefined })],
    ['a non-string import id', frame({ importId: 7 })],
    ['a non-numeric completed', frame({ completed: '1' })],
    ['a non-finite total', frame({ total: Number.NaN })],
    ['a negative counter', frame({ completed: -1 })],
    ['a non-boolean done', frame({ done: 'yes' })],
    ['an id with no status', frame({ id: 'claude:a' })],
    ['a status with no id', frame({ status: 'imported' })],
    ['an unknown status', frame({ id: 'claude:a', status: 'exploded' })],
    ['a non-string error', frame({ id: 'claude:a', status: 'failed', error: { msg: 'x' } })],
    ['a non-array threadIds', frame({ id: 'claude:a', status: 'imported', threadIds: 't1' })],
    ['a threadIds entry that is not a string', frame({ id: 'claude:a', status: 'imported', threadIds: [3] })],
  ])('rejects %s without touching the run', async (_label, payload) => {
    await startRun();
    const before = getSessionImportRun();

    expect(applySessionImportProgress(payload)).toBe(false);

    const after = getSessionImportRun();
    expect(after?.results.size).toBe(0);
    expect(after?.completed).toBe(before?.completed);
    expect(after?.active).toBe(true);
  });

  it('rejects prose longer than the cap rather than stamping it onto a row', async () => {
    await startRun();
    const payload = frame({ id: 'claude:a', status: 'failed', error: 'x'.repeat(4_001) });
    expect(applySessionImportProgress(payload)).toBe(false);
    expect(getSessionImportRun()?.results.size).toBe(0);
  });

  it('forwards only the contract fields, never extras the frame smuggled in', async () => {
    await startRun();
    applySessionImportProgress(frame({ id: 'claude:a', status: 'imported', rogue: 'value' }));

    const result = getSessionImportRun()?.results.get('claude:a');
    expect(result).toEqual({ id: 'claude:a', status: 'imported', threadIds: [], error: '' });
  });

  it('treats an omitted threadIds as none rather than rejecting the frame', async () => {
    await startRun();
    expect(applySessionImportProgress(frame({ id: 'claude:a', status: 'skipped', error: 'gone' })))
      .toBe(true);
    expect(getSessionImportRun()?.results.get('claude:a')).toMatchObject({
      status: 'skipped',
      error: 'gone',
      threadIds: [],
    });
  });
});

describe('channel wiring', () => {
  it('routes the channel into the store and stops on teardown', async () => {
    await startRun();
    const teardown = setupSessionImportEvents();

    emitWailsEvent('session-import:progress', frame({ id: 'claude:a', status: 'imported' }));
    expect(getSessionImportRun()?.results.size).toBe(1);

    teardown();
    emitWailsEvent('session-import:progress', frame({ completed: 2, id: 'codex:b', status: 'imported' }));
    expect(getSessionImportRun()?.results.size).toBe(1);
  });

  it('logs a dropped frame instead of letting the bar stall silently', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    await startRun();
    const teardown = setupSessionImportEvents();

    emitWailsEvent('session-import:progress', { importId: 'run-1', completed: 'nope' });

    expect(warn).toHaveBeenCalled();
    teardown();
  });
});

// The two ways a run's frame stream can be interrupted, and why only one of
// them ends the run. A socket drop is recoverable — the transport replays
// the channel from the server's ring on reconnect — so ending the run there
// would discard the very frames that finish it. A GAP is the transport
// saying the ring could not cover this client: those frames are gone.
describe('an interrupted frame stream', () => {
  it('keeps the run alive across a drop, and finishes it on the replayed frames', async () => {
    const info = vi.spyOn(console, 'info').mockImplementation(() => {});
    const setStatus = driveTransportStatus('connected');
    openSessionImport();
    await startRun();
    const teardown = setupSessionImportEvents();

    setStatus('reconnecting');

    // Still running: the backend's run does not care about this client's
    // socket, and the frames it emitted meanwhile are in the replay ring.
    expect(getSessionImportRun()).toMatchObject({ active: true, connectionLost: false });
    // …but the drop is on the record, so a stalled bar has an explanation.
    expect(info).toHaveBeenCalled();

    setStatus('connected');
    emitWailsEvent(
      'session-import:progress',
      frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }),
    );
    emitWailsEvent(
      'session-import:progress',
      frame({ completed: 2, id: 'codex:b', status: 'imported', threadIds: ['t2'] }),
    );
    emitWailsEvent('session-import:progress', frame({ completed: 2, done: true }));

    // The replayed terminal frame settles the run exactly as a live one does.
    expect(getSessionImportRun()).toBeNull();
    expect(isRunSurfaceOpen()).toBe(false);
    expect(toastMessages()).toEqual(['Imported 2 sessions (2 threads).']);
    teardown();
  });

  it('leaves a run with no frames in flight untouched by a drop', async () => {
    const setStatus = driveTransportStatus('connected');
    const teardown = setupSessionImportEvents();
    setStatus('disconnected');
    expect(getSessionImportRun()).toBeNull();
    teardown();
  });

  it('ends the run on a transport GAP — the proof frames were lost', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    openSessionImport();
    await startRun();
    applySessionImportProgress(
      frame({ completed: 1, id: 'claude:a', status: 'imported', threadIds: ['t1'] }),
    );

    applyTransportGap({ channel: 'session-import:progress', seq: 42 });

    expect(getSessionImportRun()).toMatchObject({ active: false, connectionLost: true });
    // Not a success: nothing closes itself and nothing claims an import.
    expect(isRunSurfaceOpen()).toBe(true);
    expect(toastMessages()).toEqual([]);
    // Handled, so it never reaches the unknown-channel fallback (which would
    // fan a refresh out over every pane mid-import).
    expect(warn).not.toHaveBeenCalledWith(expect.stringContaining('unknown channel'));
  });

  it('ignores a gap on the channel when no run is in flight', () => {
    applyTransportGap({ channel: 'session-import:progress', seq: 7 });
    expect(getSessionImportRun()).toBeNull();
  });
});
